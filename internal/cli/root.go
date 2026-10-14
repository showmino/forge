package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
)

// Run executes the CLI with args (without the program name) and returns the
// exit code. It is the only place that prints errors.
func Run(ctx context.Context, d Deps, args []string) int {
	f := newFactory(d)
	root := NewRoot(f)
	root.SetArgs(args)
	root.SetIn(d.IO.In)
	root.SetOut(d.IO.Out)
	root.SetErr(d.IO.ErrOut)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return int(exitcode.OK)
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(d.IO.ErrOut, "cancelled")
		return 130
	}
	e := classify(err)
	fmt.Fprintf(d.IO.ErrOut, "error: %s\n", e.Error())
	if e.Hint != "" {
		fmt.Fprintf(d.IO.ErrOut, "hint: %s\n", e.Hint)
	}
	return int(e.Code)
}

// classify maps cobra's own usage errors to exit code 2.
func classify(err error) *exitcode.Error {
	var e *exitcode.Error
	if errors.As(err, &e) {
		return e
	}
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand flag", "flag needs an argument", "invalid argument", "accepts ", "requires at least", "required flag"} {
		if strings.HasPrefix(msg, p) {
			return &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "see 'forge --help'"}
		}
	}
	return exitcode.From(err)
}

// NewRoot builds the command tree.
func NewRoot(f *Factory) *cobra.Command {
	root := &cobra.Command{
		Use:   "forge",
		Short: "Create, deploy and inspect services on the Forge platform",
		Long: `forge is the command-line interface to the Forge internal developer platform.

Commands follow 'forge <noun> <verb>'. Every command supports -o json for
scripts, never prompts when stdin is not a terminal, and returns documented
exit codes:

  0 success                 5 permission denied
  1 unexpected error        6 conflict or failed precondition
  2 invalid usage           7 operation failed
  3 not found               8 timed out
  4 not authenticated`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SuggestionsMinimumDistance = 2
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "see 'forge --help'"}
	})

	pf := root.PersistentFlags()
	pf.StringVarP(&f.flags.context, "context", "c", "", "Forge context to use (see 'forge context list')")
	pf.StringVar(&f.flags.apiURL, "api-url", "", "Forge API URL, overriding the context")
	pf.StringVarP(&f.flags.namespace, "namespace", "n", "", "namespace, overriding forge.yaml and the context")
	pf.StringVarP(&f.flags.output, "output", "o", "", "output format: table, json, yaml or name")
	pf.BoolVarP(&f.flags.quiet, "quiet", "q", false, "print only results and errors")
	pf.BoolVarP(&f.flags.verbose, "verbose", "v", false, "print request IDs and where settings came from")
	pf.BoolVar(&f.flags.noInput, "no-input", false, "fail instead of prompting (implied when stdin is not a terminal)")
	pf.BoolVar(&f.flags.noColor, "no-color", false, "disable colour (also NO_COLOR)")
	pf.DurationVar(&f.flags.timeout, "timeout", 0, "give up after this long, for example 30s (0 = no limit)")
	_ = root.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"table", "json", "yaml", "name"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("context", f.completeContexts)

	root.AddGroup(
		&cobra.Group{ID: "core", Title: "Core commands:"},
		&cobra.Group{ID: "setup", Title: "Setup commands:"},
	)
	for _, c := range []*cobra.Command{newServiceCmd(f), newTemplateCmd(f), newTeamCmd(f), newCreateCmd(f), newEnvCmd(f), newDeployCmd(f), newResourceCmd(f), newOpCmd(f), newAuditCmd(f), newDocsCmd(f)} {
		c.GroupID = "core"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newAuthCmd(f), newContextCmd(f), newInitCmd(f), newDoctorCmd(f), newUpgradeCmd(f)} {
		c.GroupID = "setup"
		root.AddCommand(c)
	}
	v := newVersionCmd(f)
	v.GroupID = "setup"
	root.AddCommand(v)
	root.SetHelpCommandGroupID("setup")
	root.SetCompletionCommandGroupID("setup")
	return root
}

// noArgs rejects positional arguments with a usage error and a suggestion.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += fmt.Sprintf("; did you mean %q?", s[0])
	}
	return exitcode.Usagef("%s", msg).WithHint(fmt.Sprintf("see '%s --help'", cmd.CommandPath()))
}

// group returns a noun command that only holds verbs.
func group(use, short string, aliases ...string) *cobra.Command {
	return &cobra.Command{
		Use:     use,
		Short:   short,
		Aliases: aliases,
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
}

func (f *Factory) completeContexts(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	cfg, _, err := f.Config()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	names := make([]string, 0, len(cfg.Contexts))
	for _, c := range cfg.Contexts {
		names = append(names, c.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
