package cli

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newTokenCmd(f *Factory) *cobra.Command {
	cmd := group("token", "Make, list and revoke personal tokens for CI and scripts", "tokens")
	cmd.Long = `A personal token (fgp_...) lets a script or a CI job call Forge as you. It
never has more than your role, only what its scopes allow, and it ends when it
expires, when you revoke it, or when you leave the organization. Only your
login makes or revokes one: a token cannot. Use it as FORGE_TOKEN.

Scopes: read (always), deploy (deploy, promote, roll back, previews, cancel)
and settings (every other change you may make).`
	cmd.AddCommand(newTokenCreateCmd(f), newTokenListCmd(f), newTokenRevokeCmd(f))
	return cmd
}

func newTokenCreateCmd(f *Factory) *cobra.Command {
	var scopes []string
	var expires string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Make a personal token",
		Long: `Make a personal token. The token is printed once, on its own line; Forge
keeps only its hash, so store it where it is used (a CI secret).

--expires is a number of days (7d, 30d, at most 365d) or never.`,
		Example: `  forge token create ci-deploys --scope deploy --expires 30d
  forge token create nightly-report --expires 7d
  FORGE_TOKEN=$(forge token create ci --scope deploy -q) && gh secret set FORGE_TOKEN --body "$FORGE_TOKEN"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			days, err := expiryDays(expires)
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Auth.CreateToken(ctx, connect.NewRequest(&forgev1.CreateTokenRequest{
				DisplayName: args[0], Scopes: scopes, ExpireDays: days, RequestId: client.NewRequestID(), ValidateOnly: dryRun,
			}))
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{resp.Msg.GetToken().GetName()}, Data: resp.Msg})
			}
			t := resp.Msg.GetToken()
			if dryRun {
				_, err = fmt.Fprintf(f.IO.Out, "%s may %s, %s (dry run, nothing made)\n", t.GetDisplayName(), strings.Join(t.GetScopes(), ", "), tokenExpiry(t))
				return err
			}
			f.infof("Token %s may %s, %s. It is shown once: store it now.", t.GetDisplayName(), strings.Join(t.GetScopes(), ", "), tokenExpiry(t))
			_, err = fmt.Fprintln(f.IO.Out, resp.Msg.GetSecret())
			return err
		},
	}
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "deploy or settings, besides read (repeatable)")
	cmd.Flags().StringVar(&expires, "expires", "30d", "days until it expires (1d to 365d), or never")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate on the server without making it")
	return cmd
}

// expiryDays reads --expires: "30d", "30" or "never".
func expiryDays(s string) (int32, error) {
	if s == "never" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n < 1 || n > 365 {
		return 0, exitcode.Usagef("--expires %q must be 1d to 365d, or never", s)
	}
	return int32(n), nil //nolint:gosec // checked above
}

// tokenExpiry says when a token ends: "expires 2026-12-10" or "never expires".
func tokenExpiry(t *forgev1.Token) string {
	if t.GetExpireTime() == nil {
		return "never expires"
	}
	return "expires " + t.GetExpireTime().AsTime().Local().Format("2006-01-02")
}

func newTokenListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your personal tokens",
		Example: `  forge token list
  forge token list -o json | jq -r '.tokens[].displayName'`,
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
			resp, err := c.Auth.ListTokens(ctx, connect.NewRequest(&forgev1.ListTokensRequest{}))
			if err != nil {
				return err
			}
			ts := resp.Msg.GetTokens()
			if len(ts) == 0 && format == output.Table {
				f.infof("No tokens. Make one with 'forge token create <name>'.")
				return nil
			}
			v := output.View{Headers: []string{"name", "scopes", "created", "last used", "expires"}, Data: resp.Msg}
			if len(ts) == 0 {
				v.Data = map[string]any{"tokens": []any{}}
			}
			for _, t := range ts {
				expires := "never"
				if t.GetExpireTime() != nil {
					expires = t.GetExpireTime().AsTime().Local().Format("2006-01-02")
				}
				v.Rows = append(v.Rows, []string{t.GetDisplayName(), strings.Join(t.GetScopes(), ","),
					t.GetCreateTime().AsTime().Local().Format("2006-01-02"), t.GetLastUseTime().AsTime().Local().Format("2006-01-02 15:04"), expires})
				v.Names = append(v.Names, t.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newTokenRevokeCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "revoke <name>",
		Short:   "Revoke one of your personal tokens at once",
		Example: `  forge token revoke ci-deploys`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			name := args[0]
			if !strings.HasPrefix(name, "tokens/") {
				list, err := c.Auth.ListTokens(ctx, connect.NewRequest(&forgev1.ListTokensRequest{}))
				if err != nil {
					return err
				}
				found := ""
				for _, t := range list.Msg.GetTokens() {
					if t.GetDisplayName() == name {
						found = t.GetName()
					}
				}
				if found == "" {
					return exitcode.New(exitcode.NotFound, "you have no token called %s (forge token list)", name)
				}
				name = found
			}
			if _, err := c.Auth.DeleteToken(ctx, connect.NewRequest(&forgev1.DeleteTokenRequest{Name: name})); err != nil {
				return err
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s revoked\n", args[0])
			return err
		},
	}
}
