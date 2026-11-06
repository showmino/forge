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
	"io"
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
	// RepoWorkflow is the workflow file of each infrastructure repo (under
	// .github/workflows) that Forge starts, with the input approval, once
	// production is approved; default deploy.yml.
	RepoWorkflow string `yaml:"repoWorkflow,omitempty"`
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
	Teams []string `yaml:"teams,omitempty"`
	// Objects says who makes an environment's objects: "forge" (the
	// default) writes the GitHub environment, its production branch rule,
	// the secrets, the variables and the plan settings itself, next to the
	// access template's objects; "template" writes only what the access
	// template makes, for example one instance of a kro
	// ResourceGraphDefinition that makes all of them. With "template" the
	// template is responsible for production's main-only branch rule and
	// for a separate plan identity.
	Objects      string                 `yaml:"objects,omitempty"`
	Environments map[string]Environment `yaml:"environments"`
}

// The values of Target.Objects.
const (
	ObjectsForge    = "forge"
	ObjectsTemplate = "template"
)

// TemplateObjects reports whether the target's access templates make every
// object of an environment (objects: template).
func (t Target) TemplateObjects() bool { return t.Objects == ObjectsTemplate }

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
	// Plan is a separate identity for unreviewed PR code, never the deploy identity.
	Plan *PlanAccess `yaml:"plan,omitempty"`

	access *template.Template
}

// PlanAccess provides repository-level settings and cloud trust for PR plans.
// It makes no GitHub environment.
type PlanAccess struct {
	Secret          string            `yaml:"secret"`
	GitHubSecrets   map[string]string `yaml:"githubSecrets,omitempty"`
	GitHubVariables map[string]string `yaml:"githubVariables,omitempty"`
	Access          string            `yaml:"access,omitempty"`
	access          *template.Template
}

func (p *PlanAccess) environment() Environment {
	return Environment{
		Secret: p.Secret, GitHubSecrets: p.GitHubSecrets,
		GitHubVariables: p.GitHubVariables, Access: p.Access, access: p.access,
	}
}

// AccessValues are what an access template can use.
type AccessValues struct {
	Org, Repo, Team, Environment string
	RepoID                       int64
	// Subject is the OIDC subject of the repo's runs in this environment
	// through the shared workflow (see Subject).
	Subject string
	// PlanSubject is the OIDC subject of the repo's pull-request plan jobs.
	PlanSubject string
}

var (
	dnsLabel       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ghName         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	secretKey      = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
	workflowRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_./-]+\.ya?ml@refs/(heads|tags)/[A-Za-z0-9_./-]+$`)
	repoWorkflowRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.ya?ml$`)
	kindRe         = regexp.MustCompile(`^[a-z0-9.-]+/[A-Z][A-Za-z0-9]*$`)
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
	if c.RepoWorkflow != "" && !repoWorkflowRe.MatchString(c.RepoWorkflow) {
		return fmt.Errorf("repoWorkflow %q must be a file name such as deploy.yml", c.RepoWorkflow)
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
		if t.Objects != "" && t.Objects != ObjectsForge && t.Objects != ObjectsTemplate {
			return fmt.Errorf("target %s: objects %q must be %s or %s", t.Name, t.Objects, ObjectsForge, ObjectsTemplate)
		}
		for name, e := range t.Environments {
			if !slices.Contains(Environments, name) {
				return fmt.Errorf("target %s: environment %q must be one of %s", t.Name, name, strings.Join(Environments, ", "))
			}
			if t.TemplateObjects() {
				if err := e.checkTemplateObjects(); err != nil {
					return fmt.Errorf("target %s, %s: %w", t.Name, name, err)
				}
			}
			if err := e.check(c.AccessKinds, t.TemplateObjects()); err != nil {
				return fmt.Errorf("target %s, %s: %w", t.Name, name, err)
			}
			t.Environments[name] = e
		}
	}
	return nil
}

