package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newResourceCmd(f *Factory) *cobra.Command {
	cmd := group("resource", "Plan and apply the resources a service declares", "resources")
	cmd.Long = `A service declares what it needs besides its own pods under 'resources' in
forge.yaml: today, one Postgres database. 'forge resource plan' shows what
would change in one environment and checks it against policy; 'forge resource
apply' then makes exactly that change. A plan is used once and expires after
24 hours. Deploy after an apply to give the app DATABASE_URL.`
	cmd.AddCommand(newResourcePlanCmd(f), newResourceApplyCmd(f), newResourceListCmd(f), newResourceGetCmd(f))
	return cmd
}

// loadManifest reads file, or the forge.yaml of the working directory when
// file is empty; nil when there is none there.
func (f *Factory) loadManifest(file string) (*manifest.Manifest, error) {
	if file != "" {
		m, err := manifest.Load(file)
		if err != nil {
			return nil, exitcode.Usagef("%v", err)
		}
		return m, nil
	}
	m, _, err := f.Manifest()
	if err != nil {
		return nil, exitcode.Usagef("forge.yaml: %v", err)
	}
	return m, nil
}

var planIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// planName accepts "<id>" or "resourcePlans/<id>".
func planName(s string) (string, error) {
	id := strings.TrimPrefix(s, "resourcePlans/")
	if !planIDRE.MatchString(id) {
		return "", exitcode.Usagef("%q is not a plan name (resourcePlans/<id>)", s).WithHint("see the output of 'forge resource plan'")
	}
	return "resourcePlans/" + id, nil
}

var changeSymbols = map[string]string{"create": "+", "update": "~", "delete": "-", "replace": "±"}

// settingsText reads a change's settings: "storage 5Gi → 10Gi, instances 1".
func settingsText(before, after *forgev1.ResourceSettings) string {
	one := func(s *forgev1.ResourceSettings) string {
		return fmt.Sprintf("storage %dGi, instances %d", s.GetStorageGi(), s.GetInstances())
	}
	switch {
	case before == nil && after == nil:
		return ""
	case before == nil:
		return one(after)
	case after == nil:
		return one(before)
	}
	storage := fmt.Sprintf("storage %dGi", after.GetStorageGi())
	if before.GetStorageGi() != after.GetStorageGi() {
		storage = fmt.Sprintf("storage %dGi → %dGi", before.GetStorageGi(), after.GetStorageGi())
	}
	instances := fmt.Sprintf("instances %d", after.GetInstances())
	if before.GetInstances() != after.GetInstances() {
		instances = fmt.Sprintf("instances %d → %d", before.GetInstances(), after.GetInstances())
	}
	return storage + ", " + instances
}

// planText is a plan as people read it.
func planText(p *forgev1.ResourcePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan %s for %s (%s)", p.GetName(), strings.TrimPrefix(p.GetService(), "services/"), p.GetEnv())
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
	case forgev1.ResourcePlan_STATE_DENIED:
		b.WriteString(": denied by policy")
	default:
		b.WriteString(": " + strings.ReplaceAll(lowerEnum(p.GetState(), "STATE_"), "_", " "))
	}
	b.WriteString("\n")
	for _, c := range p.GetChanges() {
		symbol := changeSymbols[c.GetAction()]
		if symbol == "" {
			symbol = "?"
		}
		fmt.Fprintf(&b, "  %s %s %s   %s\n", symbol, c.GetType(), c.GetResource(), settingsText(c.GetBefore(), c.GetAfter()))
	}
	pol := p.GetPolicy()
	if pol != nil && len(p.GetChanges()) == 0 {
		b.WriteString("  No changes\n")
	}
	if p.GetMessage() != "" {
		fmt.Fprintf(&b, "  %s\n", p.GetMessage())
	}
	for _, m := range pol.GetDeny() {
		fmt.Fprintf(&b, "  ✗ %s\n", m)
	}
	for _, m := range pol.GetWarn() {
		fmt.Fprintf(&b, "  ! %s\n", m)
	}
	if pol != nil && !pol.GetDenied() {
		b.WriteString("Policy: passed\n")
	}
	if p.GetState() == forgev1.ResourcePlan_STATE_PLANNED && len(p.GetChanges()) > 0 {
		fmt.Fprintf(&b, "Apply it with: forge resource apply %s\n", p.GetName())
	}
	return b.String()
}

func (f *Factory) printResourcePlan(format output.Format, p *forgev1.ResourcePlan) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{p.GetName()}, Data: p})
	}
	_, err := fmt.Fprint(f.IO.Out, planText(p))
	return err
}

