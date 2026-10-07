package cli

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/auth"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newAuthCmd(f *Factory) *cobra.Command {
	cmd := group("auth", "Log in to Forge and inspect credentials")
	cmd.Long = `Log in with your identity provider using the OIDC device flow. It works over
SSH and in containers because it needs no local browser callback.

Credentials are stored in the OS keychain (a 0600 file on headless Linux),
keyed by API URL. In CI, set FORGE_TOKEN instead of logging in.`
	cmd.AddCommand(newAuthLoginCmd(f), newAuthLogoutCmd(f), newAuthStatusCmd(f), newAuthTokenCmd(f))
	return cmd
}

func newAuthLoginCmd(f *Factory) *cobra.Command {
	var issuer, clientID string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in with the OIDC device flow",
		Long: `Log in with the OIDC device flow. forge asks the API which identity provider
to use, prints a code and a URL, and waits while you approve the login in any
browser.`,
		Example: `  forge auth login
  forge auth login --context prod`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := f.Resolved()
			if err != nil {
				return err
			}
			if r.Token != "" {
				return exitcode.Usagef("FORGE_TOKEN is set; unset it to log in interactively")
			}
			ctx := cmd.Context()
			if f.flags.timeout > 0 {
				var cancel func()
				ctx, cancel = f.withTimeout(ctx)
				defer cancel()
			}
			c, err := f.Client(ctx, false)
			if err != nil {
				return err
			}
			info, err := c.Auth.GetServerInfo(ctx, connect.NewRequest(&forgev1.GetServerInfoRequest{}))
			if err != nil {
				return fmt.Errorf("ask %s how to log in: %w", c.BaseURL, err)
			}
			cfg := auth.Config{
				Issuer:     info.Msg.GetOidc().GetIssuer(),
				ClientID:   info.Msg.GetOidc().GetClientId(),
				Scopes:     info.Msg.GetOidc().GetScopes(),
				HTTPClient: f.HTTPClient,
			}
			if issuer != "" {
				cfg.Issuer = issuer
			}
			if clientID != "" {
				cfg.ClientID = clientID
			}
			if cfg.Issuer == "" || cfg.ClientID == "" {
				return exitcode.New(exitcode.Unexpected, "the Forge API at %s has no identity provider configured", c.BaseURL)
			}

			creds, err := auth.DeviceLogin(ctx, cfg, func(d auth.DeviceCode) {
				w := f.IO.ErrOut
				fmt.Fprintf(w, "To log in, open this URL in a browser:\n\n  %s\n\n", d.VerificationURI)
				fmt.Fprintf(w, "and enter the code:  %s\n\n", d.UserCode)
				if d.VerificationURIComplete != "" {
					fmt.Fprintf(w, "Or open %s\n\n", d.VerificationURIComplete)
				}
				fmt.Fprintf(w, "Waiting for approval (expires %s)...\n", d.ExpiresAt.Local().Format(time.Kitchen))
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return exitcode.New(exitcode.Unauthenticated, "%v", err)
			}
			if err := f.CredentialStore().Set(r.CredentialKey(), creds); err != nil {
				return fmt.Errorf("save credentials to %s: %w", f.CredentialStore().Name(), err)
			}
			who := creds.Email
			if who == "" {
				who = creds.Subject
			}
			f.infof("Logged in to %s as %s.", c.BaseURL, who)
			return nil
		},
	}
	cmd.Flags().StringVar(&issuer, "issuer", "", "override the OIDC issuer the API advertises")
	cmd.Flags().StringVar(&clientID, "client-id", "", "override the OIDC client ID the API advertises")
	_ = cmd.Flags().MarkHidden("issuer")
	_ = cmd.Flags().MarkHidden("client-id")
	return cmd
}

func newAuthLogoutCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials for the current context",
		Example: `  forge auth logout
  forge auth logout --context prod`,
		Args: noArgs,
		RunE: func(*cobra.Command, []string) error {
			url, err := f.APIURL()
			if err != nil {
				return err
			}
			if err := f.CredentialStore().Delete(url); err != nil {
				return err
			}
			f.infof("Logged out of %s.", url)
			return nil
		},
	}
}

type authStatus struct {
	API         string    `json:"api"`
	Subject     string    `json:"subject"`
	Email       string    `json:"email,omitempty"`
	Groups      []string  `json:"groups"`
	Permissions []string  `json:"permissions"`
	Credentials string    `json:"credentials"`
	Expiry      time.Time `json:"expiry,omitzero"`
}

func newAuthStatusCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show who you are logged in as and what you may do",
		Example: `  forge auth status
  forge auth status -o json | jq -r .permissions[]`,
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
			resp, err := c.Auth.GetStatus(ctx, connect.NewRequest(&forgev1.GetStatusRequest{}))
			if err != nil {
				return err
			}
			r, _ := f.Resolved()
			st := authStatus{
				API:         c.BaseURL,
				Subject:     resp.Msg.GetSubject(),
				Email:       resp.Msg.GetEmail(),
				Groups:      nonNil(resp.Msg.GetGroups()),
				Permissions: nonNil(resp.Msg.GetPermissions()),
				Credentials: f.CredentialStore().Name(),
			}
			if r.Token != "" {
				st.Credentials = "FORGE_TOKEN"
			} else if creds, err := f.CredentialStore().Get(r.CredentialKey()); err == nil {
				st.Expiry = creds.Expiry
			}
			expires := "-"
			if !st.Expiry.IsZero() {
				expires = st.Expiry.Local().Format(time.RFC3339)
			}
			return output.Print(f.IO.Out, format, output.View{
				Headers: []string{"field", "value"},
				Rows: [][]string{
					{"api", st.API},
					{"subject", st.Subject},
					{"email", st.Email},
					{"groups", strings.Join(st.Groups, ", ")},
					{"permissions", strings.Join(st.Permissions, ", ")},
					{"credentials", st.Credentials},
					{"token expires", expires},
				},
				Names: []string{st.Subject},
				Data:  st,
			})
		},
	}
}

func newAuthTokenCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Print a valid bearer token for the current context",
		Long:  "Print a valid bearer token, refreshing it first if needed. Useful with curl.",
		Example: `  curl -H "Authorization: Bearer $(forge auth token)" \
    https://forge.example.com/forge.v1.AuthService/GetStatus -H 'Content-Type: application/json' -d '{}'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := f.APIURL(); err != nil {
				return err
			}
			ts, err := f.TokenSource(cmd.Context())
			if err != nil {
				return err
			}
			tok, err := ts.Token()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(f.IO.Out, tok.AccessToken)
			return err
		},
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
