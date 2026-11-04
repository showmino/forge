package cli

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newApprovalCmd(f *Factory) *cobra.Command {
	cmd := group("approval", "Approve the production runs of infrastructure repos", "approvals")
	cmd.Long = `An infrastructure repo's production run waits for an approval in Forge: the
shared workflow asks for one ('forge approval request'), a member of the team
that owns the repo approves or rejects it, and on approval Forge starts the
repo's production run, which claims the approval ('forge approval claim')
before it signs in to the cloud. The person who started the run cannot
approve it, unless the platform team gave the repo an exception.`
	cmd.AddCommand(newApprovalListCmd(f), newApprovalGetCmd(f), newApprovalDecideCmd(f, true), newApprovalDecideCmd(f, false),
		newApprovalRequestCmd(f), newApprovalClaimCmd(f), newApprovalExceptionCmd(f))
	return cmd
}

func newApprovalExceptionCmd(f *Factory) *cobra.Command {
	cmd := group("exception", "Let a team of one approve its own production runs", "exceptions")
	cmd.Long = `A team with one member cannot get a second approver. The platform team can
let such a team approve its own production runs of one infrastructure repo:
'forge approval exception request' opens a pull request in the deploy repo,
and the exception holds once it is merged with an approval by a member of an
admin team (Forge checks that on GitHub, not just the file).`
	cmd.AddCommand(newExceptionRequestCmd(f, true), newExceptionRequestCmd(f, false), newExceptionListCmd(f))
	return cmd
}

// newExceptionRequestCmd is 'request' when request, else 'revoke'.
func newExceptionRequestCmd(f *Factory, request bool) *cobra.Command {
	var reason string
	verb, short, example := "revoke", "Open the pull request that removes an exception", "  forge approval exception revoke infra-network --reason 'Bo joined the team'"
	if request {
		verb, short, example = "request", "Ask the platform team for an exception", "  forge approval exception request infra-network --reason 'I am the only one on the team'"
	}
	cmd := &cobra.Command{
		Use:     verb + " <service>",
		Short:   short,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			if request && strings.TrimSpace(reason) == "" {
				return exitcode.Usagef("--reason is required: say why the team needs to approve its own runs")
			}
			svc := "services/" + strings.TrimPrefix(args[0], "services/")
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			var url string
			if request {
				resp, err := c.Approvals.RequestException(ctx, connect.NewRequest(&forgev1.RequestExceptionRequest{Service: svc, Reason: reason}))
				if err != nil {
					return err
				}
				url = resp.Msg.GetPullRequestUrl()
			} else {
				resp, err := c.Approvals.RevokeException(ctx, connect.NewRequest(&forgev1.RevokeExceptionRequest{Service: svc, Reason: reason}))
				if err != nil {
					return err
				}
				url = resp.Msg.GetPullRequestUrl()
			}
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{url}, Data: map[string]any{"pullRequestUrl": url}})
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s\nThe platform team reviews it; the change holds once it is merged.\n", url)
			return err
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the platform team")
	return cmd
}

func newExceptionListCmd(f *Factory) *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the exceptions, and whether each holds",
		Example: "  forge approval exception list\n  forge approval exception list --service infra-network -o json",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			req := &forgev1.ListExceptionsRequest{}
			if service != "" {
				req.Service = "services/" + strings.TrimPrefix(service, "services/")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Approvals.ListExceptions(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			exs := resp.Msg.GetExceptions()
			if len(exs) == 0 && format == output.Table {
				f.infof("No exceptions.")
				return nil
			}
			v := output.View{Headers: []string{"service", "team", "holds", "granted by", "reason"}, Data: resp.Msg}
			if len(exs) == 0 {
				v.Data = map[string]any{"exceptions": []any{}}
			}
			for _, e := range exs {
				holds := "yes"
				if !e.GetGranted() {
					holds = "no: " + e.GetProblem()
				}
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(e.GetService(), "services/"), e.GetTeam(), holds, e.GetGrantedBy(), e.GetReason()})
				v.Names = append(v.Names, e.GetService())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only this service's exception")
	return cmd
}

