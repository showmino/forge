package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
)

func newServiceChecksCmd(f *Factory) *cobra.Command {
	var missing bool
	cmd := &cobra.Command{
		Use:   "checks [name]",
		Short: "Show how far services are on the golden path",
		Long: `Show the golden-path checks of every service, or of one: a CI pipeline, an
SLO target, tracing with OpenTelemetry, a runbook (docs/runbook.md) and an
on-call rotation for its team. Forge works them out from the catalog, the
audit log, the teams' settings and the repo; what it reads from the repo is
at most a few minutes old.

A check that does not apply (a runbook for a service without a repo) counts
neither way.`,
		Example: `  forge service checks
  forge service checks --missing
  forge service checks payments
  forge service checks -o json | jq -r '.services[] | select(.passed < .applies) | .service'`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			req := &forgev1.ListServiceChecksRequest{}
			if len(args) == 1 {
				id := strings.TrimPrefix(args[0], "services/")
				if !manifest.ValidID(id) {
					return exitcode.Usagef("%q is not a valid service name (lowercase letters, digits and '-')", id)
				}
				req.Service = "services/" + id
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Catalog.ListServiceChecks(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			all := resp.Msg.GetServices()
			if missing {
				kept := all[:0]
				for _, s := range all {
					if s.GetPassed() < s.GetApplies() {
						kept = append(kept, s)
					}
				}
				all = kept
			}
			if req.Service != "" && len(all) == 1 && format == output.Table {
				v := output.View{Headers: []string{"check", "status", "detail"}, Data: resp.Msg}
				for _, ch := range all[0].GetChecks() {
					v.Rows = append(v.Rows, []string{ch.GetName(), checkWord(ch), ch.GetDetail()})
					v.Names = append(v.Names, ch.GetName())
				}
				return output.Print(f.IO.Out, format, v)
			}
			if len(all) == 0 && format == output.Table {
				if missing {
					f.infof("Every service passes every check that applies to it.")
				} else {
					f.infof("No services found.")
				}
				return nil
			}
			v := output.View{Headers: []string{"service", "passed", "missing"}, Data: &forgev1.ListServiceChecksResponse{Services: all}}
			for _, s := range all {
				var miss []string
				for _, ch := range s.GetChecks() {
					if ch.GetApplies() && !ch.GetPassed() {
						miss = append(miss, ch.GetName())
					}
				}
				v.Rows = append(v.Rows, []string{shortName(s.GetService()), fmt.Sprintf("%d/%d", s.GetPassed(), s.GetApplies()), strings.Join(miss, ", ")})
				v.Names = append(v.Names, s.GetService())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().BoolVar(&missing, "missing", false, "only the services that miss a check")
	return cmd
}

// checkWord says how a check came out: passed, missing or not applicable.
func checkWord(ch *forgev1.GoldenCheck) string {
	switch {
	case !ch.GetApplies():
		return "n/a"
	case ch.GetPassed():
		return "passed"
	default:
		return "missing"
	}
}
