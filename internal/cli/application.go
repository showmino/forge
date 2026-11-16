package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

const applicationPrefix = "applications/"

func newApplicationCmd(f *Factory) *cobra.Command {
	cmd := group("application", "See the applications services are grouped into", "applications")
	cmd.Long = `An application is a group of services (its parts) that belong together under
one name and one team: a web app, its API and its worker. A service joins an
application with 'forge service update <service> --application <name>', or
with service.application in its forge.yaml when it is created. An application
exists while a service names it.

The parts go to production together: 'forge application promote' promotes
every part whose staging differs from production and records a release, and
'forge application rollback' returns every part to an earlier release.`
	cmd.AddCommand(newApplicationListCmd(f), newApplicationGetCmd(f), newApplicationPromoteCmd(f), newApplicationRollbackCmd(f), newApplicationReleasesCmd(f))
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
staging and in production with its health, and the parts it links to
(run.links in its forge.yaml). The columns are empty for a part that does not
run there.`,
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
			return f.printApplication(ctx, c, format, id)
		},
	}
}

// partLinks names the parts p links to (run.links), as what it runs says:
// production's when it runs there, else staging's.
func partLinks(p *forgev1.ApplicationPart) string {
	var links []string
	for _, d := range p.GetDeployments() {
		if links == nil || d.GetEnv() == "production" {
			links = d.GetLinks()
		}
	}
	for i, l := range links {
		// "services/shop-api/resources/main" is that part's database.
		links[i] = strings.Replace(strings.TrimPrefix(l, "services/"), "/resources/", "/", 1)
	}
	return strings.Join(links, ", ")
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

// applicationID is the application an argument names: "shop" or
// "applications/shop".
func applicationID(arg string) (string, error) {
	id := strings.TrimPrefix(arg, applicationPrefix)
	if !manifest.ValidID(id) {
		return "", exitcode.Usagef("%q is not an application name: use a name like shop", arg)
	}
	return id, nil
}

// plannedParts prints what a promotion or rollback does to each part.
func (f *Factory) plannedParts(format output.Format, data any, parts []*forgev1.PlannedPart) error {
	v := output.View{Headers: []string{"part", "from", "to"}, Data: data}
	for _, p := range parts {
		v.Rows = append(v.Rows, []string{strings.TrimPrefix(p.GetService(), "services/"), p.GetFromVersion(), p.GetToVersion()})
		v.Names = append(v.Names, p.GetService())
	}
	return output.Print(f.IO.Out, format, v)
}

// finishApplication watches an application's operation to its end and
// prints the application as it then is.
func (f *Factory) finishApplication(ctx context.Context, c *client.Client, format output.Format, op *forgev1.Operation, id string, wait bool) error {
	if !wait {
		return f.printStarted(format, op)
	}
	op, err := f.watchOperation(ctx, c, op.GetName())
	if err != nil {
		return err
	}
	if err := opFailure(op); err != nil {
		return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge application get %s'", id))
	}
	return f.printApplication(ctx, c, format, id)
}

// printApplication prints an application's parts and what each runs.
func (f *Factory) printApplication(ctx context.Context, c *client.Client, format output.Format, id string) error {
	resp, err := c.Applications.GetApplication(ctx, connect.NewRequest(&forgev1.GetApplicationRequest{Name: applicationPrefix + id}))
	if err != nil {
		return err
	}
	app := resp.Msg.GetApplication()
	if format == output.Table {
		f.infof("%s, team %s", app.GetName(), app.GetOwnerTeam())
	}
	v := output.View{Headers: []string{"part", "staging", "production", "links to"}, Data: app}
	for _, p := range app.GetParts() {
		v.Rows = append(v.Rows, []string{strings.TrimPrefix(p.GetService(), "services/"), partRuns(p, "staging"), partRuns(p, "production"), partLinks(p)})
		v.Names = append(v.Names, p.GetService())
	}
	return output.Print(f.IO.Out, format, v)
}

func newApplicationPromoteCmd(f *Factory) *cobra.Command {
	var bump string
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "promote <name>",
		Short: "Promote every part of an application to production",
		Long: `Promote, as one operation, every part whose staging differs from production:
a part after the parts it links to. Each part is promoted as 'forge deploy
promote' would, under the same rules, and gets its own version.

If a part fails, it and the parts promoted before it are put back to what
they ran, so production is not left half promoted. A promotion that succeeds
records a release ('forge application releases').

It asks for the application's name unless --yes. --dry-run shows what it
would promote.`,
		Example: `  forge application promote shop --dry-run
  forge application promote shop --yes
  forge application promote shop --bump minor`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := applicationID(args[0])
			if err != nil {
				return err
			}
			level, err := parseBump(bump)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if !dryRun {
				if err := f.confirmDestructive("promote to production", applicationPrefix+id, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Applications.PromoteApplication(ctx, connect.NewRequest(&forgev1.PromoteApplicationRequest{Name: applicationPrefix + id, ValidateOnly: dryRun, Bump: level}))
			if err != nil {
				return err
			}
			if dryRun {
				if format == output.Table {
					f.infof("Would promote these parts of %s%s, in this order:", applicationPrefix, id)
				}
				return f.plannedParts(format, resp.Msg, resp.Msg.GetParts())
			}
			return f.finishApplication(ctx, c, format, resp.Msg.GetOperation(), id, wait)
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "check without promoting")
	fl.BoolVar(&wait, "wait", true, "watch the promotion until it ends")
	fl.StringVar(&bump, "bump", "", "how far each part's version goes up: patch (default), minor or major")
	return cmd
}

func newApplicationRollbackCmd(f *Factory) *cobra.Command {
	var to int
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "rollback <name>",
		Short: "Return every part of an application to an earlier release",
		Long: `Return, as one operation, every part of an application to what it ran in
production in an earlier release: the one before the newest, or --to. A part
that runs that already is left alone, and so is a part that joined the
application after that release. The rollback is recorded as a new release.

It asks for the application's name unless --yes. --dry-run shows what it
would change.`,
		Example: `  forge application rollback shop --dry-run
  forge application rollback shop --yes
  forge application rollback shop --to 6`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := applicationID(args[0])
			if err != nil {
				return err
			}
			if to < 0 {
				return exitcode.Usagef("--to %d must be a release number", to)
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if !dryRun {
				if err := f.confirmDestructive("roll back production", applicationPrefix+id, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Applications.RollbackApplication(ctx, connect.NewRequest(&forgev1.RollbackApplicationRequest{Name: applicationPrefix + id, Release: int32(to), ValidateOnly: dryRun})) //nolint:gosec // a release number
			if err != nil {
				return err
			}
			if dryRun {
				if format == output.Table {
					f.infof("Would return %s%s to release %d:", applicationPrefix, id, resp.Msg.GetRelease().GetNumber())
				}
				return f.plannedParts(format, resp.Msg, resp.Msg.GetParts())
			}
			return f.finishApplication(ctx, c, format, resp.Msg.GetOperation(), id, wait)
		},
	}
	fl := cmd.Flags()
	fl.IntVar(&to, "to", 0, "the release to return to (default: the one before the newest)")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "check without rolling back")
	fl.BoolVar(&wait, "wait", true, "watch the rollback until it ends")
	return cmd
}

func newApplicationReleasesCmd(f *Factory) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "releases <name>",
		Short: "List an application's releases",
		Long: `List the releases of an application, newest first: what every part ran in
production after each promotion or rollback of the application.`,
		Example: `  forge application releases shop
  forge application releases shop -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := applicationID(args[0])
			if err != nil {
				return err
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
			resp, err := c.Applications.ListApplicationReleases(ctx, connect.NewRequest(&forgev1.ListApplicationReleasesRequest{Name: applicationPrefix + id, PageSize: int32(limit)})) //nolint:gosec // a small count
			if err != nil {
				return err
			}
			rs := resp.Msg.GetReleases()
			if len(rs) == 0 && format == output.Table {
				f.infof("No releases yet. 'forge application promote %s' records the first.", id)
				return nil
			}
			v := output.View{Headers: []string{"release", "when", "by", "parts", "note"}, Data: resp.Msg}
			if len(rs) == 0 {
				// protojson omits empty lists; keep '.releases[]' working in jq.
				v.Data = map[string]any{"releases": []any{}}
			}
			for _, r := range rs {
				var parts []string
				for _, p := range r.GetParts() {
					parts = append(parts, strings.TrimPrefix(p.GetService(), "services/")+" "+p.GetVersion())
				}
				v.Rows = append(v.Rows, []string{strconv.Itoa(int(r.GetNumber())), r.GetCreateTime().AsTime().Local().Format("2006-01-02 15:04"), r.GetCreator(), strings.Join(parts, ", "), r.GetNote()})
				v.Names = append(v.Names, r.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many (default 20)")
	return cmd
}
