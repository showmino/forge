package config

import (
	"errors"
	"fmt"
)

// Overrides are values set by command-line flags. Empty means "not set".
type Overrides struct {
	Context   string
	APIURL    string
	Namespace string
	Output    string
}

// Defaults are the lowest-precedence layers below the user config: the repo
// manifest and the built-in values.
type Defaults struct {
	Namespace string // from forge.yaml
	Output    string // built-in
	APIURL    string // built-in; set in release builds
}

// Resolved is the effective configuration for one command.
type Resolved struct {
	ContextName string
	APIURL      string
	Namespace   string
	Output      string
	// Token is a FORGE_TOKEN override. When set, stored credentials are not used.
	Token string
	// Source of each value, for 'forge doctor' and -v output.
	Sources map[string]string
}

// ErrNoContext means no context is selected and no API URL was given.
var ErrNoContext = errors.New("no Forge context selected")

// Resolve applies precedence: flags > FORGE_* env > manifest > user config > defaults.
// It does not fail when no API URL is known; callers that need one check APIURL.
func Resolve(c *Config, env Env, o Overrides, d Defaults) (Resolved, error) {
	r := Resolved{Sources: map[string]string{}}
	get := func(key string) string {
		v, _ := env(key)
		return v
	}

	// Which context.
	switch {
	case o.Context != "":
		r.ContextName, r.Sources["context"] = o.Context, "flag --context"
	case get("FORGE_CONTEXT") != "":
		r.ContextName, r.Sources["context"] = get("FORGE_CONTEXT"), "env FORGE_CONTEXT"
	case c.CurrentContext != "":
		r.ContextName, r.Sources["context"] = c.CurrentContext, "config current-context"
	}
	var ctx Context
	if r.ContextName != "" {
		var ok bool
		ctx, ok = c.Context(r.ContextName)
		if !ok {
			return r, fmt.Errorf("context %q is not defined; run 'forge context list'", r.ContextName)
		}
	}

	pick := func(field string, layers ...[2]string) string {
		for _, l := range layers {
			if l[0] != "" {
				r.Sources[field] = l[1]
				return l[0]
			}
		}
		return ""
	}
	r.APIURL = pick("api",
		[2]string{o.APIURL, "flag --api-url"},
		[2]string{get("FORGE_API_URL"), "env FORGE_API_URL"},
		[2]string{ctx.API, "context " + ctx.Name},
		[2]string{d.APIURL, "built-in"},
	)
	r.Namespace = pick("namespace",
		[2]string{o.Namespace, "flag --namespace"},
		[2]string{get("FORGE_NAMESPACE"), "env FORGE_NAMESPACE"},
		[2]string{d.Namespace, "forge.yaml"},
		[2]string{ctx.Namespace, "context " + ctx.Name},
	)
	r.Output = pick("output",
		[2]string{o.Output, "flag --output"},
		[2]string{get("FORGE_OUTPUT"), "env FORGE_OUTPUT"},
		[2]string{c.Preferences.Output, "config preferences"},
		[2]string{d.Output, "default"},
	)
	if t := get("FORGE_TOKEN"); t != "" {
		r.Token, r.Sources["token"] = t, "env FORGE_TOKEN"
	}
	return r, nil
}

// CredentialKey identifies stored credentials. Contexts that share an API share
// a login, so switching between namespaces does not force a new login.
func (r Resolved) CredentialKey() string { return r.APIURL }
