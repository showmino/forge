package cli

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newAuditCmd(f *Factory) *cobra.Command {
	cmd := group("audit", "Read the audit log", "audits")
	cmd.Long = `The audit log has one event per write call to the Forge API, refused calls
included, plus the outcome of long-running work (rows marked ↳) and forge-api's
own actions (actor "system"). Every member can read it; nothing can change it.`
	cmd.AddCommand(newAuditListCmd(f), newAuditGetCmd(f))
	return cmd
}

func newAuditListCmd(f *Factory) *cobra.Command {
	var service, actor, action, since, until string
	var failed bool
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List audit events, newest first",
		Example: `  forge audit list --service payments --since 7d
  forge audit list --actor ana@example.com --failed
  forge audit list --action deploy.promote -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			if limit < 1 {
				return exitcode.Usagef("--limit must be at least 1")
			}
			req := &forgev1.ListAuditEventsRequest{Service: service, Actor: actor, Action: action, FailedOnly: failed}
			now := f.Now()
			if req.Since, err = auditTime(since, now, "--since"); err != nil {
				return err
			}
			if req.Until, err = auditTime(until, now, "--until"); err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			var events []*forgev1.AuditEvent
			for len(events) < limit {
				req.PageSize = int32(min(limit-len(events), 500)) //nolint:gosec // bounded to 1..500
				resp, err := c.Audit.ListAuditEvents(ctx, connect.NewRequest(req))
				if err != nil {
					return err
				}
				events = append(events, resp.Msg.GetEvents()...)
				if req.PageToken = resp.Msg.GetNextPageToken(); req.PageToken == "" {
					break
				}
			}
			if len(events) == 0 && format == output.Table {
				f.infof("No audit events.")
				return nil
			}
			v := output.View{
				Headers: []string{"id", "time", "actor", "action", "service", "target", "result"},
				Data:    &forgev1.ListAuditEventsResponse{Events: events},
			}
			if len(events) == 0 {
				v.Data = map[string]any{"events": []any{}} // keep '.events[]' working in jq
			}
			for _, e := range events {
				act := e.GetAction()
				if e.GetKind() == "outcome" {
					act = "↳ " + act
				}
				v.Rows = append(v.Rows, []string{
					auditID(e.GetName()), e.GetTime().AsTime().UTC().Format("2006-01-02 15:04:05"), auditActor(e.GetActor()),
					act, strings.TrimPrefix(e.GetService(), "services/"), e.GetTarget(), auditResult(e),
				})
				v.Names = append(v.Names, e.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&service, "service", "", "only events about this service")
	fl.StringVar(&actor, "actor", "", "only events by this actor: an email, a repository <org>/<repo>, system or anonymous")
	fl.StringVar(&action, "action", "", "only this action, for example deploy.promote")
	fl.BoolVar(&failed, "failed", false, "only events whose result is not ok")
	fl.StringVar(&since, "since", "", "only events at or after this: a duration back from now (24h, 7d) or an RFC 3339 time")
	fl.StringVar(&until, "until", "", "only events before this: a duration back from now or an RFC 3339 time")
	fl.IntVar(&limit, "limit", 50, "most events to show")
	return cmd
}

func newAuditGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <id>",
		Short:   "Show one audit event",
		Example: "  forge audit get 1234\n  forge audit get auditEvents/1234 -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			id := strings.TrimPrefix(args[0], "auditEvents/")
			if n, err := strconv.ParseInt(id, 10, 64); err != nil || n < 1 {
				return exitcode.Usagef("%q is not an audit event id", args[0]).WithHint("see 'forge audit list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Audit.GetAuditEvent(ctx, connect.NewRequest(&forgev1.GetAuditEventRequest{Name: "auditEvents/" + id}))
			if err != nil {
				return err
			}
			e := resp.Msg.GetEvent()
			a := e.GetActor()
			var det []string
			for _, k := range slices.Sorted(maps.Keys(e.GetDetails())) {
				det = append(det, k+"="+e.GetDetails()[k])
			}
			rows := [][]string{
				{"name", e.GetName()},
				{"time", e.GetTime().AsTime().UTC().Format(time.RFC3339)},
				{"kind", e.GetKind()},
				{"action", e.GetAction()},
				{"actor", auditActor(a)},
				{"actor kind", a.GetKind()},
				{"subject", a.GetSubject()},
				{"service", e.GetService()},
				{"target", e.GetTarget()},
				{"result", e.GetResult().GetCode()},
				{"message", e.GetResult().GetMessage()},
				{"operation", e.GetOperation()},
				{"details", strings.Join(det, ", ")},
				{"client", e.GetClient().GetVersion()},
				{"request id", e.GetClient().GetRequestId()},
			}
			return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows, Names: []string{e.GetName()}, Data: e})
		},
	}
}

// auditTime reads a --since/--until value: "" (none), a duration back from
// now ("24h", "7d"), or an RFC 3339 time.
func auditTime(s string, now time.Time, flag string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return timestamppb.New(now.AddDate(0, 0, -n)), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return timestamppb.New(now.Add(-d)), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t), nil
	}
	return nil, exitcode.Usagef("%s %q must be a duration such as 24h or 7d, or an RFC 3339 time", flag, s)
}

// auditID is the number of "auditEvents/<id>".
func auditID(name string) string { return strings.TrimPrefix(name, "auditEvents/") }

// auditActor is who acted, as one word: an email, a repository, or the kind.
func auditActor(a *forgev1.AuditActor) string {
	switch {
	case a.GetEmail() != "":
		return a.GetEmail()
	case a.GetRepository() != "":
		return a.GetRepository()
	case a.GetSubject() != "":
		return a.GetSubject()
	default:
		return a.GetKind()
	}
}

// auditResult is the RESULT cell: the code, marked when the call was a dry run.
func auditResult(e *forgev1.AuditEvent) string {
	code := e.GetResult().GetCode()
	if e.GetDetails()["validate_only"] == "true" {
		return code + " (dry run)"
	}
	return code
}
