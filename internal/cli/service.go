package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newServiceCmd(f *Factory) *cobra.Command {
	cmd := group("service", "Browse and manage the service catalog", "svc", "services")
	cmd.Long = `Browse and manage the service catalog.

Inside a repo with a forge.yaml, commands that take a service name default to
the service the manifest describes.`
	cmd.AddCommand(
		newServiceListCmd(f), newServiceGetCmd(f), newServiceRegisterCmd(f), newServiceUpdateCmd(f),
		newServiceOwnersCmd(f), newServiceDepsCmd(f), newServiceDeleteCmd(f),
	)
	return cmd
}

// --- name handling ---

// serviceName turns "payments" or "services/payments" into "services/payments".
// With no argument it uses forge.yaml.
func (f *Factory) serviceName(args []string) (string, error) {
	var id string
	switch {
	case len(args) > 1:
		return "", exitcode.Usagef("expected at most one service name, got %d", len(args))
	case len(args) == 1:
		id = strings.TrimPrefix(args[0], "services/")
	default:
		m, _ := f.Manifest()
		if m == nil {
			return "", exitcode.Usagef("no service name given and no forge.yaml found").
				WithHint("pass a name, for example 'payments', or run inside a repo with a forge.yaml")
		}
		id = m.Service.Name
	}
	if !manifest.ValidID(id) {
		return "", exitcode.Usagef("%q is not a valid service name (lowercase letters, digits and '-')", id)
	}
	return "services/" + id, nil
}

