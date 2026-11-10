package cli

import (
	"fmt"
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/output"
)

func newAuditAdoptionCmd(f *Factory) *cobra.Command {
	var weekly bool
	return withWeekly(&cobra.Command{
		Use:   "adoption",
		Short: "Show how much Forge is used",
		Long: `Show how much Forge is used, from the audit log: the people who changed
something, the deploys (by people and by CI), and the share of changes made by
people outside the admin groups, for the last 30 days and the 30 before.
--weekly shows the last 12 weeks instead.

The share counts only changes whose audit event records the person's role,
which Forge does since 0.54.0.`,
		Example: `  forge audit adoption
  forge audit adoption --weekly
  forge audit adoption -o json | jq '.last'`,
		Args: noArgs,
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
			resp, err := c.Audit.GetAdoption(ctx, connect.NewRequest(&forgev1.GetAdoptionRequest{}))
			if err != nil {
				return err
			}
			m := resp.Msg
			if weekly {
				v := output.View{Headers: []string{"week", "deploys", "self-served", "people"}, Data: m}
				for _, w := range m.GetWeeks() {
					start := w.GetStart().AsTime().Format("2006-01-02")
					v.Rows = append(v.Rows, []string{start, strconv.Itoa(int(w.GetDeploys())), strconv.Itoa(int(w.GetSelfServed())), strconv.Itoa(int(w.GetActiveUsers()))})
					v.Names = append(v.Names, start)
				}
				return output.Print(f.IO.Out, format, v)
			}
			v := output.View{Headers: []string{"", "last 30 days", "30 days before"}, Data: m}
			last, prev := m.GetLast(), m.GetPrevious()
			v.Rows = [][]string{
				{"people", strconv.Itoa(int(last.GetActiveUsers())), strconv.Itoa(int(prev.GetActiveUsers()))},
				{"deploys", strconv.Itoa(int(last.GetDeploys())), strconv.Itoa(int(prev.GetDeploys()))},
				{"self-served", share(last), share(prev)},
			}
			v.Names = []string{"people", "deploys", "self-served"}
			if err := output.Print(f.IO.Out, format, v); err != nil {
				return err
			}
			if format == output.Table {
				f.infof("Counted from %s.", m.GetSince().AsTime().Local().Format("2006-01-02"))
			}
			return nil
		},
	}, &weekly)
}

func withWeekly(cmd *cobra.Command, weekly *bool) *cobra.Command {
	cmd.Flags().BoolVar(weekly, "weekly", false, "the last 12 weeks, one row each")
	return cmd
}

// share says the self-served changes of a period: "67% (2 of 3)", or "–"
// when no change records a role.
func share(p *forgev1.AdoptionPeriod) string {
	if p.GetWrites() == 0 {
		return "–"
	}
	return fmt.Sprintf("%d%% (%d of %d)", 100*p.GetSelfServed()/p.GetWrites(), p.GetSelfServed(), p.GetWrites())
}
