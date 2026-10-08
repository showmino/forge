package cli

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
}

func newDoctorCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check config, API reachability, version skew and login",
		Example: `  forge doctor
  forge doctor --context prod -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// An invalid forge.yaml is one of the checks, not a reason to stop.
			r, err := f.Resolved()
			if _, _, manifestErr := f.Manifest(); err != nil && !errors.Is(err, manifestErr) {
				return err
			}
			format, err := parseFormat(r)
			if err != nil {
				return err
			}
			checks := f.runChecks(cmd)
			v := output.View{Headers: []string{"check", "status", "detail"}, Data: map[string]any{"checks": checks}}
			failed := 0
			for _, c := range checks {
				v.Rows = append(v.Rows, []string{c.Name, c.Status, c.Detail})
				v.Names = append(v.Names, c.Name)
				if c.Status == "fail" {
					failed++
				}
			}
			if err := output.Print(f.IO.Out, format, v); err != nil {
				return err
			}
			if failed > 0 {
				return exitcode.New(exitcode.Unexpected, "%d check(s) failed", failed)
			}
			return nil
		},
	}
}

func (f *Factory) runChecks(cmd *cobra.Command) []check {
	var out []check
	add := func(name, status, format string, args ...any) {
		out = append(out, check{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	}

	_, path, err := f.Config()
	if err != nil {
		add("config", "fail", "%v", err)
		return out
	}
	add("config", "ok", "%s", path)

	if m, at, err := f.Manifest(); err != nil {
		add("manifest", "fail", "%v", err)
		return out // every later check needs a valid forge.yaml
	} else if m != nil {
		add("manifest", "ok", "%s (service %s)", at, m.Service.Name)
	} else {
		add("manifest", "skip", "no forge.yaml in this directory or its parents")
	}

	r, _ := f.Resolved()
	if r.APIURL == "" {
		add("context", "fail", "no context selected; run 'forge context set <name> --api <url> --use'")
		return out
	}
	add("context", "ok", "%s -> %s (%s)", orDash(r.ContextName), r.APIURL, r.Sources["api"])

	ctx, cancel := f.withTimeout(cmd.Context())
	defer cancel()
	c, err := f.Client(ctx, false)
	if err != nil {
		add("api", "fail", "%v", err)
		return out
	}
	info, err := c.Auth.GetServerInfo(ctx, connect.NewRequest(&forgev1.GetServerInfoRequest{}))
	if err != nil {
		add("api", "fail", "%s unreachable: %v", r.APIURL, err)
		return out
	}
	add("api", "ok", "reachable, server %s", info.Msg.GetServerVersion())

	if msg := skewWarning(f.Version, info.Msg.GetServerVersion(), info.Msg.GetMinClientVersion()); msg != "" {
		add("version", "warn", "%s", msg)
	} else {
		add("version", "ok", "client %s, server %s", f.Version, info.Msg.GetServerVersion())
	}

	if oidc := info.Msg.GetOidc(); oidc.GetIssuer() != "" {
		add("identity provider", "ok", "%s (client %s)", oidc.GetIssuer(), oidc.GetClientId())
	} else {
		add("identity provider", "warn", "the API advertises no OIDC issuer; 'forge auth login' will not work")
	}

	ac, err := f.Client(ctx, true)
	if err != nil {
		add("auth", "fail", "%v", err)
		return out
	}
	st, err := ac.Auth.GetStatus(ctx, connect.NewRequest(&forgev1.GetStatusRequest{}))
	if err != nil {
		e := exitcode.From(err)
		detail := e.Error()
		if e.Hint != "" {
			detail += "; " + e.Hint
		}
		add("auth", "fail", "%s", detail)
		return out
	}
	who := st.Msg.GetEmail()
	if who == "" {
		who = st.Msg.GetSubject()
	}
	add("auth", "ok", "logged in as %s", who)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "(flags/env)"
	}
	return s
}
