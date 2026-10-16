package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/output"
)

func newDomainCmd(f *Factory) *cobra.Command {
	cmd := group("domain", "List the public domains a service can be reached under", "domains")
	cmd.Long = `A public service is reached under one of the platform's domains: the default,
or the one 'forge create --domain' or forge.yaml's run.domain names. The
platform's operators choose the domains when they deploy the Forge API.`
	cmd.AddCommand(newDomainListCmd(f))
	return cmd
}

func newDomainListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the public domains, the default first",
		Long:    "List the public domains services can be reached under, the default first.",
		Example: `  forge domain list
  forge domain list -o name`,
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
			resp, err := c.Deploys.ListDomains(ctx, connect.NewRequest(&forgev1.ListDomainsRequest{}))
			if err != nil {
				return err
			}
			ds := resp.Msg.GetDomains()
			if len(ds) == 0 && format == output.Table {
				f.infof("This Forge API offers no public domains: services get internal addresses only.")
				return nil
			}
			v := output.View{Headers: []string{"domain", "default"}, Data: resp.Msg}
			if len(ds) == 0 {
				// protojson omits empty lists; keep '.domains[]' working in jq.
				v.Data = map[string]any{"domains": []any{}}
			}
			for _, d := range ds {
				def := ""
				if d.GetIsDefault() {
					def = "yes"
				}
				v.Rows = append(v.Rows, []string{d.GetName(), def})
				v.Names = append(v.Names, d.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}
