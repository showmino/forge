package cli

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newTeamCmd(f *Factory) *cobra.Command {
	cmd := group("team", "List the teams a service can belong to, and set their on-call", "teams")
	cmd.Long = `A service belongs to a team of the GitHub organization. These are the teams
'forge create --team' and the dashboard choose from. Forge keeps one thing of
its own about a team: the name of its on-call rotation.`
	cmd.AddCommand(newTeamListCmd(f), newTeamUpdateCmd(f))
	return cmd
}

func newTeamListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the organization's teams",
		Long:    "List the teams of the GitHub organization, read from GitHub by the Forge API, with how many members each has and its on-call rotation.",
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
			v := output.View{Headers: []string{"team", "name", "members", "oncall", "description"}, Data: resp.Msg}
			if len(ts) == 0 {
				// protojson omits empty lists; keep '.teams[]' working in jq.
				v.Data = map[string]any{"teams": []any{}}
			}
			for _, t := range ts {
				members := ""
				if t.GetMemberCount() > 0 {
					members = strconv.Itoa(int(t.GetMemberCount()))
				}
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(t.GetName(), "teams/"), t.GetDisplayName(), members, t.GetOncall(), t.GetDescription()})
				v.Names = append(v.Names, t.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newTeamUpdateCmd(f *Factory) *cobra.Command {
	var oncall string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "update <team> --oncall <rotation>",
		Short: "Set a team's on-call rotation",
		Long: `Set the name of a team's on-call rotation, as your paging tool calls it.
An empty name removes it. Admins only: the team itself, its name and its
members, stays GitHub's.`,
		Example: `  forge team update billing --oncall billing-primary
  forge team update billing --oncall ""`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("oncall") {
				return exitcode.Usagef("nothing to update").WithHint("pass --oncall <rotation>, or --oncall \"\" to remove it")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			name := "teams/" + strings.TrimPrefix(args[0], "teams/")
			resp, err := c.Teams.UpdateTeam(ctx, connect.NewRequest(&forgev1.UpdateTeamRequest{
				Team:         &forgev1.Team{Name: name, Oncall: oncall},
				UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"oncall"}},
				RequestId:    client.NewRequestID(),
				ValidateOnly: dryRun,
			}))
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			t := resp.Msg.GetTeam()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{t.GetName()}, Data: t})
			}
			what := "has no on-call rotation"
			if t.GetOncall() != "" {
				what = "is on call as " + t.GetOncall()
			}
			suffix := ""
			if dryRun {
				suffix = " (dry run, nothing changed)"
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s %s%s\n", t.GetName(), what, suffix)
			return err
		},
	}
	cmd.Flags().StringVar(&oncall, "oncall", "", "the rotation's name; \"\" removes it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate on the server without changing anything")
	return cmd
}
