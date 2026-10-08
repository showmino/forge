package cli

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

var envIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// envName accepts "x" or "environments/x" and returns "environments/x".
func envName(s string) (string, error) {
	id := strings.TrimPrefix(s, "environments/")
	if !envIDPattern.MatchString(id) {
		return "", exitcode.Usagef("%q is not a valid environment name (lowercase letters, digits and '-', at most 40)", s)
	}
	return "environments/" + id, nil
}

// until reads time left coarsely: "in 23h", "in 5m", "in <1m", "expired".
func until(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	return "in " + age(d)
}

func newEnvCmd(f *Factory) *cobra.Command {
	cmd := group("env", "Create and manage preview environments", "envs", "environment", "environments")
	cmd.Long = `A preview environment runs a service's CI image for one Git ref in its own
Kubernetes namespace, with a URL, until its TTL runs out. Creating and
destroying one are operations: forge watches them, and 'forge op watch'
picks them up again.`
	cmd.AddCommand(newEnvCreateCmd(f), newEnvListCmd(f), newEnvGetCmd(f), newEnvExtendCmd(f), newEnvDestroyCmd(f))
	return cmd
}

func newEnvCreateCmd(f *Factory) *cobra.Command {
	var service, ref string
	var ttl time.Duration
	var dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Run a service's CI image in a new preview environment",
		Long: `Create a preview environment: the Forge API resolves --ref in the service's
repo, waits until CI has pushed the image for that commit, creates the
namespace preview-<name> and waits until the service is ready. Inside a
service repo, --service defaults to forge.yaml's service.`,
		Example: `  forge env create pr-142 --service payments --ref feature/x --ttl 48h
  forge env create demo                  # in a service repo
  forge env create demo --wait=false     # then: forge op watch <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if service == "" {
				m, _, err := f.Manifest()
				if err != nil {
					return err
				}
				if m == nil {
					return exitcode.Usagef("--service is required outside a service repo")
				}
				service = m.Service.Name
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Environments.CreateEnvironment(ctx, connect.NewRequest(&forgev1.CreateEnvironmentRequest{
				EnvironmentId: strings.TrimPrefix(name, "environments/"),
				Service:       "services/" + strings.TrimPrefix(service, "services/"),
				Ref:           ref,
				Ttl:           durationpb.New(ttl),
				RequestId:     client.NewRequestID(),
				ValidateOnly:  dryRun,
			}))
			if err != nil {
				return err
			}
			env := resp.Msg.GetEnvironment()
			if dryRun {
				if format != output.Table {
					return output.Print(f.IO.Out, format, output.View{Names: []string{env.GetName()}, Data: env})
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would create %s for %s at %s (%s), expiring in %s:\n  url:   %s\n  image: %s\n",
					env.GetName(), env.GetService(), env.GetRef(), sha7(env.GetCommit()), age(ttl), env.GetUrl(), env.GetImage())
				return err
			}
			op := resp.Msg.GetOperation()
			if !wait {
				return f.printStarted(format, op)
			}
			if op, err = f.watchOperation(ctx, c, op.GetName()); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge env get %s'", strings.TrimPrefix(name, "environments/")))
			}
			got, err := c.Environments.GetEnvironment(ctx, connect.NewRequest(&forgev1.GetEnvironmentRequest{Name: name}))
			if err != nil {
				return err
			}
			return f.printEnv(format, got.Msg.GetEnvironment())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&service, "service", "", "service to run (default: forge.yaml's service)")
	fl.StringVar(&ref, "ref", "main", "branch, tag or commit to run")
	fl.DurationVar(&ttl, "ttl", 24*time.Hour, "lifetime, 1h to 168h")
	fl.BoolVar(&dryRun, "dry-run", false, "check and resolve without creating anything")
	fl.BoolVar(&wait, "wait", true, "watch the create until it ends")
	return cmd
}

// printStarted reports an operation started with --wait=false: its name,
// and on stderr how to watch it.
func (f *Factory) printStarted(format output.Format, op *forgev1.Operation) error {
	f.infof("Watch it with: forge op watch %s", opID(op.GetName()))
	if format != output.Table {
		return f.printOperation(format, op)
	}
	_, err := fmt.Fprintln(f.IO.Out, op.GetName())
	return err
}

// sha7 is a commit's first 7 characters.
func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func stateName(s forgev1.Environment_State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATE_"))
}

func (f *Factory) expires(env *forgev1.Environment) string {
	if env.GetExpireTime() == nil {
		return "-"
	}
	return until(env.GetExpireTime().AsTime().Sub(f.Now()))
}

// printEnv prints one environment.
func (f *Factory) printEnv(format output.Format, env *forgev1.Environment) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{env.GetName()}, Data: env})
	}
	rows := [][]string{
		{"name", env.GetName()},
		{"service", env.GetService()},
		{"state", stateName(env.GetState())},
	}
	if env.GetStateMessage() != "" {
		rows = append(rows, []string{"message", env.GetStateMessage()})
	}
	rows = append(rows,
		[]string{"ref", env.GetRef()},
		[]string{"commit", env.GetCommit()},
		[]string{"image", env.GetImage()},
		[]string{"url", env.GetUrl()},
		[]string{"namespace", env.GetNamespace()},
		[]string{"creator", env.GetCreator()},
		[]string{"expires", env.GetExpireTime().AsTime().UTC().Format(time.RFC3339) + " (" + f.expires(env) + ")"},
	)
	if env.GetOperation() != "" {
		rows = append(rows, []string{"operation", env.GetOperation()})
	}
	return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
}

func newEnvListCmd(f *Factory) *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List preview environments",
		Example: `  forge env list
  forge env list --service payments -o json`,
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
			req := &forgev1.ListEnvironmentsRequest{}
			if service != "" {
				req.Service = "services/" + strings.TrimPrefix(service, "services/")
			}
			resp, err := c.Environments.ListEnvironments(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			envs := resp.Msg.GetEnvironments()
			if len(envs) == 0 && format == output.Table {
				f.infof("No environments.")
				return nil
			}
			v := output.View{Headers: []string{"name", "service", "state", "ref", "commit", "expires", "url"}, Data: resp.Msg}
			if len(envs) == 0 {
				v.Data = map[string]any{"environments": []any{}} // keep '.environments[]' working in jq
			}
			for _, e := range envs {
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(e.GetName(), "environments/"), strings.TrimPrefix(e.GetService(), "services/"),
					stateName(e.GetState()), e.GetRef(), sha7(e.GetCommit()), f.expires(e), e.GetUrl(),
				})
				v.Names = append(v.Names, e.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only environments of this service")
	return cmd
}

func newEnvGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <name>",
		Short:   "Show a preview environment",
		Example: `  forge env get pr-142`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
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
			resp, err := c.Environments.GetEnvironment(ctx, connect.NewRequest(&forgev1.GetEnvironmentRequest{Name: name}))
			if err != nil {
				return err
			}
			return f.printEnv(format, resp.Msg.GetEnvironment())
		},
	}
}

func newEnvExtendCmd(f *Factory) *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:     "extend <name> --ttl <duration>",
		Short:   "Set a preview environment to expire after --ttl from now",
		Example: `  forge env extend pr-142 --ttl 48h`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			if ttl == 0 {
				return exitcode.Usagef("--ttl is required, for example --ttl 48h")
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
			resp, err := c.Environments.ExtendEnvironment(ctx, connect.NewRequest(&forgev1.ExtendEnvironmentRequest{Name: name, Ttl: durationpb.New(ttl)}))
			if err != nil {
				return err
			}
			env := resp.Msg.GetEnvironment()
			if format != output.Table {
				return f.printEnv(format, env)
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s expires %s (%s)\n", env.GetName(), env.GetExpireTime().AsTime().UTC().Format(time.RFC3339), f.expires(env))
			return err
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "new lifetime from now, 1h to 168h")
	return cmd
}

func newEnvDestroyCmd(f *Factory) *cobra.Command {
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Remove a preview environment",
		Long: `Remove a preview environment and its namespace. A create still running for
it is cancelled first. Asks for the name unless --yes.`,
		Example: `  forge env destroy pr-142 --yes
  forge env destroy pr-142 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if !dryRun {
				if err := f.confirmDestructive("destroy", name, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Environments.DestroyEnvironment(ctx, connect.NewRequest(&forgev1.DestroyEnvironmentRequest{Name: name, ValidateOnly: dryRun}))
			if err != nil {
				return err
			}
			if dryRun {
				if format != output.Table {
					return f.printEnv(format, resp.Msg.GetEnvironment())
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would destroy %s (namespace %s)\n", name, resp.Msg.GetEnvironment().GetNamespace())
				return err
			}
			op := resp.Msg.GetOperation()
			if !wait {
				return f.printStarted(format, op)
			}
			if op, err = f.watchOperation(ctx, c, op.GetName()); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			if format != output.Table {
				return f.printOperation(format, op)
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s destroyed\n", name)
			return err
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check without destroying anything")
	cmd.Flags().BoolVar(&wait, "wait", true, "watch the destroy until it ends")
	return cmd
}
