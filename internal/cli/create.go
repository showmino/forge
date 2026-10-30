package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/template"
	"github.com/showmino/forge/pkg/client"
)

func newCreateCmd(f *Factory) *cobra.Command {
	var tmpl, team, image, health string
	var chart, chartRepo, chartVersion, expose, valuesFile, stagingValues, productionValues string
	var owners, sets, writable []string
	var dryRun, deploy bool
	var port int
	var run runFlags
	cmd := &cobra.Command{
		Use:   "create <name> (--template <template>[@version] | --image <image> | --chart <chart>) --team <team>",
		Short: "Create a service from a template, an image or a Helm chart",
		Long: `Create a service from a template: the Forge API renders the template,
creates the private repo github.com/<org>/<name>, pushes the first commit
(which starts its CI) and registers the service in the catalog. If a step
after the repo exists fails, the repo is deleted again.

Template parameters are set with --set name=value; on a terminal forge asks
for missing required ones. See 'forge template get <template>'.

Run options (--replicas, --resources, --env, --secret, --job,
--autoscaling-max) go into the new repo's forge.yaml, run section; a
template offers some of them (see 'forge template get'), and the rest are
refused. Change them later in forge.yaml.

A file template (type file in 'forge template get') makes no repo: it opens
a pull request that adds its files to the template's target repo, and
prints the pull request's URL. <name> names the branch and the files.

With --image instead of --template, the service runs an image someone else
built (nginx:1.27, ghcr.io/acme/tool:2.3) and gets no repo: its forge.yaml,
with run.image, is kept in the deploy repo. The image must have a tag other
than latest, and come from a registry the platform allows. --port and
--health say where it listens and what is probed; every run option may be
set. The image must run as a non-root user given by number (its USER): a
root image is refused. The root filesystem is read-only except /tmp:
--writable names more folders the image writes, each an empty folder. forge then deploys it to staging and watches the deploy (--deploy=false
does not); with -o json or yaml it prints the deployment. Deploy a new tag
with 'forge deploy start <name> --env staging --ref <tag>'.

With --chart, the service runs a published Helm chart (--chart-repo
oci://... or https://..., --chart-version one exact version) and gets no
repo: its forge.yaml, with chart, is kept in the deploy repo. --expose
<service>:<port> names the chart's Service that gets the address (the
chart's release is named after the service). --values, --staging-values and
--production-values are YAML files of the chart's values. The chart may make
only resources inside its namespace, and its pods may not be privileged. forge
then deploys it to staging and watches the deploy, as for --image. Deploy a
new version with 'forge deploy start <name> --env staging --ref <version>',
and new values with --values there.`,
		Example: `  forge create payments --template go-grpc --team billing
  forge create ledger --template go-grpc@v1.0.0 --team billing --owner ana@example.com --set port=9090
  forge create shop --template nextjs --team web --replicas 2 --env LOG_LEVEL=info \
    --secret stripe=vaults/Apps/items/shop-stripe:STRIPE_KEY=credential --job 'cleanup=0 3 * * *:node cleanup.js'
  forge create payments --template go-grpc --team billing --dry-run
  forge create web --image nginxinc/nginx-unprivileged:1.27 --health / --team web
  forge create podinfo --chart podinfo --chart-repo oci://ghcr.io/stefanprodan/charts --chart-version 6.7.1 \
    --expose podinfo:9898 --values podinfo.yaml --team web`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !manifest.ValidID(id) {
				return exitcode.Usagef("%q is not a valid service name (lowercase letters, digits and '-')", id)
			}
			sources := 0
			for _, s := range []string{tmpl, image, chart} {
				if s != "" {
					sources++
				}
			}
			switch {
			case sources == 0:
				return exitcode.Usagef("--template, --image or --chart is required").WithHint("see 'forge template list'")
			case sources > 1:
				return exitcode.Usagef("--template, --image and --chart: use one")
			case chart == "" && (chartRepo != "" || chartVersion != "" || expose != "" || valuesFile != "" || stagingValues != "" || productionValues != ""):
				return exitcode.Usagef("--chart-repo, --chart-version, --expose and the values files are for --chart")
			case chart != "" && (len(sets) > 0 || port != 0 || health != "" || len(writable) > 0):
				return exitcode.Usagef("--set, --port, --health and --writable do not apply to a chart: set them in its values")
			case image != "" && len(sets) > 0:
				return exitcode.Usagef("--set is for a template's parameters; an image has none")
			case tmpl != "" && (port != 0 || health != "" || len(writable) > 0):
				return exitcode.Usagef("--port, --health and --writable are for --image; a template sets its own")
			}
			raw, err := parseSets(sets)
			if err != nil {
				return err
			}
			runOptions, err := run.options()
			if err != nil {
				return err
			}
			if port < 0 || port > 65535 {
				return exitcode.Usagef("--port %d must be 1-65535", port)
			}
			if image != "" && (port != 0 || health != "" || len(writable) > 0) {
				if runOptions == nil {
					runOptions = &forgev1.RunOptions{}
				}
				runOptions.Port, runOptions.Health, runOptions.Writable = int32(port), health, writable //nolint:gosec // checked above
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if team == "" {
				if !f.interactive() {
					return exitcode.Usagef("--team is required")
				}
				if team, err = f.ask("Owning team"); err != nil {
					return err
				}
				if team == "" {
					return exitcode.Usagef("--team is required")
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			if chart != "" {
				src, err := chartSource(chart, chartRepo, chartVersion, expose, valuesFile, stagingValues, productionValues)
				if err != nil {
					return err
				}
				return f.createFromImage(ctx, c, format, &forgev1.CreateServiceRequest{
					ServiceId: id, Chart: src, OwnerTeam: team, Owners: owners,
					RequestId: client.NewRequestID(), ValidateOnly: dryRun, RunOptions: runOptions,
				}, deploy)
			}
			if image != "" {
				return f.createFromImage(ctx, c, format, &forgev1.CreateServiceRequest{
					ServiceId: id, Image: image, OwnerTeam: team, Owners: owners,
					RequestId: client.NewRequestID(), ValidateOnly: dryRun, RunOptions: runOptions,
				}, deploy)
			}
			name, version, _ := strings.Cut(tmpl, "@")
			tr, err := c.Templates.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{
				Name: "templates/" + strings.TrimPrefix(name, "templates/"), Version: version,
			}))
			if err != nil {
				return err
			}
			t := tr.Msg.GetTemplate()
			if err := f.completeParams(t, raw); err != nil {
				return err
			}
			sc, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			final, err := f.streamCreate(ctx, sc, &forgev1.CreateServiceRequest{
				ServiceId:       id,
				Template:        t.GetName(),
				TemplateVersion: t.GetVersion(), // pinned: what was validated is what is rendered
				Parameters:      raw,
				OwnerTeam:       team,
				Owners:          owners,
				RequestId:       client.NewRequestID(),
				ValidateOnly:    dryRun,
				RunOptions:      runOptions,
			})
			if errors.Is(err, errMayStillFinish) {
				return exitcode.From(errors.Unwrap(err)).WithHint(fmt.Sprintf("the create may still finish on the Forge API; check 'forge service get %s'", id))
			}
			if err != nil {
				return err
			}
			switch {
			case final.GetService() != nil:
				return f.printMutation(final.GetService(), "created", false)
			case final.GetChange() != nil:
				ch := final.GetChange()
				if format == output.Table {
					_, err := fmt.Fprintln(f.IO.Out, ch.GetPullRequestUrl())
					return err
				}
				return output.Print(f.IO.Out, format, output.View{Names: []string{ch.GetPullRequestUrl()}, Data: ch})
			case final.GetPlan() != nil:
				return f.printPlan(format, strings.TrimPrefix(t.GetName(), "templates/"), t.GetType() == "file", final.GetPlan())
			case final.GetFailure() != nil:
				fl := final.GetFailure()
				hint := "the repo was deleted again; fix the cause and run the same command"
				if !fl.GetRolledBack() {
					hint = "the repo could not be deleted; delete it on GitHub before trying again"
				}
				return exitcode.New(exitcode.OperationFailed, "%s failed: %s", fl.GetStep(), fl.GetMessage()).WithHint(hint)
			}
			return errors.New("the Forge API ended the create without a result")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tmpl, "template", "", "template to use, optionally with @version (see 'forge template list')")
	fl.StringVar(&team, "team", "", "owning team")
	fl.StringSliceVar(&owners, "owner", nil, "owner email or handle (repeatable)")
	fl.StringArrayVar(&sets, "set", nil, "template parameter as name=value (repeatable)")
	fl.BoolVar(&dryRun, "dry-run", false, "render and check without creating anything; print the files")
	fl.StringVar(&image, "image", "", "an image to run instead of a template, such as nginx:1.27; the service gets no repo")
	fl.IntVar(&port, "port", 0, "with --image: the port it listens on (default 8080)")
	fl.StringVar(&health, "health", "", "with --image: the path its readiness is probed at (default /healthz)")
	fl.StringSliceVar(&writable, "writable", nil, "with --image: folders it writes besides /tmp, such as /var/cache/nginx (repeatable)")
	fl.BoolVar(&deploy, "deploy", true, "with --image or --chart: deploy it to staging and watch the deploy")
	fl.StringVar(&chart, "chart", "", "a published Helm chart to run instead of a template, by name; the service gets no repo")
	fl.StringVar(&chartRepo, "chart-repo", "", "with --chart: its repository, oci://<registry>/<path> or https://<host>/<path>")
	fl.StringVar(&chartVersion, "chart-version", "", "with --chart: one exact version, such as 6.7.1")
	fl.StringVar(&expose, "expose", "", "with --chart: the chart's Service that gets the address, as <service>:<port>")
	fl.StringVar(&valuesFile, "values", "", "with --chart: a YAML file of the chart's values")
	fl.StringVar(&stagingValues, "staging-values", "", "with --chart: a YAML file of values for staging only, over --values")
	fl.StringVar(&productionValues, "production-values", "", "with --chart: a YAML file of values for production only, over --values")
	run.register(fl)
	return cmd
}

// parseSets turns repeated --set name=value into a map.
func parseSets(sets []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, exitcode.Usagef("--set %q must be name=value", s)
		}
		out[k] = v
	}
	return out, nil
}

