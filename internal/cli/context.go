package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/config"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newContextCmd(f *Factory) *cobra.Command {
	cmd := group("context", "Manage which Forge installation commands target", "ctx")
	cmd.Long = `A context names a Forge installation (its API URL) and a default namespace,
like a kubeconfig context. The config lives in ~/.config/forge/config.yaml
(or $XDG_CONFIG_HOME/forge/config.yaml). Credentials are never stored there.`
	cmd.AddCommand(newContextListCmd(f), newContextUseCmd(f), newContextCurrentCmd(f), newContextSetCmd(f), newContextDeleteCmd(f))
	return cmd
}

func newContextListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List contexts",
		Example: `  forge context list
  forge context list -o json`,
		Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, _, err := f.Config()
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			v := output.View{Headers: []string{"current", "name", "api", "namespace"}, Data: contextsData(cfg)}
			for _, c := range cfg.Contexts {
				cur := ""
				if c.Name == cfg.CurrentContext {
					cur = "*"
				}
				v.Rows = append(v.Rows, []string{cur, c.Name, c.API, c.Namespace})
				v.Names = append(v.Names, c.Name)
			}
			if len(cfg.Contexts) == 0 && format == output.Table {
				f.infof("No contexts. Add one with 'forge context set <name> --api <url>'.")
				return nil
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

type contextsJSON struct {
	CurrentContext string           `json:"currentContext"`
	Contexts       []config.Context `json:"contexts"`
}

func contextsData(cfg *config.Config) contextsJSON {
	out := contextsJSON{CurrentContext: cfg.CurrentContext, Contexts: cfg.Contexts}
	if out.Contexts == nil {
		out.Contexts = []config.Context{}
	}
	return out
}

func newContextUseCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "use <name>",
		Short:             "Make a context the default",
		Example:           `  forge context use staging-eu`,
		Args:              oneArg("context name"),
		ValidArgsFunction: f.completeContexts,
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, path, err := f.Config()
			if err != nil {
				return err
			}
			if _, ok := cfg.Context(args[0]); !ok {
				return exitcode.New(exitcode.NotFound, "context %q is not defined", args[0]).WithHint("see 'forge context list'")
			}
			cfg.CurrentContext = args[0]
			if err := cfg.Save(path); err != nil {
				return err
			}
			f.infof("Switched to context %q.", args[0])
			return nil
		},
	}
}

func newContextCurrentCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Print the context commands will use",
		Long:  "Print the context commands will use, after --context and FORGE_CONTEXT are applied.",
		Example: `  forge context current
  FORGE_CONTEXT=prod forge context current -v`,
		Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			r, err := f.Resolved()
			if err != nil {
				return err
			}
			if r.ContextName == "" {
				return exitcode.New(exitcode.NotFound, "no context selected").WithHint("run 'forge context use <name>'")
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if f.flags.verbose {
				f.infof("context from %s; api from %s", r.Sources["context"], r.Sources["api"])
			}
			return output.Print(f.IO.Out, format, output.View{
				Headers: []string{"name", "api", "namespace"},
				Rows:    [][]string{{r.ContextName, r.APIURL, r.Namespace}},
				Names:   []string{r.ContextName},
				Data:    config.Context{Name: r.ContextName, API: r.APIURL, Namespace: r.Namespace},
			})
		},
	}
}

func newContextSetCmd(f *Factory) *cobra.Command {
	var api string
	var use bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a context",
		Example: `  forge context set staging-eu --api https://forge.staging.example.com --namespace billing
  forge context set local --api http://localhost:8080 --use`,
		Args: oneArg("context name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, path, err := f.Config()
			if err != nil {
				return err
			}
			ctx, exists := cfg.Context(args[0])
			ctx.Name = args[0]
			if cmd.Flags().Changed("api") {
				if !strings.HasPrefix(api, "http://") && !strings.HasPrefix(api, "https://") {
					return exitcode.Usagef("--api %q must start with http:// or https://", api)
				}
				ctx.API = strings.TrimRight(api, "/")
			}
			if cmd.Flags().Changed("namespace") {
				ctx.Namespace = f.flags.namespace // the global --namespace flag
			}
			if ctx.API == "" {
				return exitcode.Usagef("--api is required for a new context")
			}
			cfg.SetContext(ctx)
			if use || cfg.CurrentContext == "" {
				cfg.CurrentContext = ctx.Name
			}
			if err := cfg.Save(path); err != nil {
				return err
			}
			verb := "Created"
			if exists {
				verb = "Updated"
			}
			f.infof("%s context %q.", verb, ctx.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&api, "api", "", "Forge API URL")
	cmd.Flags().BoolVar(&use, "use", false, "also make it the current context")
	return cmd
}

func newContextDeleteCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a context",
		Long: `Remove a context from the config. A sign-in is stored by API URL, not by
context, so it stays: end it first with 'forge auth logout --context <name>',
or afterwards with 'forge auth logout --api-url <url>'.`,
		Example:           `  forge context delete staging-eu`,
		Args:              oneArg("context name"),
		ValidArgsFunction: f.completeContexts,
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, path, err := f.Config()
			if err != nil {
				return err
			}
			name := args[0]
			ctx, ok := cfg.Context(name)
			if !ok {
				return exitcode.New(exitcode.NotFound, "context %q is not defined", name).WithHint("see 'forge context list'")
			}
			wasCurrent := cfg.CurrentContext == name
			cfg.DeleteContext(name)
			if err := cfg.Save(path); err != nil {
				return err
			}
			f.infof("Deleted context %q.", name)
			if wasCurrent {
				f.infof("No context is current now; pick one with 'forge context use <name>'.")
			}
			// Name the stored sign-in only when no other context reaches that API.
			if !slices.ContainsFunc(cfg.Contexts, func(c config.Context) bool { return c.API == ctx.API }) {
				if _, err := f.CredentialStore().Get(ctx.API); err == nil {
					f.infof("A sign-in for %s is still stored; remove it with 'forge auth logout --api-url %s'.", ctx.API, ctx.API)
				}
			}
			return nil
		},
	}
}

// oneArg is cobra.ExactArgs(1) with an exit-code-2 error that names the argument.
func oneArg(what string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return exitcode.Usagef("%s expects 1 argument (%s), got %d", cmd.CommandPath(), what, len(args)).
				WithHint("see '" + cmd.CommandPath() + " --help'")
		}
		return nil
	}
}