// approvalName turns "apr_x" or "approvals/apr_x" into "approvals/apr_x".
func approvalName(s string) (string, error) {
	id := strings.TrimPrefix(s, "approvals/")
	if !strings.HasPrefix(id, "apr_") || len(id) < 5 || strings.Contains(id, "/") {
		return "", exitcode.Usagef("%q is not an approval (apr_...)", s).WithHint("see 'forge approval list'")
	}
	return "approvals/" + id, nil
}

var approvalStateNames = map[forgev1.Approval_State]string{
	forgev1.Approval_STATE_PENDING:    "pending",
	forgev1.Approval_STATE_APPROVED:   "approved",
	forgev1.Approval_STATE_REJECTED:   "rejected",
	forgev1.Approval_STATE_SUPERSEDED: "superseded",
	forgev1.Approval_STATE_CLAIMED:    "claimed",
}

func approvalStateName(s forgev1.Approval_State) string {
	if n, ok := approvalStateNames[s]; ok {
		return n
	}
	return "unknown"
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func newApprovalListCmd(f *Factory) *cobra.Command {
	var service, state string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List approvals, newest first",
		Example: `  forge approval list --state pending
  forge approval list --service infra-network -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			req := &forgev1.ListApprovalsRequest{PageSize: int32(min(max(limit, 0), 200))} //nolint:gosec // capped above
			if service != "" {
				req.Service = "services/" + strings.TrimPrefix(service, "services/")
			}
			if state != "" {
				found := false
				for s, n := range approvalStateNames {
					if n == state {
						req.State, found = s, true
					}
				}
				if !found {
					return exitcode.Usagef("--state %q must be pending, approved, rejected, superseded or claimed", state)
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Approvals.ListApprovals(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			aps := resp.Msg.GetApprovals()
			if len(aps) == 0 && format == output.Table {
				f.infof("No approvals found.")
				return nil
			}
			v := output.View{Headers: []string{"approval", "service", "commit", "state", "asked by", "decided by", "asked"}, Data: resp.Msg}
			if len(aps) == 0 {
				v.Data = map[string]any{"approvals": []any{}}
			}
			for _, a := range aps {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(a.GetName(), "approvals/"), strings.TrimPrefix(a.GetService(), "services/"),
					shortCommit(a.GetCommit()), approvalStateName(a.GetState()), a.GetRequestedBy(), a.GetDecidedBy(),
					a.GetCreateTime().AsTime().Local().Format("2006-01-02 15:04")})
				v.Names = append(v.Names, a.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only this service's approvals")
	cmd.Flags().StringVar(&state, "state", "", "only approvals in this state: pending, approved, rejected, superseded or claimed")
	cmd.Flags().IntVar(&limit, "limit", 50, "at most this many (up to 200)")
	return cmd
}

// printApproval shows one approval: as a table of fields, or the approval.
func (f *Factory) printApproval(format output.Format, a *forgev1.Approval) error {
	ts := func(t interface{ AsTime() time.Time }, set bool) string {
		if !set {
			return ""
		}
		return t.AsTime().Local().Format("2006-01-02 15:04")
	}
	rows := [][]string{
		{"approval", a.GetName()},
		{"service", a.GetService()},
		{"environment", a.GetEnvironment()},
		{"commit", a.GetCommit()},
		{"state", approvalStateName(a.GetState())},
		{"asked by", a.GetRequestedBy()},
		{"run", a.GetRunUrl()},
		{"approvers", "team " + a.GetOwnerTeam()},
		{"decided by", a.GetDecidedBy()},
		{"reason", a.GetReason()},
		{"asked", ts(a.GetCreateTime(), a.GetCreateTime() != nil)},
		{"decided", ts(a.GetDecideTime(), a.GetDecideTime() != nil)},
		{"claimed", ts(a.GetClaimTime(), a.GetClaimTime() != nil)},
	}
	if e := a.GetDispatchError(); e != "" {
		rows = append(rows, []string{"run not started", e + " (approve again to retry)"})
	}
	return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows, Names: []string{a.GetName()}, Data: a})
}

func newApprovalGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <approval>",
		Short:   "Show one approval",
		Example: "  forge approval get apr_3950f08e3db4a99f",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, err := approvalName(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Approvals.GetApproval(ctx, connect.NewRequest(&forgev1.GetApprovalRequest{Name: name}))
			if err != nil {
				return err
			}
			return f.printApproval(format, resp.Msg.GetApproval())
		},
	}
}

// newApprovalDecideCmd is 'approve' when approve, else 'reject'.
func newApprovalDecideCmd(f *Factory, approve bool) *cobra.Command {
	var reason string
	verb, short, example := "reject", "Reject a production run", "  forge approval reject apr_3950f08e3db4a99f --reason 'deletes the subnet'"
	if approve {
		verb, short, example = "approve", "Approve a production run, and start it", "  forge approval approve apr_3950f08e3db4a99f --reason 'plan reviewed'"
	}
	cmd := &cobra.Command{
		Use:     verb + " <approval>",
		Short:   short,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, err := approvalName(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			var a *forgev1.Approval
			if approve {
				resp, err := c.Approvals.ApproveApproval(ctx, connect.NewRequest(&forgev1.ApproveApprovalRequest{Name: name, Reason: reason}))
				if err != nil {
					return err
				}
				a = resp.Msg.GetApproval()
			} else {
				resp, err := c.Approvals.RejectApproval(ctx, connect.NewRequest(&forgev1.RejectApprovalRequest{Name: name, Reason: reason}))
				if err != nil {
					return err
				}
				a = resp.Msg.GetApproval()
			}
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{a.GetName()}, Data: a})
			}
			if e := a.GetDispatchError(); e != "" {
				return exitcode.New(exitcode.OperationFailed, "%s is approved, but its production run could not be started: %s", a.GetName(), e).
					WithHint("run 'forge approval approve " + strings.TrimPrefix(a.GetName(), "approvals/") + "' again to retry")
			}
			msg := fmt.Sprintf("%s %s", a.GetName(), approvalStateName(a.GetState()))
			if approve {
				msg += fmt.Sprintf(": the production run of %s at %s is starting (%s)", strings.TrimPrefix(a.GetService(), "services/"), shortCommit(a.GetCommit()), a.GetRunUrl())
			}
			_, err = fmt.Fprintln(f.IO.Out, msg)
			return err
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the audit log")
	return cmd
}

func newApprovalRequestCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "request",
		Short: "Ask for production's approval (in the shared workflow)",
		Long: `Ask for the approval of this job's commit in production. Only a job of the
shared workflow of an infrastructure repo may: it runs with the job's GitHub
Actions token. -o name prints the approval's name.`,
		Example: "  forge approval request -o name",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Approvals.RequestApproval(ctx, connect.NewRequest(&forgev1.RequestApprovalRequest{}))
			if err != nil {
				return err
			}
			a := resp.Msg.GetApproval()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{a.GetName()}, Data: a})
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s asks team %s to approve %s in production: forge approval approve %s\n",
				a.GetName(), a.GetOwnerTeam(), shortCommit(a.GetCommit()), strings.TrimPrefix(a.GetName(), "approvals/"))
			return err
		},
	}
}

func newApprovalClaimCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "claim <approval>",
		Short: "Claim an approved production run (in the shared workflow)",
		Long: `Claim an approved approval before deploying to production: it must be
approved, of this repo, and not claimed before. Only a job of the shared
workflow may, with its GitHub Actions token. It prints the commit to deploy;
-o json prints the approval.`,
		Example: "  commit=$(forge approval claim \"$APPROVAL\")",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, err := approvalName(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Approvals.ClaimApproval(ctx, connect.NewRequest(&forgev1.ClaimApprovalRequest{Name: name}))
			if err != nil {
				return err
			}
			a := resp.Msg.GetApproval()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{a.GetName()}, Data: a})
			}
			_, err = fmt.Fprintln(f.IO.Out, a.GetCommit())
			return err
		},
	}
}