func newResourcePlanCmd(f *Factory) *cobra.Command {
	var env, file string
	var allowDestroy, wait bool
	cmd := &cobra.Command{
		Use:   "plan --env staging|production",
		Short: "Show what forge.yaml's resources would change, and check it against policy",
		Long: `Plan the resources in forge.yaml for one environment. Nothing changes: the
plan is saved, checked against policy, and printed with its name. Exits 5
when policy denies it. A plan that deletes a database is denied unless it is
made with --allow-destroy.`,
		Example: `  forge resource plan --env staging
  forge resource plan --env production -f ./forge.yaml
  forge resource plan --env staging --allow-destroy`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			m, err := f.loadManifest(file)
			if err != nil {
				return err
			}
			if m == nil {
				return exitcode.Usagef("no forge.yaml here").WithHint("run inside a service repo, or pass -f <file>")
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
			resp, err := c.Resources.PlanResources(ctx, connect.NewRequest(&forgev1.PlanResourcesRequest{
				Service: "services/" + m.Service.Name, Env: env, Resources: m.Declarations(),
				AllowDestroy: allowDestroy, RequestId: client.NewRequestID(),
			}))
			if err != nil {
				return err
			}
			if !wait {
				return f.printStarted(format, resp.Msg.GetOperation())
			}
			op, err := f.watchOperation(ctx, c, resp.Msg.GetOperation().GetName())
			if err != nil {
				return err
			}
			got, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: resp.Msg.GetPlan().GetName()}))
			if err != nil {
				return err
			}
			p := got.Msg.GetPlan()
			if err := f.printResourcePlan(format, p); err != nil {
				return err
			}
			if p.GetState() == forgev1.ResourcePlan_STATE_DENIED {
				return exitcode.New(exitcode.PermissionDenied, "%s was denied by policy", p.GetName())
			}
			if err := opFailure(op); err != nil {
				return err
			}
			if len(m.Resources) > 0 {
				f.previewNote(ctx, c, m.Service.Name)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&env, "env", "", "staging or production")
	fl.StringVarP(&file, "file", "f", "", "manifest to read (default: forge.yaml here or in a parent directory)")
	fl.BoolVar(&allowDestroy, "allow-destroy", false, "let the plan delete resources")
	fl.BoolVar(&wait, "wait", true, "watch the plan until it is made")
	return cmd
}

// previewNote says, when the service has previews, that they get no database.
func (f *Factory) previewNote(ctx context.Context, c *client.Client, svc string) {
	resp, err := c.Environments.ListEnvironments(ctx, connect.NewRequest(&forgev1.ListEnvironmentsRequest{Service: "services/" + svc}))
	if err == nil && len(resp.Msg.GetEnvironments()) > 0 {
		f.infof("Previews of %s get no database and no DATABASE_URL.", svc)
	}
}

// confirmApply asks before an apply: the name of every resource the plan
// deletes or replaces, or yes/no when it deletes nothing.
func (f *Factory) confirmApply(p *forgev1.ResourcePlan, yes bool) error {
	svc := strings.TrimPrefix(p.GetService(), "services/")
	destructive := false
	for _, c := range p.GetChanges() {
		if c.GetAction() != "delete" && c.GetAction() != "replace" {
			continue
		}
		destructive = true
		if err := f.confirmDestructive("delete", "resources/"+svc+"/"+p.GetEnv()+"/"+c.GetResource(), yes); err != nil {
			return err
		}
	}
	if destructive {
		return nil
	}
	return f.confirm("Apply this plan?", yes)
}