// checkKinds checks that every object of docs is of a kind in kinds
// ("<group>/<Kind>"): the AppProject allows only those.
func checkKinds(docs []byte, kinds []string) error {
	dec := yaml.NewDecoder(bytes.NewReader(docs))
	for {
		var d struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		group, _, _ := strings.Cut(d.APIVersion, "/")
		if !strings.Contains(d.APIVersion, "/") {
			group = "" // the core group: v1
		}
		if k := group + "/" + d.Kind; !slices.Contains(kinds, k) {
			return fmt.Errorf("makes a %s, which accessKinds does not list (add %s)", d.Kind, k)
		}
	}
}

// checkTemplateObjects refuses the settings Forge would ignore when the
// access template makes every object (objects: template).
func (e *Environment) checkTemplateObjects() error {
	const why = "with objects: template the access template makes them"
	switch {
	case strings.TrimSpace(e.Access) == "":
		return errors.New("access: with objects: template it makes every object of the environment, so it must not be empty")
	case len(e.GitHubSecrets) > 0:
		return errors.New("githubSecrets: " + why)
	case len(e.GitHubVariables) > 0:
		return errors.New("githubVariables: " + why)
	case e.Plan != nil:
		return errors.New("plan: with objects: template the access template makes the plan access")
	}
	return nil
}

// check checks the environment. secretOptional: with objects: template
// Forge itself reads no Secret, so the template may name its own.
func (e *Environment) check(kinds []string, secretOptional bool) error {
	if (!secretOptional || e.Secret != "") && !dnsLabel.MatchString(e.Secret) {
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
		t, err := template.New("access").
			Funcs(template.FuncMap{"upper": strings.ToUpper}).
			Option("missingkey=error").
			Parse(e.Access)
		if err != nil {
			return fmt.Errorf("access: %w", err)
		}
		e.access = t
		// A sample repo shows a template that cannot render now, not at the
		// first create.
		sample, err := e.renderAccess(AccessValues{
			Org: "my-org", Repo: "sample", Team: "team", Environment: "staging",
			RepoID:      1,
			Subject:     "repo:my-org/sample:environment:staging",
			PlanSubject: "job_workflow_ref:my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main:repo:my-org/sample:pull_request",
		}, nil)
		if err != nil {
			return fmt.Errorf("access: %w", err)
		}
		if err := checkKinds(sample, kinds); err != nil {
			return fmt.Errorf("access: %w", err)
		}
	}
	if e.Plan != nil {
		if e.Plan.Secret == e.Secret {
			return errors.New("plan.secret must differ from the deployment secret: PRs use a separate identity")
		}
		p := e.Plan.environment()
		if err := p.check(kinds, false); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
		e.Plan.access = p.access
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

// Subject is the OIDC subject GitHub gives a job of r in env once the repo's
// subject claims are SubjectClaims, with immutable identifiers: only runs of
// the shared workflow carry it, and a repo deleted and made again under the
// same name gets another (its ID changes).
func (c *Config) Subject(r Repo, env string) string {
	return c.subject(r, "environment:"+env)
}

// PlanSubject trusts only PR jobs of the shared workflow, without an environment.
func (c *Config) PlanSubject(r Repo) string {
	return c.subject(r, "pull_request")
}

func (c *Config) subject(r Repo, context string) string {
	repo := r.Org + "/" + r.Name
	if r.OwnerID != 0 && r.RepoID != 0 {
		repo = fmt.Sprintf("%s@%d/%s@%d", r.Org, r.OwnerID, r.Name, r.RepoID)
	}
	return "job_workflow_ref:" + c.Workflow + ":repo:" + repo + ":" + context
}

// SubjectClaims are the claims, in order, of an infrastructure repo's
// customized OIDC subject. The workflow comes first: GitHub's default
// subject starts with "repo:", so if the repo's setting were ever reset, no
// token, whatever its environment's name, could match the subject a cloud
// trusts.
var SubjectClaims = []string{"job_workflow_ref", "repo", "context"}

// RepoWorkflowFile is RepoWorkflow, or its default.
func (c *Config) RepoWorkflowFile() string {
	if c.RepoWorkflow == "" {
		return "deploy.yml"
	}
	return c.RepoWorkflow
}