// templateParam turns the API's parameter into the template package's, to
// reuse its type, pattern and enum checks before calling the API.
func templateParam(p *forgev1.TemplateParameter) template.Parameter {
	tp := template.Parameter{
		Name:    p.GetName(),
		Type:    template.ParamType(strings.ToLower(strings.TrimPrefix(p.GetType().String(), "TYPE_"))),
		Pattern: p.GetPattern(),
		Values:  p.GetValues(),
	}
	if p.Min != nil {
		m := int(p.GetMin())
		tp.Min = &m
	}
	if p.Max != nil {
		m := int(p.GetMax())
		tp.Max = &m
	}
	return tp
}

// completeParams rejects unknown or invalid values and fills in missing
// required ones by asking, or fails naming the --set flag without a terminal.
func (f *Factory) completeParams(t *forgev1.Template, raw map[string]string) error {
	byName := map[string]*forgev1.TemplateParameter{}
	for _, p := range t.GetParameters() {
		byName[p.GetName()] = p
	}
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if _, ok := byName[k]; !ok {
			return exitcode.Usagef("%s has no parameter %q", t.GetName(), k).
				WithHint(fmt.Sprintf("see 'forge template get %s'", strings.TrimPrefix(t.GetName(), "templates/")))
		}
	}
	for _, p := range t.GetParameters() {
		v, ok := raw[p.GetName()]
		if !ok {
			if !p.GetRequired() {
				continue
			}
			if !f.interactive() {
				return exitcode.Usagef("parameter %q is required", p.GetName()).
					WithHint(fmt.Sprintf("pass --set %s=<%s>", p.GetName(), paramType(p)))
			}
			var err error
			if v, err = f.ask(fmt.Sprintf("%s (%s)", p.GetName(), paramType(p))); err != nil {
				return err
			}
			raw[p.GetName()] = v
		}
		if _, err := templateParam(p).Convert(v); err != nil {
			return exitcode.Usagef("%v", err)
		}
	}
	return nil
}

