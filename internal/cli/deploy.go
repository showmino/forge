package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func deployName(svc, env string) string { return "deployments/" + svc + "/" + env }

// deployService is the optional service argument, or forge.yaml's service.
func (f *Factory) deployService(args []string) (string, error) {
	if len(args) == 1 {
		return strings.TrimPrefix(args[0], "services/"), nil
	}
	m, _, err := f.Manifest()
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", exitcode.Usagef("a service name is required outside a service repo")
	}
	return m.Service.Name, nil
}

func lowerEnum(s fmt.Stringer, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), prefix))
}

func tag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// versionCell is a table cell for a deploy's version; older deploys have none.
func versionCell(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// parseBump reads --bump; empty leaves the choice (a patch) to the API.
func parseBump(s string) (forgev1.Bump, error) {
	switch s {
	case "":
		return forgev1.Bump_BUMP_UNSPECIFIED, nil
	case "patch":
		return forgev1.Bump_BUMP_PATCH, nil
	case "minor":
		return forgev1.Bump_BUMP_MINOR, nil
	case "major":
		return forgev1.Bump_BUMP_MAJOR, nil
	}
	return 0, exitcode.Usagef("--bump %q must be patch, minor or major", s)
}

// asVersion is " as v0.3.1" for a dry run that says which version it would
// cut; empty when the API cuts none.
func asVersion(d *forgev1.Deployment) string {
	if d.GetVersion() == "" {
		return ""
	}
	return " as " + d.GetVersion()
}

func newDeployCmd(f *Factory) *cobra.Command {
	cmd := group("deploy", "Deploy services to staging and production", "deploys")
	cmd.Long = `Deploy a service's CI image to staging or production. Every change is a
commit to Forge's GitOps repo that Argo CD applies; forge watches until Argo
CD reports the new version Synced and Healthy. Inside a service repo the
service name defaults to forge.yaml's.`
	cmd.AddCommand(newDeployStartCmd(f), newDeployStatusCmd(f), newDeployHistoryCmd(f), newDeployRollbackCmd(f), newDeployPromoteCmd(f))
	return cmd
}

func newDeployStartCmd(f *Factory) *cobra.Command {
	var env, ref, bump string
	var dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "start [service] --env staging|production",
		Short: "Deploy a ref of a service to an environment",
		Long: `Deploy a ref of a service to an environment. Every commit that reaches
staging gets a release candidate version (v0.3.1-rc.2); one that reaches
production gets a final version (v0.3.1), each with a GitHub release.

A service without a repo (forge create --image) runs its image's tag: --ref
is the tag to run (default the one its forge.yaml names), and a new tag is
written into that forge.yaml. The tag is pinned to its digest, so promoting
moves exactly what staging ran, and the version is the tag.`,
		Example: `  forge deploy start payments --env staging
  forge deploy start --env staging --ref v1.4.2    # in a service repo
  forge deploy start web --env staging --ref 1.28  # a service that runs nginx`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			level, err := parseBump(bump)
			if err != nil {
				return err
			}
			svc, err := f.deployService(args)
			if err != nil {
				return err
			}
			if ref == "" {
				if sha, _ := f.Env("GITHUB_SHA"); sha != "" {
					ref = sha // in GitHub Actions: the commit the job runs at
				}
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Deploys.StartDeploy(ctx, connect.NewRequest(&forgev1.StartDeployRequest{
				Service: "services/" + svc, Env: env, Ref: ref, RequestId: client.NewRequestID(), ValidateOnly: dryRun, Bump: level,
			}))
			if err != nil {
				return err
			}
			d := resp.Msg.GetDeployment()
			if dryRun {
				if format != output.Table {
					return f.printDeployment(format, d)
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would deploy %s (%s at %s) to %s at %s%s\n", d.GetImage(), d.GetRef(), sha7(d.GetCommit()), d.GetName(), d.GetUrl(), asVersion(d))
				return err
			}
			return f.finishDeploy(ctx, c, format, resp.Msg.GetOperation(), svc, env, wait)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "staging or production")
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit (default $GITHUB_SHA in GitHub Actions, else main); for a service without a repo, the image tag")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check and resolve without deploying")
	cmd.Flags().BoolVar(&wait, "wait", true, "watch the deploy until it ends")
	cmd.Flags().StringVar(&bump, "bump", "", "for a first deploy of a commit to production: patch (default), minor or major")
	return cmd
}

// finishDeploy watches op (unless !wait) and prints the deployment.
func (f *Factory) finishDeploy(ctx context.Context, c *client.Client, format output.Format, op *forgev1.Operation, svc, env string, wait bool) error {
	if !wait {
		return f.printStarted(format, op)
	}
	op, err := f.watchOperation(ctx, c, op.GetName())
	if err != nil {
		return err
	}
	if err := opFailure(op); err != nil {
		return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge deploy status %s --env %s'; roll back with 'forge deploy rollback %s --env %s'", svc, env, svc, env))
	}
	got, err := c.Deploys.GetDeployment(ctx, connect.NewRequest(&forgev1.GetDeploymentRequest{Name: deployName(svc, env)}))
	if err != nil {
		return err
	}
	return f.printDeployment(format, got.Msg.GetDeployment())
}

// printDeployment prints one deployment as a field/value table.
func (f *Factory) printDeployment(format output.Format, d *forgev1.Deployment) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{d.GetName()}, Data: d})
	}
	var rows [][]string
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, []string{k, v})
		}
	}
	add("name", d.GetName())
	add("service", d.GetService())
	add("env", d.GetEnv())
	add("version", d.GetVersion())
	add("image", d.GetImage())
	add("ref", d.GetRef())
	add("commit", d.GetCommit())
	add("url", d.GetUrl())
	add("sync", lowerEnum(d.GetSync(), "SYNC_STATUS_"))
	add("health", lowerEnum(d.GetHealth(), "HEALTH_"))
	add("message", d.GetMessage())
	add("deploy_commit", d.GetDeployCommit())
	add("deployed_by", d.GetDeployedBy())
	if d.GetDeployTime() != nil {
		add("deployed", d.GetDeployTime().AsTime().UTC().Format(time.RFC3339)+" ("+age(f.Now().Sub(d.GetDeployTime().AsTime()))+" ago)")
	}
	add("operation", d.GetOperation())
	return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
}

