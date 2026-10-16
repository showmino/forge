package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
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

A login becomes a Forge session: you stay logged in until the session has
not been used for 90 days, you log out, or you leave the organization.
'forge auth session list' shows your sessions, on this machine and others.

Credentials are stored in the OS keychain (a 0600 file on headless Linux),
keyed by API URL. In CI, set FORGE_TOKEN instead of logging in.`
	cmd.AddCommand(newAuthLoginCmd(f), newAuthLogoutCmd(f), newAuthStatusCmd(f), newAuthCanCmd(f), newAuthTokenCmd(f), newAuthSessionCmd(f))
	return cmd
}

// bearer returns req with token as its bearer token, for the calls made
// with a token that is not the stored one.
func bearer[T any](token string, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

// startSession turns the login in creds into a Forge session, which lasts
// without the identity provider. An API that keeps no sessions leaves creds
// as they are; so does one that refuses, with the reason in a warning.
func (f *Factory) startSession(ctx context.Context, api interface {
	CreateSession(context.Context, *connect.Request[forgev1.CreateSessionRequest]) (*connect.Response[forgev1.CreateSessionResponse], error)
}, creds *auth.Credentials) {
	host, _ := os.Hostname()
	resp, err := api.CreateSession(ctx, bearer(creds.IDToken, &forgev1.CreateSessionRequest{Label: host}))
	switch {
	case connect.CodeOf(err) == connect.CodeUnimplemented:
		return
	case err != nil:
		f.infof("warning: no long-lived session (%v); this login lasts as long as the identity provider's token, about an hour.", exitcode.From(err))
		return
	}
	creds.SessionToken = resp.Msg.GetToken()
	creds.IDToken, creds.RefreshToken = "", ""
	creds.Expiry = resp.Msg.GetSession().GetExpireTime().AsTime()
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
			if info.Msg.GetSessions() {
				// The login is traded for a session at once: a refresh token
				// would only be a second, unused way in.
				cfg.Scopes = slices.DeleteFunc(slices.Clone(cfg.Scopes), func(s string) bool { return s == "offline_access" })
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
				return &exitcode.Error{Code: exitcode.Unauthenticated, Err: err}
			}
			f.startSession(ctx, c.Auth, creds)
			// A new login replaces the one this machine had: end that
			// session rather than leave it to its 90 days.
			if old, err := f.CredentialStore().Get(r.CredentialKey()); err == nil && old.SessionToken != "" && creds.SessionToken != "" {
				_, _ = c.Auth.DeleteSession(ctx, bearer(old.SessionToken, &forgev1.DeleteSessionRequest{Name: "sessions/current"}))
			}
			if err := f.CredentialStore().Set(r.CredentialKey(), creds); err != nil {
				return fmt.Errorf("save credentials to %s: %w", f.CredentialStore().Name(), err)
			}
			who := creds.Email
			if who == "" {
				who = creds.Subject
			}
			if creds.SessionToken != "" {
				f.infof("Logged in to %s as %s. You stay logged in until 90 days without use, or 'forge auth logout'.", c.BaseURL, who)
				return nil
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
		Short: "End the session and remove stored credentials for the current context",
		Example: `  forge auth logout
  forge auth logout --context prod`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := f.APIURL()
			if err != nil {
				return err
			}
			// End the session on the API too, so the token is worth nothing
			// to anyone who copied it. The local logout does not wait on it.
			ended := true
			if creds, err := f.CredentialStore().Get(url); err == nil && creds.SessionToken != "" {
				ctx, cancel := f.withTimeout(cmd.Context())
				defer cancel()
				ended = false
				if c, err := f.Client(ctx, false); err == nil {
					_, err = c.Auth.DeleteSession(ctx, bearer(creds.SessionToken, &forgev1.DeleteSessionRequest{Name: "sessions/current"}))
					// An API that no longer knows the session has nothing to end.
					ended = err == nil || connect.CodeOf(err) == connect.CodeUnauthenticated
				}
			}
			if err := f.CredentialStore().Delete(url); err != nil {
				return err
			}
			if !ended {
				f.infof("Logged out of %s on this machine. The API could not be reached to end the session; it ends on its own after 90 days without use.", url)
				return nil
			}
			f.infof("Logged out of %s.", url)
			return nil
		},
	}
}

type authStatus struct {
	API         string   `json:"api"`
	Subject     string   `json:"subject"`
	Email       string   `json:"email,omitempty"`
	Groups      []string `json:"groups"`
	Permissions []string `json:"permissions"`
	// Admin is true for a member of an admin group; Teams are the
	// organization's teams the caller is in.
	Admin       bool      `json:"admin"`
	Teams       []string  `json:"teams"`
	Credentials string    `json:"credentials"`
	Expiry      time.Time `json:"expiry,omitzero"`
	// Session is the Forge session in use, "sessions/<id>"; empty for a
	// login the identity provider keeps.
	Session string `json:"session,omitempty"`
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
				Admin:       resp.Msg.GetAdmin(),
				Teams:       nonNil(resp.Msg.GetTeams()),
				Credentials: f.CredentialStore().Name(),
			}
			if r.Token != "" {
				st.Credentials = "FORGE_TOKEN"
			} else if creds, err := f.CredentialStore().Get(r.CredentialKey()); err == nil {
				st.Expiry = creds.Expiry
			}
			rows := [][]string{
				{"api", st.API},
				{"subject", st.Subject},
				{"email", st.Email},
				{"groups", strings.Join(st.Groups, ", ")},
				{"permissions", strings.Join(st.Permissions, ", ")},
				{"role", roleString(st.Admin, st.Teams)},
				{"credentials", st.Credentials},
			}
			if ses := resp.Msg.GetSession(); ses != nil {
				// The API knows when the session ends; the stored copy is from the login.
				st.Session, st.Expiry = ses.GetName(), ses.GetExpireTime().AsTime()
				rows = append(rows, []string{"session", st.Session}, []string{"session ends", st.Expiry.Local().Format(time.RFC3339) + sessionEnds(ses)})
				return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows, Names: []string{st.Subject}, Data: st})
			}
			expires := "-"
			if !st.Expiry.IsZero() {
				expires = st.Expiry.Local().Format(time.RFC3339)
			}
			rows = append(rows, []string{"token expires", expires})
			return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows, Names: []string{st.Subject}, Data: st})
		},
	}
}

// roleString says what the caller is: an admin, a member of teams, or a
// reader.
func roleString(admin bool, teams []string) string {
	switch {
	case admin:
		return "admin (everything, on every service)"
	case len(teams) > 0:
		return "team: " + strings.Join(teams, ", ")
	default:
		return "reader"
	}
}

type authCan struct {
	Action  string `json:"action"`
	Service string `json:"service,omitempty"`
	Team    string `json:"team,omitempty"`
	Allowed bool   `json:"allowed"`
	// Role is the caller's role toward the service: admin, team or reader.
	Role string `json:"role"`
}

func newAuthCanCmd(f *Factory) *cobra.Command {
	var team string
	cmd := &cobra.Command{
		Use:   "can <action> [service]",
		Short: "Check whether you may do an action",
		Long: `Ask the API whether you may do an action, on a service or (for
service.create and service.register) on none. Prints yes or no and exits 0 or
5, so scripts can check before they try.