// stepVerbs turn step names into progress lines.
var stepVerbs = map[string]string{
	"fetch": "fetched template", "render": "rendered", "create_repo": "created",
	"grant_team": "gave access to", "push": "pushed initial commit", "register": "registered", "pull_request": "opened pull request",
	"check_repo": "checked", "check_chart": "checked",
}

// errMayStillFinish wraps a stream error after the server may have started
// creating the repo.
var errMayStillFinish = errors.New("the create may still finish")

type mayStillFinish struct{ err error }

func (e mayStillFinish) Error() string        { return e.err.Error() }
func (e mayStillFinish) Unwrap() error        { return e.err }
func (e mayStillFinish) Is(target error) bool { return target == errMayStillFinish }

// streamCreate runs CreateService, printing finished steps on stderr, and
// returns its last message. If the stream drops before that message, it
// retries once with the same request_id: the server waits for the running
// create and replays its outcome.
func (f *Factory) streamCreate(ctx context.Context, c *client.Client, req *forgev1.CreateServiceRequest) (*forgev1.CreateServiceResponse, error) {
	for attempt := 0; ; attempt++ {
		stream, err := c.Catalog.CreateService(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		var final *forgev1.CreateServiceResponse
		for stream.Receive() {
			msg := stream.Msg()
			if s := msg.GetStep(); s != nil {
				if s.GetState() == forgev1.CreateStep_STATE_DONE {
					verb := stepVerbs[s.GetName()]
					if verb == "" {
						verb = s.GetName()
					}
					f.infof("✓ %s %s", verb, s.GetDetail())
				}
				continue
			}
			final = msg
		}
		err = stream.Err()
		_ = stream.Close()
		if final != nil {
			return final, nil
		}
		// An error the server did not send (a reset, a truncated stream) means
		// the connection dropped; the server may still be working.
		dropped := err == nil || !connect.IsWireError(err)
		if attempt == 0 && dropped && ctx.Err() == nil {
			f.infof("The connection dropped; asking the Forge API for the outcome...")
			continue
		}
		if err == nil {
			err = errors.New("the Forge API ended the create without a result")
		}
		if !req.GetValidateOnly() && (dropped || connect.CodeOf(err) == connect.CodeDeadlineExceeded || ctx.Err() != nil) {
			return nil, mayStillFinish{err}
		}
		return nil, err
	}
}

// printPlan prints what a --dry-run would create.
func (f *Factory) printPlan(format output.Format, tmpl string, file bool, p *forgev1.CreatePlan) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: p.GetFiles(), Data: p})
	}
	files := "files"
	if len(p.GetFiles()) == 1 {
		files = "file"
	}
	line := fmt.Sprintf("Would create %s from %s@%s with %d %s:\n", p.GetRepoUrl(), tmpl, p.GetTemplateVersion(), len(p.GetFiles()), files)
	if file {
		line = fmt.Sprintf("Would add %d %s to %s with %s@%s:\n", len(p.GetFiles()), files, p.GetRepoUrl(), tmpl, p.GetTemplateVersion())
	}
	if _, err := fmt.Fprint(f.IO.Out, line); err != nil {
		return err
	}
	for _, path := range p.GetFiles() {
		if _, err := fmt.Fprintf(f.IO.Out, "  %s\n", path); err != nil {
			return err
		}
	}
	return nil
}