func (f *Factory) completeServices(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c, err := f.Client(cmd.Context(), true)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	resp, err := c.Catalog.ListServices(cmd.Context(), connect.NewRequest(&forgev1.ListServicesRequest{PageSize: 500}))
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var out []string
	for _, s := range resp.Msg.GetServices() {
		out = append(out, shortName(s.GetName()))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// --- lifecycle ---

var lifecycles = map[string]forgev1.Lifecycle{
	"experimental": forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL,
	"production":   forgev1.Lifecycle_LIFECYCLE_PRODUCTION,
	"deprecated":   forgev1.Lifecycle_LIFECYCLE_DEPRECATED,
}

func parseLifecycle(s string) (forgev1.Lifecycle, error) {
	if s == "" {
		return forgev1.Lifecycle_LIFECYCLE_UNSPECIFIED, nil
	}
	l, ok := lifecycles[s]
	if !ok {
		return 0, exitcode.Usagef("--lifecycle %q must be experimental, production or deprecated", s)
	}
	return l, nil
}

func lifecycleString(l forgev1.Lifecycle) string {
	for k, v := range lifecycles {
		if v == l {
			return k
		}
	}
	return ""
}

// --- views ---

func serviceRow(s *forgev1.Service) []string {
	return []string{shortName(s.GetName()), s.GetOwnerTeam(), lifecycleString(s.GetLifecycle()), strings.Join(s.GetOwners(), ", ")}
}

var serviceHeaders = []string{"name", "team", "lifecycle", "owners"}

// --- list ---

func newServiceListCmd(f *Factory) *cobra.Command {
	var team string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List services in the catalog",
		Example: `  forge service list
  forge service list --team billing -o json | jq -r '.services[].name'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return exitcode.Usagef("--limit must not be negative")
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
			all, err := listAll(ctx, c, team, limit)
			if err != nil {
				return err
			}
			if len(all.GetServices()) == 0 && format == output.Table {
				f.infof("No services found.")
				return nil
			}
			v := output.View{Headers: serviceHeaders, Data: all}
			if len(all.GetServices()) == 0 {
				// protojson omits empty lists; keep '.services[]' working in jq.
				v.Data = map[string]any{"services": []any{}}
			}
			for _, s := range all.GetServices() {
				v.Rows = append(v.Rows, serviceRow(s))
				v.Names = append(v.Names, s.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&team, "team", "", "only services owned by this team")
	cmd.Flags().IntVar(&limit, "limit", 0, "return at most this many services (0 = all)")
	return cmd
}

// listAll follows page tokens so scripts always get the whole result.
func listAll(ctx context.Context, c *client.Client, team string, limit int) (*forgev1.ListServicesResponse, error) {
	out := &forgev1.ListServicesResponse{}
	token := ""
	for {
		size := 500
		if limit > 0 {
			size = min(size, limit-len(out.Services))
		}
		resp, err := c.Catalog.ListServices(ctx, connect.NewRequest(&forgev1.ListServicesRequest{
			PageSize:  int32(size), //nolint:gosec // size is at most 500
			PageToken: token,
			OwnerTeam: team,
		}))
		if err != nil {
			return nil, err
		}
		out.Services = append(out.Services, resp.Msg.GetServices()...)
		token = resp.Msg.GetNextPageToken()
		if token == "" || (limit > 0 && len(out.Services) >= limit) {
			return out, nil
		}
	}
}

// --- get ---

func newServiceGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get [name]",
		Short: "Show one service",
		Example: `  forge service get payments
  forge service get payments -o yaml`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := f.getService(cmd, args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			return output.Print(f.IO.Out, format, output.View{
				Headers: []string{"field", "value"},
				Rows: [][]string{
					{"name", s.GetName()},
					{"display name", s.GetDisplayName()},
					{"team", s.GetOwnerTeam()},
					{"owners", strings.Join(s.GetOwners(), ", ")},
					{"lifecycle", lifecycleString(s.GetLifecycle())},
					{"repo", s.GetRepoUrl()},
					{"dependencies", strings.Join(s.GetDependencies(), ", ")},
					{"labels", formatLabels(s.GetLabels())},
					{"updated", s.GetUpdateTime().AsTime().Local().Format("2006-01-02 15:04")},
				},
				Names: []string{s.GetName()},
				Data:  s,
			})
		},
	}
}

func (f *Factory) getService(cmd *cobra.Command, args []string) (*forgev1.Service, error) {
	name, err := f.serviceName(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := f.withTimeout(cmd.Context())
	defer cancel()
	c, err := f.Client(ctx, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.Catalog.GetService(ctx, connect.NewRequest(&forgev1.GetServiceRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetService(), nil
}

func formatLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}

// --- register / update shared flags ---

type serviceFlags struct {
	displayName string
	team        string
	owners      []string
	lifecycle   string
	repo        string
	deps        []string
	labels      map[string]string
}

func (sf *serviceFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&sf.displayName, "display-name", "", "human-friendly name")
	fl.StringVar(&sf.team, "team", "", "owning team")
	fl.StringSliceVar(&sf.owners, "owner", nil, "owner email or handle (repeatable)")
	fl.StringVar(&sf.lifecycle, "lifecycle", "", "experimental, production or deprecated")
	fl.StringVar(&sf.repo, "repo", "", "source repository URL")
	fl.StringSliceVar(&sf.deps, "depends-on", nil, "service this one depends on (repeatable)")
	fl.StringToStringVar(&sf.labels, "label", nil, "label as key=value (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("lifecycle", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"experimental", "production", "deprecated"}, cobra.ShellCompDirectiveNoFileComp
	})
}

func depNames(deps []string) []string {
	out := make([]string, len(deps))
	for i, d := range deps {
		out[i] = "services/" + strings.TrimPrefix(d, "services/")
	}
	return out
}

// --- register ---

func newServiceRegisterCmd(f *Factory) *cobra.Command {
	var sf serviceFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "register [name]",
		Short: "Add a service to the catalog",
		Long: `Add a service to the catalog. Inside a repo with a forge.yaml, the manifest
supplies the name and fields; flags override them.`,
		Example: `  forge service register payments --team billing --owner ana@example.com --lifecycle production
  forge service register            # from ./forge.yaml
  forge service register ledger --team billing --depends-on payments --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := f.serviceName(args)
			if err != nil {
				return err
			}
			svc := &forgev1.Service{}
			if m, _ := f.Manifest(); m != nil && (len(args) == 0 || shortName(name) == m.Service.Name) {
				svc = fromManifest(m)
			}
			fl := cmd.Flags()
			if fl.Changed("display-name") {
				svc.DisplayName = sf.displayName
			}
			if fl.Changed("team") {
				svc.OwnerTeam = sf.team
			}
			if fl.Changed("owner") {
				svc.Owners = sf.owners
			}
			if fl.Changed("lifecycle") {
				l, err := parseLifecycle(sf.lifecycle)
				if err != nil {
					return err
				}
				svc.Lifecycle = l
			}
			if fl.Changed("repo") {
				svc.RepoUrl = sf.repo
			}
			if fl.Changed("depends-on") {
				svc.Dependencies = depNames(sf.deps)
			}
			if fl.Changed("label") {
				svc.Labels = sf.labels
			}
			if svc.GetOwnerTeam() == "" {
				return exitcode.Usagef("--team is required").WithHint("pass --team, or set service.team in forge.yaml")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Catalog.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
				ServiceId:    shortName(name),
				Service:      svc,
				RequestId:    client.NewRequestID(),
				ValidateOnly: dryRun,
			}))
			if err != nil {
				return err
			}
			return f.printMutation(resp.Msg.GetService(), "registered", dryRun)
		},
	}
	sf.bind(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate on the server without registering")
	return cmd
}