func newResourceApplyCmd(f *Factory) *cobra.Command {
	var env, file string
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "apply [plan] [--env staging|production]",
		Short: "Apply a saved plan",
		Long: `Apply exactly the plan 'forge resource plan' saved. Give its name, or --env
to apply your own newest plan that is ready for that environment. The plan is
shown and confirmed first; a plan that deletes a database asks for the
resource's name. Apply refuses a plan that was used, expired, or made before
forge.yaml or the resources changed (exit 6), and one policy now denies
(exit 5).`,
		Example: `  forge resource apply --env staging
  forge resource apply resourcePlans/7f3a91c2b04e --yes
  forge resource apply --env production --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0 && env == "":
				return exitcode.Usagef("give a plan name, or --env to apply your newest plan")
			case len(args) == 1 && env != "":
				return exitcode.Usagef("give a plan name or --env, not both")
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			// A manifest is optional here: with one, the API also checks that
			// forge.yaml did not change since the plan.
			m, err := f.loadManifest(file)
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				if name, err = planName(args[0]); err != nil {
					return err
				}
			} else {
				if m == nil {
					return exitcode.Usagef("--env needs a forge.yaml to know the service").WithHint("run inside a service repo, pass -f <file>, or give a plan name")
				}
				resp, err := c.Resources.ListResourcePlans(ctx, connect.NewRequest(&forgev1.ListResourcePlansRequest{
					Service: "services/" + m.Service.Name, Env: env, Mine: true, PageSize: 1,
					States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_PLANNED},
				}))
				if err != nil {
					return err
				}
				if len(resp.Msg.GetPlans()) == 0 {
					return exitcode.New(exitcode.NotFound, "you have no plan ready to apply for %s (%s)", m.Service.Name, env).
						WithHint("run 'forge resource plan --env " + env + "' first")
				}
				name = resp.Msg.GetPlans()[0].GetName()
			}
			got, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: name}))
			if err != nil {
				return err
			}
			p := got.Msg.GetPlan()
			svc := strings.TrimPrefix(p.GetService(), "services/")
			req := &forgev1.ApplyResourcePlanRequest{Name: name, ValidateOnly: dryRun}
			if m != nil && m.Service.Name == svc {
				req.Resources, req.CheckResources = m.Declarations(), true
			}
			if dryRun {
				resp, err := c.Resources.ApplyResourcePlan(ctx, connect.NewRequest(req))
				if err != nil {
					return err
				}
				if format != output.Table {
					return f.printResourcePlan(format, resp.Msg.GetPlan())
				}
				_, err = fmt.Fprintf(f.IO.Out, "Would apply %s to %s (%s)\n", name, svc, p.GetEnv())
				return err
			}
			if !yes {
				fmt.Fprint(f.IO.ErrOut, planText(p))
			}
			if err := f.confirmApply(p, yes); err != nil {
				return err
			}
			resp, err := c.Resources.ApplyResourcePlan(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !wait {
				return f.printStarted(format, resp.Msg.GetOperation())
			}
			op, err := f.watchOperation(ctx, c, resp.Msg.GetOperation().GetName())
			if err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge resource list %s --env %s'; plan and apply again to repair it", svc, p.GetEnv()))
			}
			if format != output.Table {
				done, err := c.Resources.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: name}))
				if err != nil {
					return err
				}
				return f.printResourcePlan(format, done.Msg.GetPlan())
			}
			_, err = fmt.Fprintf(f.IO.Out, "Applied %s to %s (%s)\n", name, svc, p.GetEnv())
			return err
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&env, "env", "", "apply your newest ready plan for this environment")
	fl.StringVarP(&file, "file", "f", "", "manifest to check the plan against (default: forge.yaml here or in a parent directory)")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "run every check without applying")
	fl.BoolVar(&wait, "wait", true, "watch the apply until it ends")
	return cmd
}

// healthText reads a resource's live health: "Cluster in healthy state (2/2 ready)".
func healthText(h *forgev1.ResourceHealth) string {
	if h == nil || h.GetPhase() == "" || h.GetPhase() == "unknown" {
		return "unknown"
	}
	return fmt.Sprintf("%s (%d/%d ready)", h.GetPhase(), h.GetReadyInstances(), h.GetInstances())
}

func newResourceListCmd(f *Factory) *cobra.Command {
	var env string
	cmd := &cobra.Command{
		Use:     "list [service] [--env staging|production]",
		Aliases: []string{"ls"},
		Short:   "List resources with their state and live health",
		Long: `List resources. Without a service name, inside a service repo it shows that
service, and elsewhere every service.`,
		Example: `  forge resource list
  forge resource list payments --env production -o json`,
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
			req := &forgev1.ListResourcesRequest{Env: env}
			if svc != "" {
				req.Service = "services/" + svc
			}
			resp, err := c.Resources.ListResources(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			rs := resp.Msg.GetResources()
			if len(rs) == 0 && format == output.Table {
				f.infof("No resources.")
				return nil
			}
			v := output.View{Headers: []string{"service", "env", "name", "type", "storage", "instances", "state", "health"}, Data: resp.Msg}
			if len(rs) == 0 {
				v.Data = map[string]any{"resources": []any{}} // keep '.resources[]' working in jq
			}
			for _, r := range rs {
				s := r.GetSettings()
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(r.GetService(), "services/"), r.GetEnv(), s.GetName(), r.GetType(),
					fmt.Sprintf("%dGi", s.GetStorageGi()), fmt.Sprint(s.GetInstances()),
					lowerEnum(r.GetState(), "STATE_"), healthText(r.GetHealth()),
				})
				v.Names = append(v.Names, r.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "only this environment")
	return cmd
}

func newResourceGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <service>/<env>/<name>",
		Short:   "Show one resource",
		Example: "  forge resource get payments/staging/main\n  forge resource get resources/payments/production/main -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			short := strings.TrimPrefix(args[0], "resources/")
			if strings.Count(short, "/") != 2 {
				return exitcode.Usagef("%q is not a resource name (<service>/<env>/<name>)", args[0]).WithHint("see 'forge resource list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Resources.GetResource(ctx, connect.NewRequest(&forgev1.GetResourceRequest{Name: "resources/" + short}))
			if err != nil {
				return err
			}
			r := resp.Msg.GetResource()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{r.GetName()}, Data: r})
			}
			s := r.GetSettings()
			rows := [][]string{
				{"name", r.GetName()},
				{"service", r.GetService()},
				{"env", r.GetEnv()},
				{"type", r.GetType()},
				{"storage", fmt.Sprintf("%dGi", s.GetStorageGi())},
				{"instances", fmt.Sprint(s.GetInstances())},
				{"state", lowerEnum(r.GetState(), "STATE_")},
				{"health", healthText(r.GetHealth())},
				{"secret", r.GetSecret()},
				{"last plan", r.GetLastPlan()},
			}
			if r.GetUpdateTime() != nil {
				rows = append(rows, []string{"updated", r.GetUpdateTime().AsTime().UTC().Format(time.RFC3339) + " (" + age(f.Now().Sub(r.GetUpdateTime().AsTime())) + " ago)"})
			}
			return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
		},
	}
}