// createFromImage creates a service that runs an image or a chart and, when
// deploy is set, deploys it to staging and watches the deploy.
func (f *Factory) createFromImage(ctx context.Context, c *client.Client, format output.Format, req *forgev1.CreateServiceRequest, deploy bool) error {
	sc, err := f.StreamClient(ctx)
	if err != nil {
		return err
	}
	id := req.GetServiceId()
	final, err := f.streamCreate(ctx, sc, req)
	if errors.Is(err, errMayStillFinish) {
		return exitcode.From(errors.Unwrap(err)).WithHint(fmt.Sprintf("the create may still finish on the Forge API; check 'forge service get %s'", id))
	}
	if err != nil {
		return err
	}
	switch {
	case final.GetPlan() != nil:
		if format != output.Table {
			return output.Print(f.IO.Out, format, output.View{Names: final.GetPlan().GetFiles(), Data: final.GetPlan()})
		}
		runs := req.GetImage()
		if ch := req.GetChart(); ch != nil {
			runs = fmt.Sprintf("chart %s %s from %s", ch.GetName(), ch.GetVersion(), ch.GetRepo())
		}
		_, err := fmt.Fprintf(f.IO.Out, "Would create services/%s, running %s, with its forge.yaml in the deploy repo:\n  %s\n",
			id, runs, strings.Join(final.GetPlan().GetFiles(), "\n  "))
		return err
	case final.GetFailure() != nil:
		fl := final.GetFailure()
		hint := "its forge.yaml was removed from the deploy repo again; fix the cause and run the same command"
		if !fl.GetRolledBack() {
			hint = fmt.Sprintf("its forge.yaml is still in the deploy repo (services/%s/forge.yaml); the next create overwrites it", id)
		}
		return exitcode.New(exitcode.OperationFailed, "%s failed: %s", fl.GetStep(), fl.GetMessage()).WithHint(hint)
	case final.GetService() == nil:
		return errors.New("the Forge API ended the create without a result")
	}
	if !deploy {
		return f.printMutation(final.GetService(), "created", false)
	}
	// One resource for -o json and yaml: the deployment, printed at the end.
	if format == output.Table {
		if err := f.printMutation(final.GetService(), "created", false); err != nil {
			return err
		}
	}
	resp, err := c.Deploys.StartDeploy(ctx, connect.NewRequest(&forgev1.StartDeployRequest{
		Service: "services/" + id, Env: "staging", RequestId: client.NewRequestID(),
	}))
	if err != nil {
		return exitcode.From(err).WithHint(fmt.Sprintf("services/%s is created; deploy it with 'forge deploy start %s --env staging'", id, id))
	}
	return f.finishDeploy(ctx, sc, format, resp.Msg.GetOperation(), id, "staging", true)
}