func fromManifest(m *manifest.Manifest) *forgev1.Service {
	l, _ := parseLifecycle(m.Service.Lifecycle)
	return &forgev1.Service{
		DisplayName:  m.Service.DisplayName,
		OwnerTeam:    m.Service.Team,
		Owners:       m.Service.Owners,
		Lifecycle:    l,
		RepoUrl:      m.Service.Repo,
		Dependencies: depNames(m.Service.Dependencies),
		Labels:       m.Service.Labels,
	}
}

// printMutation prints the resulting object: the resource in -o json/yaml,
// a one-line summary in table mode.
func (f *Factory) printMutation(s *forgev1.Service, verb string, dryRun bool) error {
	format, err := f.Format()
	if err != nil {
		return err
	}
	if format == output.Table {
		suffix := ""
		if dryRun {
			suffix = " (dry run, nothing changed)"
		}
		_, err := fmt.Fprintf(f.IO.Out, "%s %s%s\n", s.GetName(), verb, suffix)
		return err
	}
	return output.Print(f.IO.Out, format, output.View{Names: []string{s.GetName()}, Data: s})
}

// --- update ---

func newServiceUpdateCmd(f *Factory) *cobra.Command {
	var sf serviceFlags
	var dryRun bool
	var addOwners, removeOwners []string
	cmd := &cobra.Command{
		Use:   "update [name]",
		Short: "Change fields of a service",
		Long:  "Change fields of a service. Only the flags you pass are changed.",
		Example: `  forge service update payments --lifecycle deprecated
  forge service update payments --add-owner li@example.com --remove-owner ana@example.com`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := f.serviceName(args)
			if err != nil {
				return err
			}
			fl := cmd.Flags()
			svc := &forgev1.Service{Name: name}
			var paths []string
			set := func(flag, path string, apply func() error) error {
				if !fl.Changed(flag) {
					return nil
				}
				if !slices.Contains(paths, path) {
					paths = append(paths, path)
				}
				return apply()
			}
			steps := []struct {
				flag, path string
				apply      func() error
			}{
				{"display-name", "display_name", func() error { svc.DisplayName = sf.displayName; return nil }},
				{"team", "owner_team", func() error { svc.OwnerTeam = sf.team; return nil }},
				{"owner", "owners", func() error { svc.Owners = sf.owners; return nil }},
				{"lifecycle", "lifecycle", func() error {
					l, err := parseLifecycle(sf.lifecycle)
					svc.Lifecycle = l
					return err
				}},
				{"repo", "repo_url", func() error { svc.RepoUrl = sf.repo; return nil }},
				{"depends-on", "dependencies", func() error { svc.Dependencies = depNames(sf.deps); return nil }},
				{"label", "labels", func() error { svc.Labels = sf.labels; return nil }},
			}
			for _, s := range steps {
				if err := set(s.flag, s.path, s.apply); err != nil {
					return err
				}
			}
			if fl.Changed("owner") && (len(addOwners) > 0 || len(removeOwners) > 0) {
				return exitcode.Usagef("--owner replaces the owner list; do not combine it with --add-owner or --remove-owner")
			}

			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			if len(addOwners) > 0 || len(removeOwners) > 0 {
				// Read-modify-write, guarded by the etag so a concurrent change is not lost.
				cur, err := c.Catalog.GetService(ctx, connect.NewRequest(&forgev1.GetServiceRequest{Name: name}))
				if err != nil {
					return err
				}
				owners := slices.DeleteFunc(slices.Clone(cur.Msg.GetService().GetOwners()), func(o string) bool {
					return slices.Contains(removeOwners, o)
				})
				for _, o := range addOwners {
					if !slices.Contains(owners, o) {
						owners = append(owners, o)
					}
				}
				svc.Owners = owners
				svc.Etag = cur.Msg.GetService().GetEtag()
				paths = append(paths, "owners")
			}
			if len(paths) == 0 {
				return exitcode.Usagef("nothing to update").WithHint("pass at least one field flag, for example --lifecycle")
			}
			resp, err := c.Catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
				Service:      svc,
				UpdateMask:   &fieldmaskpb.FieldMask{Paths: paths},
				RequestId:    client.NewRequestID(),
				ValidateOnly: dryRun,
			}))
			if err != nil {
				return err
			}
			return f.printMutation(resp.Msg.GetService(), "updated", dryRun)
		},
	}
	sf.bind(cmd)
	cmd.Flags().StringSliceVar(&addOwners, "add-owner", nil, "add an owner (repeatable)")
	cmd.Flags().StringSliceVar(&removeOwners, "remove-owner", nil, "remove an owner (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate on the server without changing anything")
	return cmd
}