func newDeployStatusCmd(f *Factory) *cobra.Command {
	var env string
	cmd := &cobra.Command{
		Use:   "status [service] [--env staging|production]",
		Short: "Show what is deployed",
		Long: `Show deployments. With a service and --env, one deployment in detail;
otherwise a table. Without a service name, inside a service repo it shows that
service, and elsewhere every service.`,
		Example: `  forge deploy status
  forge deploy status payments --env production`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := ""
			if len(args) == 1 {
				svc = strings.TrimPrefix(args[0], "services/")
			} else {
				m, _, err := f.Manifest()
				if err != nil {
					return err
				}
				if m != nil {
					svc = m.Service.Name
				}
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
			if svc != "" && env != "" {
				resp, err := c.Deploys.GetDeployment(ctx, connect.NewRequest(&forgev1.GetDeploymentRequest{Name: deployName(svc, env)}))
				if err != nil {
					return err
				}
				return f.printDeployment(format, resp.Msg.GetDeployment())
			}
			req := &forgev1.ListDeploymentsRequest{Env: env}
			if svc != "" {
				req.Service = "services/" + svc
			}
			resp, err := c.Deploys.ListDeployments(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			ds := resp.Msg.GetDeployments()
			if len(ds) == 0 && format == output.Table {
				f.infof("No deployments.")
				return nil
			}
			v := output.View{Headers: []string{"service", "env", "version", "image", "sync", "health", "deployed", "by", "url"}, Data: resp.Msg}
			if len(ds) == 0 {
				v.Data = map[string]any{"deployments": []any{}} // keep '.deployments[]' working in jq
			}
			for _, d := range ds {
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(d.GetService(), "services/"), d.GetEnv(), versionCell(d.GetVersion()), tag(d.GetImage()),
					lowerEnum(d.GetSync(), "SYNC_STATUS_"), lowerEnum(d.GetHealth(), "HEALTH_"),
					age(f.Now().Sub(d.GetDeployTime().AsTime())), d.GetDeployedBy(), d.GetUrl(),
				})
				v.Names = append(v.Names, d.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "only this environment")
	return cmd
}

func newDeployHistoryCmd(f *Factory) *cobra.Command {
	var env string
	var limit int32
	cmd := &cobra.Command{
		Use:     "history [service] --env staging|production",
		Short:   "List past deploys, rollbacks and promotions",
		Example: `  forge deploy history payments --env production --limit 5`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			svc, err := f.deployService(args)
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
			resp, err := c.Deploys.ListDeployHistory(ctx, connect.NewRequest(&forgev1.ListDeployHistoryRequest{Name: deployName(svc, env), Limit: limit}))
			if err != nil {
				return err
			}
			recs := resp.Msg.GetRecords()
			if len(recs) == 0 && format == output.Table {
				f.infof("No deploys.")
				return nil
			}
			v := output.View{Headers: []string{"deploy", "kind", "version", "image", "commit", "actor", "age"}, Data: resp.Msg}
			if len(recs) == 0 {
				v.Data = map[string]any{"records": []any{}}
			}
			for _, r := range recs {
				v.Rows = append(v.Rows, []string{
					sha7(r.GetDeployCommit()), r.GetKind(), versionCell(r.GetVersion()), tag(r.GetImage()), sha7(r.GetCommit()), r.GetActor(),
					age(f.Now().Sub(r.GetTime().AsTime())),
				})
				v.Names = append(v.Names, sha7(r.GetDeployCommit()))
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "staging or production")
	cmd.Flags().Int32Var(&limit, "limit", 20, "show at most this many records")
	return cmd
}

func newDeployRollbackCmd(f *Factory) *cobra.Command {
	var env, to string
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "rollback [service] --env staging|production",
		Short: "Restore a previous version of a service",
		Long: `Roll an environment back by committing the previous version (or --to a
deploy commit from 'forge deploy history') to the GitOps repo. Asks for the
deployment name unless --yes.`,
		Example: `  forge deploy rollback payments --env production --yes
  forge deploy rollback payments --env staging --to 9f8e7d6 --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			svc, err := f.deployService(args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			name := deployName(svc, env)
			if !dryRun {
				if err := f.confirmDestructive("roll back", name, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Deploys.RollbackDeploy(ctx, connect.NewRequest(&forgev1.RollbackDeployRequest{Name: name, To: to, ValidateOnly: dryRun}))
			if err != nil {
				return err
			}
			if dryRun {
				if format != output.Table {
					return f.printDeployment(format, resp.Msg.GetDeployment())
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would roll back %s to %s\n", name, resp.Msg.GetDeployment().GetImage())
				return err
			}
			return f.finishDeploy(ctx, c, format, resp.Msg.GetOperation(), svc, env, wait)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&env, "env", "", "staging or production")
	fl.StringVar(&to, "to", "", "deploy commit to restore (default: the one before the current)")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "check without rolling back")
	fl.BoolVar(&wait, "wait", true, "watch the rollback until it ends")
	return cmd
}

func newDeployPromoteCmd(f *Factory) *cobra.Command {
	var from, to, bump string
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "promote [service]",
		Short: "Deploy what runs in one environment to the next",
		Long: `Promote the image running in --from (default staging) to --to (default
production). Promoting to production asks for the service name unless --yes.

The promotion releases the commit: it gets the next final version (a patch,
or what --bump says), a tag and a GitHub release with notes since the last
one. A commit that was released before keeps its version.`,
		Example: `  forge deploy promote payments --yes
  forge deploy promote payments --bump minor
  forge deploy promote payments --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := parseBump(bump)
			if err != nil {
				return err
			}
			svc, err := f.deployService(args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if to == "production" && !dryRun {
				if err := f.confirmDestructive("promote to production", "services/"+svc, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Deploys.PromoteDeploy(ctx, connect.NewRequest(&forgev1.PromoteDeployRequest{Service: "services/" + svc, From: from, To: to, ValidateOnly: dryRun, Bump: level}))
			if err != nil {
				return err
			}
			if dryRun {
				if format != output.Table {
					return f.printDeployment(format, resp.Msg.GetDeployment())
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would promote %s from %s to %s%s\n", resp.Msg.GetDeployment().GetImage(), from, to, asVersion(resp.Msg.GetDeployment()))
				return err
			}
			return f.finishDeploy(ctx, c, format, resp.Msg.GetOperation(), svc, to, wait)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&from, "from", "staging", "environment to take the image from")
	fl.StringVar(&to, "to", "production", "environment to deploy it to")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "check without promoting")
	fl.BoolVar(&wait, "wait", true, "watch the promotion until it ends")
	fl.StringVar(&bump, "bump", "", "how far the version goes up: patch (default), minor or major")
	return cmd
}