Members of a service's owner team may update it, deploy, promote and roll it
back, manage its environments and plan and apply its resources outside
production. Admins may do everything. The actions:

  service.create, service.create.any_team, service.register, service.update,
  service.update.team, service.delete, service.address, deploy.start,
  deploy.promote, deploy.rollback, environment.create, environment.extend,
  environment.destroy, operation.cancel, resource.plan, resource.apply,
  resource.apply.production`,
		Example: `  forge auth can deploy.promote payments
  forge auth can service.create --team billing
  forge auth can resource.apply.production payments -o json`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			can := authCan{Action: args[0], Team: team}
			if len(args) == 2 {
				if can.Service, err = f.serviceName(args[1:]); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Auth.TestPermissions(ctx, connect.NewRequest(&forgev1.TestPermissionsRequest{
				Service: can.Service, Actions: []string{can.Action}, OwnerTeam: team,
			}))
			if err != nil {
				return err
			}
			can.Allowed = len(resp.Msg.GetAllowed()) == 1
			can.Role = resp.Msg.GetRole()
			answer := "no"
			if can.Allowed {
				answer = "yes"
			}
			// The table is the bare answer, for 'if forge auth can ...' in a shell.
			if format == output.Table {
				_, err = fmt.Fprintln(f.IO.Out, answer)
			} else {
				err = output.Print(f.IO.Out, format, output.View{Names: []string{answer}, Data: can})
			}
			if err != nil {
				return err
			}
			if can.Allowed {
				return nil
			}
			on := ""
			if can.Service != "" {
				on = " on " + can.Service
			} else if team != "" {
				on = " for the team " + team
			}
			return exitcode.New(exitcode.PermissionDenied, "you may not %s%s (your role: %s)", can.Action, on, can.Role)
		},
	}
	cmd.Flags().StringVar(&team, "team", "", "for service.create: the team the new service would belong to")
	return cmd
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

// sessionEnds says what a session's end time means for its kind.
func sessionEnds(s *forgev1.Session) string {
	if s.GetClient() == forgev1.Session_CLIENT_CLI {
		return " (if not used before then)"
	}
	return ""
}

func newAuthSessionCmd(f *Factory) *cobra.Command {
	cmd := group("session", "List and end your Forge sessions", "sessions")
	cmd.Long = `A session is one login: a machine where you ran 'forge auth login', or a