// --- owners / deps ---

func newServiceOwnersCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "owners [name]",
		Short: "List a service's team and owners",
		Example: `  forge service owners payments
  forge service owners -o name     # inside the repo`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := f.getService(cmd, args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			v := output.View{
				Headers: []string{"owner", "team"},
				Names:   s.GetOwners(),
				Data:    map[string]any{"name": s.GetName(), "ownerTeam": s.GetOwnerTeam(), "owners": nonNil(s.GetOwners())},
			}
			for _, o := range s.GetOwners() {
				v.Rows = append(v.Rows, []string{o, s.GetOwnerTeam()})
			}
			if len(v.Rows) == 0 {
				v.Rows = [][]string{{"", s.GetOwnerTeam()}}
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newServiceDepsCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "deps [name]",
		Aliases: []string{"dependencies"},
		Short:   "List the services a service depends on",
		Example: `  forge service deps ledger
  forge service deps ledger -o json`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := f.getService(cmd, args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if len(s.GetDependencies()) == 0 && format == output.Table {
				f.infof("%s has no dependencies.", s.GetName())
				return nil
			}
			v := output.View{
				Headers: []string{"dependency"},
				Names:   s.GetDependencies(),
				Data:    map[string]any{"name": s.GetName(), "dependencies": nonNil(s.GetDependencies())},
			}
			for _, d := range s.GetDependencies() {
				v.Rows = append(v.Rows, []string{d})
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

// --- delete ---

func newServiceDeleteCmd(f *Factory) *cobra.Command {
	var yes, dryRun bool
	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a service from the catalog",
		Long: `Remove a service from the catalog. You must type the service's name to
confirm, or pass --yes. The name is never taken from forge.yaml.`,
		Example: `  forge service delete payments
  forge service delete payments --yes
  forge service delete payments --dry-run`,
		Args:              exactArgs(1, "service name"),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := f.serviceName(args)
			if err != nil {
				return err
			}
			if !dryRun {
				if err := f.confirmDestructive("delete", name, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			if _, err := c.Catalog.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: name, ValidateOnly: dryRun})); err != nil {
				return err
			}
			if dryRun {
				f.infof("%s would be deleted (dry run, nothing changed).", name)
				return nil
			}
			f.infof("%s deleted.", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check that the delete would succeed without deleting")
	return cmd
}
