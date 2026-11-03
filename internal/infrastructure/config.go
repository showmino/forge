// Package infrastructure is phase 12: repos for infrastructure code (Bicep,
// OpenTofu) that deploy themselves from GitHub Actions to a target, a set of
// cloud accounts the platform team set up. Forge writes what such a repo
// needs as Crossplane objects into the deploy repo: its GitHub environments,
// their secrets and variables, and the target's cloud access. It knows
// nothing about any cloud: what is cloud-specific is in the target's access
// template.
package infrastructure

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Environments are the environments an infrastructure repo may have, in
// deploy order.
var Environments = []string{"staging", "production"}

// Production is the environment whose runs wait for an approval in Forge.
const Production = "production"

// Config is the platform's infrastructure setting (the chart's
// infrastructure value, a file forge-api reads with --infrastructure-config).
type Config struct {
	// Namespace holds the Crossplane objects and the targets' Secrets.
	Namespace string `yaml:"namespace"`
	// ProviderConfig is the Crossplane GitHub provider's config the objects use.
	ProviderConfig ProviderConfig `yaml:"providerConfig"`
	// Workflow is the shared workflow ("<owner>/<repo>/<path>@<ref>") every
	// infrastructure repo calls, and the only one production's cloud access
	// accepts.
	Workflow string `yaml:"workflow"`
	// AccessKinds are the kinds the targets' access templates make, as
	// "<group>/<Kind>"; the deploy repo's AppProject allows them.
	AccessKinds []string `yaml:"accessKinds,omitempty"`
	Targets     []Target `yaml:"targets"`
}

// ProviderConfig names a Crossplane ProviderConfig or ClusterProviderConfig.
type ProviderConfig struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

// Target is a set of cloud accounts, one per environment.
type Target struct {
	Name  string `yaml:"name"`
	Title string `yaml:"title,omitempty"`
	// Teams may use the target; empty: every team.
	Teams        []string               `yaml:"teams,omitempty"`
	Environments map[string]Environment `yaml:"environments"`
}

// Environment is one environment of a target: one cloud account.
type Environment struct {
	// Secret is a Secret in Config.Namespace with the secret values.
	Secret string `yaml:"secret"`
	// GitHubSecrets maps an environment secret's name to its key in Secret.
	GitHubSecrets map[string]string `yaml:"githubSecrets,omitempty"`
	// GitHubVariables maps an environment variable's name to its value.
	GitHubVariables map[string]string `yaml:"githubVariables,omitempty"`
	// MaxRepos is how many repos the environment takes; 0 is no limit.
	MaxRepos int `yaml:"maxRepos,omitempty"`
	// Access is a Go template of the Crossplane objects that let one repo
	// sign in to the account (see AccessValues).
	Access string `yaml:"access,omitempty"`

	access *template.Template
}

