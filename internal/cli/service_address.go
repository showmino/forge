package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newServiceAddressCmd(f *Factory) *cobra.Command {
	var host, domain string
	var claim []string
	var clear, yes, dryRun bool
	cmd := &cobra.Command{
		Use:   "address [name]",
		Short: "Show or change where a service is reached",
		Long: `Without flags, show where a service is reached in staging and production.

With --host or --domain, set its address in Forge: it wins over forge.yaml's
run.host and run.domain until --clear goes back to them. --host @ is the
domain itself: production at https://<domain>, staging at
https://staging.<domain>. Nothing moves at once: staging moves at its next
deploy, production at its next promote.

This moves a public address, and the old one stops working. An address
that is in use already (it resolves in DNS) is taken over only when named
with --claim; its visitors then reach this service. You must type the
service's name to confirm, or pass --yes.`,
		Example: `  forge service address deogratias
  forge service address deogratias --host @ --domain deogratias.cloud --dry-run
  forge service address deogratias --host @ --domain deogratias.cloud --claim deogratias.cloud
  forge service address deogratias --clear`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, err := f.serviceName(args)
			if err != nil {
				return err
			}
			short := shortName(name)
			fl := cmd.Flags()
			change := fl.Changed("host") || fl.Changed("domain") || clear
			if !change && len(claim) > 0 {
				return exitcode.Usagef("--claim goes with --host or --domain")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			req := &forgev1.SetAddressRequest{Service: name, Host: host, Domain: domain, Clear: clear, Claim: claim}
			if !change {
				// Where it is now: the setting as it stands, planned without changing anything.
				svc, err := c.Catalog.GetService(ctx, connect.NewRequest(&forgev1.GetServiceRequest{Name: name}))
				if err != nil {
					return err
				}
				a := svc.Msg.GetService().GetAddress()
				if a == nil {
					f.infof("%s's address comes from its forge.yaml (run.host, run.domain).", short)
					req.Clear = true
				} else {
					f.infof("%s's address is set in Forge: host %q, domain %q.", short, a.GetHost(), a.GetDomain())
					req.Host, req.Domain = a.GetHost(), a.GetDomain()
				}
			}
			req.ValidateOnly = true
			plan, err := c.Deploys.SetAddress(ctx, connect.NewRequest(req))
			if !change && connect.CodeOf(err) == connect.CodePermissionDenied {
				return nil // planning is for writers; a reader sees the setting alone
			}
			if err != nil {
				return err
			}
			if err := printPlannedAddresses(f, format, plan.Msg); err != nil || !change {
				return err
			}
			if dryRun {
				f.infof("%s's address would change at the next deploy of each environment (dry run, nothing changed).", short)
				return nil
			}
			if err := f.confirmDestructive("change the address of", short, yes); err != nil {
				return err
			}
			req.ValidateOnly = false
			if _, err := c.Deploys.SetAddress(ctx, connect.NewRequest(req)); err != nil {
				return err
			}
			f.infof("%s's address is changed. Staging moves at its next deploy, production at its next promote.", short)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&host, "host", "", `the name before the domain; "@" for the domain itself`)
	fl.StringVar(&domain, "domain", "", `one of the platform's public domains, or "none"`)
	fl.StringSliceVar(&claim, "claim", nil, "an address in use to take over (repeatable)")
	fl.BoolVar(&clear, "clear", false, "go back to forge.yaml's run.host and run.domain")
	fl.BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&dryRun, "dry-run", false, "show where each environment would go without changing anything")
	return cmd
}

func printPlannedAddresses(f *Factory, format output.Format, plan *forgev1.SetAddressResponse) error {
	v := output.View{Headers: []string{"env", "address", "now", "in use"}, Data: plan}
	for _, p := range plan.GetAddresses() {
		now, inUse := p.GetCurrentUrl(), "-"
		if now == "" {
			now = "-"
		}
		if p.GetInUse() {
			inUse = fmt.Sprintf("yes: claim %s", p.GetHost())
		}
		v.Rows = append(v.Rows, []string{p.GetEnv(), p.GetUrl(), now, inUse})
		v.Names = append(v.Names, p.GetUrl())
	}
	return output.Print(f.IO.Out, format, v)
}