// chartSource is the --chart flags as a create's chart.
func chartSource(name, repo, version, expose, values, staging, production string) (*forgev1.ChartSource, error) {
	if repo == "" || version == "" || expose == "" {
		return nil, exitcode.Usagef("--chart needs --chart-repo, --chart-version and --expose")
	}
	svc, p, ok := strings.Cut(expose, ":")
	port, err := strconv.Atoi(p)
	if !ok || svc == "" || err != nil || port < 1 || port > 65535 {
		return nil, exitcode.Usagef("--expose %q must be <service>:<port>, such as podinfo:9898", expose)
	}
	src := &forgev1.ChartSource{Repo: repo, Name: name, Version: version, ExposeService: svc, ExposePort: int32(port)} //nolint:gosec // checked above
	for _, v := range []struct {
		flag, path string
		to         *string
	}{{"--values", values, &src.Values}, {"--staging-values", staging, &src.StagingValues}, {"--production-values", production, &src.ProductionValues}} {
		if v.path == "" {
			continue
		}
		data, err := readValues(v.flag, v.path)
		if err != nil {
			return nil, err
		}
		*v.to = data
	}
	return src, nil
}

// readValues reads a YAML file of chart values.
func readValues(flag, path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user names the file to send
	if err != nil {
		return "", exitcode.Usagef("%s: %v", flag, err)
	}
	if len(data) > manifest.MaxChartValues {
		return "", exitcode.Usagef("%s: %s takes %d bytes; at most %d", flag, path, len(data), manifest.MaxChartValues)
	}
	return string(data), nil
}
