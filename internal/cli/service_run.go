package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

// runAction is what restart and scale share: the call, its dry run and its
// operation.
type runAction struct {
	env          string
	dryRun, wait bool
}

func (a *runAction) flags(cmd *cobra.Command, what string) {
	fl := cmd.Flags()
	fl.StringVar(&a.env, "env", "", "staging or production")
	fl.BoolVar(&a.dryRun, "dry-run", false, "check without "+what)
	fl.BoolVar(&a.wait, "wait", true, "watch it until the pods are healthy")
}

// run calls the API with the deployment's name and then watches the
// operation call started; dry and done print the outcome as a table.
func (a *runAction) run(f *Factory, cmd *cobra.Command, args []string, again string,
	call func(context.Context, *client.Client, string) (*forgev1.Operation, *forgev1.Deployment, error),
	dry func(*forgev1.Deployment) string, done func() string,
) error {
	if a.env == "" {
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
	c, err := f.StreamClient(ctx)
	if err != nil {
		return err
	}
	name := deployName(svc, a.env)
	op, dep, err := call(ctx, c, name)
	if err != nil {
		return err
	}
	if a.dryRun {
		if format != output.Table {
			return f.printDeployment(format, dep)
		}
		_, err := fmt.Fprintln(f.IO.Out, dry(dep))
		return err
	}
	if !a.wait {
		return f.printStarted(format, op)
	}
	finished, err := f.watchOperation(ctx, c, op.GetName())
	if err != nil {
		return err
	}
	if err := opFailure(finished); err != nil {
		return exitcode.From(err).WithHint(fmt.Sprintf("run '%s' again", fmt.Sprintf(again, svc, a.env)))
	}
	if format != output.Table {
		return f.printOperation(format, finished)
	}
	f.infof("%s", done())
	return nil
}

func newServiceRestartCmd(f *Factory) *cobra.Command {
	var a runAction
	cmd := &cobra.Command{
		Use:   "restart [service] --env staging|production",
		Short: "Restart a service's pods, one at a time",
		Long: `Restart the pods of one environment of a service with no new deploy: the
same version, new pods. Forge marks the pods in the GitOps repo and Argo CD
replaces them one at a time, each new pod ready before an old one stops.

Needs the service's team or an admin. It is not a deploy: the history and a
rollback do not list it; the audit log does.`,
		Example: `  forge service restart payments --env production
  forge service restart payments --env staging --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var name string
			return a.run(f, cmd, args, "forge service restart %s --env %s",
				func(ctx context.Context, c *client.Client, n string) (*forgev1.Operation, *forgev1.Deployment, error) {
					name = n
					resp, err := c.Deploys.RestartDeployment(ctx, connect.NewRequest(&forgev1.RestartDeploymentRequest{Name: n, ValidateOnly: a.dryRun}))
					if err != nil {
						return nil, nil, err
					}
					return resp.Msg.GetOperation(), resp.Msg.GetDeployment(), nil
				},
				func(d *forgev1.Deployment) string {
					return fmt.Sprintf("Would restart the %s of %s (%s)", pods(d.GetReplicas()), name, d.GetImage())
				},
				func() string { return name + " restarted: its new pods are healthy." })
		},
	}
	a.flags(cmd, "restarting")
	return cmd
}

func newServiceScaleCmd(f *Factory) *cobra.Command {
	var a runAction
	var replicas int32
	cmd := &cobra.Command{
		Use:   "scale [service] --env staging|production --replicas <n>",
		Short: "Set how many pods a service runs until its next deploy",
		Long: `Set how many pods one environment of a service runs, from 1 to 10. It lasts
until the next deploy, promote or rollback of that environment, which runs
forge.yaml's count again: run.replicas in production, one pod in staging. To
keep a production count, set run.replicas in forge.yaml.

An autoscaled service (run.autoscaling) refuses it: change run.autoscaling
instead. Needs the service's team or an admin.`,
		Example: `  forge service scale payments --env production --replicas 4
  forge service scale payments --env staging --replicas 2 --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("replicas") {
				return exitcode.Usagef("--replicas is required (1 to 10)")
			}
			var name string
			var previous int32
			return a.run(f, cmd, args, "forge service scale %s --env %s --replicas "+fmt.Sprint(replicas),
				func(ctx context.Context, c *client.Client, n string) (*forgev1.Operation, *forgev1.Deployment, error) {
					name = n
					resp, err := c.Deploys.ScaleDeployment(ctx, connect.NewRequest(&forgev1.ScaleDeploymentRequest{Name: n, Replicas: replicas, ValidateOnly: a.dryRun}))
					if err != nil {
						return nil, nil, err
					}
					previous = resp.Msg.GetPreviousReplicas()
					return resp.Msg.GetOperation(), resp.Msg.GetDeployment(), nil
				},
				func(*forgev1.Deployment) string {
					return fmt.Sprintf("Would scale %s from %s to %s", name, pods(previous), pods(replicas))
				},
				func() string {
					keep := fmt.Sprintf("; set run.replicas: %d in forge.yaml to keep it", replicas)
					if a.env != "production" {
						keep = ", which runs 1 pod again"
					}
					return fmt.Sprintf("%s runs %s (was %d) until the next deploy%s.", name, pods(replicas), previous, keep)
				})
		},
	}
	a.flags(cmd, "scaling")
	cmd.Flags().Int32Var(&replicas, "replicas", 0, "how many pods, 1 to 10")
	return cmd
}

func pods(n int32) string {
	if n == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", n)
}
