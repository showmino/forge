package cli

import (
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/output"
)

func newTeamCmd(f *Factory) *cobra.Command {
	cmd := group("team", "List the teams a service can belong to", "teams")
	cmd.Long = `A service belongs to a team of the GitHub organization. These are the teams
'forge create --team' and the dashboard choose from.`
	cmd.AddCommand(newTeamListCmd(f))
	return cmd
}

func newTeamListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the organization's teams",
		Long:    "List the teams of the GitHub organization, read from GitHub by the Forge API.",
		Example: `  forge team list
  forge team list -o json | jq -r '.teams[].name'`,
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
			resp, err := c.Teams.ListTeams(ctx, connect.NewRequest(&forgev1.ListTeamsRequest{}))
			if err != nil {
				return err
			}
			ts := resp.Msg.GetTeams()
			if len(ts) == 0 && format == output.Table {
				f.infof("No teams found.")
				return nil
			}
			v := output.View{Headers: []string{"team", "name", "description"}, Data: resp.Msg}
			if len(ts) == 0 {
				// protojson omits empty lists; keep '.teams[]' working in jq.
				v.Data = map[string]any{"teams": []any{}}
			}
			for _, t := range ts {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(t.GetName(), "teams/"), t.GetDisplayName(), t.GetDescription()})
				v.Names = append(v.Names, t.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}