browser signed in to the dashboard. End one you no longer trust or use.`
	list := &cobra.Command{
		Use:   "list",
		Short: "List your sessions, newest first",
		Example: `  forge auth session list
  forge auth session list -o json`,
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
			resp, err := c.Auth.ListSessions(ctx, connect.NewRequest(&forgev1.ListSessionsRequest{}))
			if err != nil {
				return err
			}
			ss := resp.Msg.GetSessions()
			if len(ss) == 0 && format == output.Table {
				f.infof("No sessions.")
				return nil
			}
			v := output.View{Headers: []string{"session", "client", "where", "created", "last used", "ends in"}, Data: resp.Msg}
			if len(ss) == 0 {
				v.Data = map[string]any{"sessions": []any{}}
			}
			for _, s := range ss {
				id := strings.TrimPrefix(s.GetName(), "sessions/")
				if s.GetCurrent() {
					id += " (this one)"
				}
				v.Rows = append(v.Rows, []string{
					id, lowerEnum(s.GetClient(), "CLIENT_"), s.GetLabel(),
					age(f.Now().Sub(s.GetCreateTime().AsTime())) + " ago", age(f.Now().Sub(s.GetLastUseTime().AsTime())) + " ago",
					age(s.GetExpireTime().AsTime().Sub(f.Now())),
				})
				v.Names = append(v.Names, s.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	revoke := &cobra.Command{
		Use:     "revoke <session>",
		Short:   "End one of your sessions at once",
		Long:    "End one of your sessions at once: its token stops working. To end the session of this machine, use 'forge auth logout'.",
		Example: `  forge auth session revoke ses_4f2a9c1d0b3e5a67`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			name := "sessions/" + strings.TrimPrefix(args[0], "sessions/")
			if _, err := c.Auth.DeleteSession(ctx, connect.NewRequest(&forgev1.DeleteSessionRequest{Name: name})); err != nil {
				return err
			}
			f.infof("Ended %s.", name)
			return nil
		},
	}
	cmd.AddCommand(list, revoke)
	return cmd
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
