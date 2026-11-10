package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newPolicyCmd(f *Factory) *cobra.Command {
	cmd := group("policy", "List the rules Forge checks, and set their modes", "rules")
	cmd.Long = `Forge checks rules before it applies a resource plan, promotes, runs an image
or a chart, or makes an infrastructure repo. An admin sets each switchable rule
to enforce (the action is refused), warn (it goes ahead, and the warning is in
the audit log) or off (the rule is not checked). The rules that guard Forge
itself are always enforced.`
	cmd.AddCommand(newPolicyListCmd(f), newPolicySetCmd(f))
	return cmd
}

func newPolicyListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the rules and their modes",
		Example: `  forge policy list
  forge policy list -o json | jq -r '.rules[] | select(.mode != .defaultMode) | .name'`,
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
			resp, err := c.Policy.ListRules(ctx, connect.NewRequest(&forgev1.ListRulesRequest{}))
			if err != nil {
				return err
			}
			v := output.View{Headers: []string{"rule", "kind", "mode", "title"}, Data: resp.Msg}
			for _, r := range resp.Msg.GetRules() {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(r.GetName(), "rules/"), r.GetKind(), modeText(r), r.GetTitle()})
				v.Names = append(v.Names, r.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

// modeText says a rule's mode, and when it is not the default or cannot change.
func modeText(r *forgev1.Rule) string {
	switch {
	case !r.GetSwitchable():
		return "always"
	case r.GetMode() != r.GetDefaultMode():
		return r.GetMode() + " (default " + r.GetDefaultMode() + ")"
	default:
		return r.GetMode()
	}
}

func newPolicySetCmd(f *Factory) *cobra.Command {
	var mode string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "set <rule> --mode enforce|warn|off",
		Short: "Set a rule's mode",
		Long: `Set the mode of a switchable rule: enforce refuses the action, warn lets it go
ahead and records the warning in the audit log, off does not check it. Admins
only. Setting a rule back to its default mode is how to undo a change.`,
		Example: `  forge policy set images.pinned-version --mode warn
  forge policy set resources.production-ha --mode enforce --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mode == "" {
				return exitcode.Usagef("--mode is required: enforce, warn or off")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Policy.UpdateRule(ctx, connect.NewRequest(&forgev1.UpdateRuleRequest{
				Rule:         &forgev1.Rule{Name: "rules/" + strings.TrimPrefix(args[0], "rules/"), Mode: mode},
				UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"mode"}},
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
			r := resp.Msg.GetRule()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{r.GetName()}, Data: r})
			}
			suffix := ""
			if dryRun {
				suffix = " (dry run, nothing changed)"
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s is %s%s\n", r.GetName(), modeText(r), suffix)
			return err
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", "enforce, warn or off")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate on the server without changing anything")
	_ = cmd.RegisterFlagCompletionFunc("mode", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"enforce", "warn", "off"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}