// AccessValues are what an access template can use.
type AccessValues struct {
	Org, Repo, Team, Environment string
	// Subject is the OIDC subject of the repo's runs in this environment
	// through the shared workflow (see Subject).
	Subject string
}

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ghName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	secretKey  = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
	workflowRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_./-]+\.ya?ml@refs/(heads|tags)/[A-Za-z0-9_./-]+$`)
	kindRe     = regexp.MustCompile(`^[a-z0-9.-]+/[A-Z][A-Za-z0-9]*$`)
)

// Load reads and checks the config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads and checks a config.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) check() error {
	if !dnsLabel.MatchString(c.Namespace) {
		return fmt.Errorf("namespace %q must be a namespace's name", c.Namespace)
	}
	if c.ProviderConfig.Kind != "ProviderConfig" && c.ProviderConfig.Kind != "ClusterProviderConfig" {
		return fmt.Errorf("providerConfig.kind %q must be ProviderConfig or ClusterProviderConfig", c.ProviderConfig.Kind)
	}
	if !dnsLabel.MatchString(c.ProviderConfig.Name) {
		return fmt.Errorf("providerConfig.name %q must be a Kubernetes name", c.ProviderConfig.Name)
	}
	if !workflowRe.MatchString(c.Workflow) {
		return fmt.Errorf("workflow %q must be <owner>/<repo>/.github/workflows/<file>.yml@refs/heads/<branch> (or refs/tags/<tag>)", c.Workflow)
	}
	for _, k := range c.AccessKinds {
		if !kindRe.MatchString(k) {
			return fmt.Errorf("accessKinds %q must be <group>/<Kind>, such as managedidentity.azure.m.upbound.io/FederatedIdentityCredential", k)
		}
	}
	if len(c.Targets) == 0 {
		return errors.New("targets: name at least one")
	}
	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		if !dnsLabel.MatchString(t.Name) {
			return fmt.Errorf("targets[%d].name %q must be a lowercase DNS label", i, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("target %s is named twice", t.Name)
		}
		seen[t.Name] = true
		if len(t.Environments) == 0 {
			return fmt.Errorf("target %s: name at least one environment (%s)", t.Name, strings.Join(Environments, ", "))
		}
		for name, e := range t.Environments {
			if !slices.Contains(Environments, name) {
				return fmt.Errorf("target %s: environment %q must be one of %s", t.Name, name, strings.Join(Environments, ", "))
			}
			if err := e.check(); err != nil {
				return fmt.Errorf("target %s, %s: %w", t.Name, name, err)
			}
			t.Environments[name] = e
		}
	}
	return nil
}

func (e *Environment) check() error {
	if !dnsLabel.MatchString(e.Secret) {
		return fmt.Errorf("secret %q must be a Secret's name", e.Secret)
	}
	for name, key := range e.GitHubSecrets {
		if !ghName.MatchString(name) || strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
			return fmt.Errorf("githubSecrets %q is not a name GitHub allows (letters, digits and _, not starting with GITHUB_)", name)
		}
		if !secretKey.MatchString(key) {
			return fmt.Errorf("githubSecrets %s: %q is not a Secret's key", name, key)
		}
	}
	for name := range e.GitHubVariables {
		if !ghName.MatchString(name) || strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
			return fmt.Errorf("githubVariables %q is not a name GitHub allows (letters, digits and _, not starting with GITHUB_)", name)
		}
		if _, dup := e.GitHubSecrets[name]; dup {
			return fmt.Errorf("%s is both a secret and a variable", name)
		}
	}
	if e.MaxRepos < 0 {
		return fmt.Errorf("maxRepos %d must not be negative", e.MaxRepos)
	}
	if strings.TrimSpace(e.Access) != "" {
		t, err := template.New("access").Option("missingkey=error").Parse(e.Access)
		if err != nil {
			return fmt.Errorf("access: %w", err)
		}
		e.access = t
		// A sample repo shows a template that cannot render now, not at the
		// first create.
		if _, err := e.renderAccess(AccessValues{Org: "my-org", Repo: "sample", Team: "team", Environment: "staging", Subject: "repo:my-org/sample:environment:staging"}, nil); err != nil {
			return fmt.Errorf("access: %w", err)
		}
	}
	return nil
}

// Target returns the target called name.
func (c *Config) Target(name string) (Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// EnvironmentNames are the target's environments, in deploy order.
func (t Target) EnvironmentNames() []string {
	var out []string
	for _, e := range Environments {
		if _, ok := t.Environments[e]; ok {
			out = append(out, e)
		}
	}
	return out
}

// Allows reports whether team may use the target.
func (t Target) Allows(team string) bool {
	return len(t.Teams) == 0 || slices.Contains(t.Teams, team)
}

// Subject is the OIDC subject GitHub gives a job of org/repo in env once the
// repo's subject claim includes the workflow (SubjectClaims): only runs of
// the shared workflow carry it.
func (c *Config) Subject(org, repo, env string) string {
	return "repo:" + org + "/" + repo + ":environment:" + env + ":job_workflow_ref:" + c.Workflow
}

// SubjectClaims are the claims, in order, of an infrastructure repo's
// customized OIDC subject.
var SubjectClaims = []string{"repo", "context", "job_workflow_ref"}
