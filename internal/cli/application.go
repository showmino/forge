package cli

import (
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
)

const applicationPrefix = "applications/"

func newApplicationCmd(f *Factory) *cobra.Command {
	cmd := group("application", "See the applications services are grouped into", "applications")
	cmd.Long = `An application is a group of services (its parts) that belong together under
one name and one team: a web app, its API and its worker. A service joins an
application with 'forge service update <service> --application <name>', or
with service.application in its forge.yaml when it is created. An application
exists while a service names it.`
	cmd.AddCommand(newApplicationListCmd(f), newApplicationGetCmd(f))
	return cmd
}

func newApplicationListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the applications",
		Long:    "List every application, with its team and how many parts it has.",
		Example: `  forge application list
  forge application list -o json | jq -r '.applications[].name'`,
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
			resp, err := c.Applications.ListApplications(ctx, connect.NewRequest(&forgev1.ListApplicationsRequest{}))
			if err != nil {
				return err
			}
			apps := resp.Msg.GetApplications()
			if len(apps) == 0 && format == output.Table {
				f.infof("No applications. Make a service a part of one with 'forge service update <service> --application <name>'.")
				return nil
			}
			v := output.View{Headers: []string{"application", "team", "parts"}, Data: resp.Msg}
			if len(apps) == 0 {
				// protojson omits empty lists; keep '.applications[]' working in jq.
				v.Data = map[string]any{"applications": []any{}}
			}
			for _, a := range apps {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(a.GetName(), applicationPrefix), a.GetOwnerTeam(), strconv.Itoa(len(a.GetParts()))})
				v.Names = append(v.Names, a.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newApplicationGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show an application's parts and what each runs",
		Long: `Show the parts of an application: each service, and the version it runs in
staging and in production with its health. The columns are empty for a part
that does not run there.`,
		Example: `  forge application get shop
  forge application get shop -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimPrefix(args[0], applicationPrefix)
			if !manifest.ValidID(id) {
				return exitcode.Usagef("%q is not an application name: use a name like shop", args[0])
			}
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
			resp, err := c.Applications.GetApplication(ctx, connect.NewRequest(&forgev1.GetApplicationRequest{Name: applicationPrefix + id}))
			if err != nil {
				return err
			}
			app := resp.Msg.GetApplication()
			if format == output.Table {
				f.infof("%s, team %s", app.GetName(), app.GetOwnerTeam())
			}
			v := output.View{Headers: []string{"part", "staging", "production"}, Data: app}
			for _, p := range app.GetParts() {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(p.GetService(), "services/"), partRuns(p, "staging"), partRuns(p, "production")})
				v.Names = append(v.Names, p.GetService())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

// partRuns says what a part runs in env: its version (or, without one, its
// ref) and, unless it is healthy, its health. "" when it does not run there.
func partRuns(p *forgev1.ApplicationPart, env string) string {
	for _, d := range p.GetDeployments() {
		if d.GetEnv() != env {
			continue
		}
		what := d.GetVersion()
		if what == "" {
			what = d.GetRef()
		}
		if h := d.GetHealth(); h != forgev1.Deployment_HEALTH_HEALTHY {
			what += " (" + strings.ToLower(strings.TrimPrefix(h.String(), "HEALTH_")) + ")"
		}
		return what
	}
	return ""
}
