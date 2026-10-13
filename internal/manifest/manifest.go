// Package manifest reads and writes forge.yaml, the per-repo source of truth
// for a service's catalog entry. Phase 1 covers the catalog fields; Phase 3
// adds run, how a preview environment runs the service; Phase 5 adds
// resources; Phase 7 adds docs, the order of the service's doc pages.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/docs"
	"github.com/showmino/forge/internal/resource"
)

// FileName is the manifest's file name.
const FileName = "forge.yaml"

// APIVersion is the manifest schema version.
const APIVersion = "forge.dev/v1"

// Manifest is the content of forge.yaml.
type Manifest struct {
	APIVersion string  `yaml:"apiVersion"`
	Service    Service `yaml:"service"`
	// Namespace is the default namespace for commands run inside the repo.
	Namespace string `yaml:"namespace,omitempty"`
	// Run says how preview environments run the service.
	Run *Run `yaml:"run,omitempty"`
	// Resources are what the service needs besides its own pods.
	Resources []Resource `yaml:"resources,omitempty"`
	// Docs lists the pages under docs/ in the order of the side menu.
	Docs []DocPage `yaml:"docs,omitempty"`
}

// DocPage is one entry of docs: a page under docs/ and, optionally, its title.
type DocPage struct {
	Title string `yaml:"title,omitempty"`
	Path  string `yaml:"path"`
}

// DocEntries returns docs: as the docs package takes it.
func (m *Manifest) DocEntries() []docs.Entry {
	out := make([]docs.Entry, 0, len(m.Docs))
	for _, d := range m.Docs {
		out = append(out, docs.Entry{Title: d.Title, Path: d.Path})
	}
	return out
}

// Defaults for a manifest without run settings.
const (
	DefaultRunPort   = 8080
	DefaultRunHealth = "/healthz"
)

// Run says how preview environments run the service.
type Run struct {
	// Port the service listens on for HTTP.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
	// Health is the HTTP path probed for readiness and liveness.
	Health string `yaml:"health,omitempty" json:"health,omitempty"`
	// Domain is the public domain of the service's staging and production
	// addresses, one the platform offers: "payments.<domain>" in production.
	// Empty means the platform's default; DomainNone keeps the service on
	// the platform's internal address.
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty"`
	// Host is the name before the domain in the service's public addresses,
	// when it is not the service's own: with "shop" a service called
	// web-storefront is reached at shop.<domain> in production and at
	// shop-staging.<domain> in staging.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	// Replicas is production's pod count (staging runs one); see options.go
	// for these and the rest of the run options.
	Replicas    int               `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Resources   *Resources        `yaml:"resources,omitempty" json:"resources,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Secrets     []Secret          `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Jobs        []Job             `yaml:"jobs,omitempty" json:"jobs,omitempty"`
	Autoscaling *Autoscaling      `yaml:"autoscaling,omitempty" json:"autoscaling,omitempty"`
}

// RunHost returns run.host as it is written: "" for the service's name.
func (m *Manifest) RunHost() string {
	if m.Run == nil {
		return ""
	}
	return m.Run.Host
}

// DomainNone is the run.domain of a service without a public address.
const DomainNone = "none"

// RunDomain returns run.domain as it is written: "" for the default.
func (m *Manifest) RunDomain() string {
	if m.Run == nil {
		return ""
	}
	return m.Run.Domain
}

// RunPort returns run.port or DefaultRunPort.
func (m *Manifest) RunPort() int {
	if m.Run != nil && m.Run.Port != 0 {
		return m.Run.Port
	}
	return DefaultRunPort
}

// RunHealth returns run.health or DefaultRunHealth.
func (m *Manifest) RunHealth() string {
	if m.Run != nil && m.Run.Health != "" {
		return m.Run.Health
	}
	return DefaultRunHealth
}

// Resource is one entry of resources: today a Postgres database.
type Resource struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	// Storage is whole gibibytes, such as "5Gi" (default 1Gi).
	Storage string `yaml:"storage,omitempty"`
	// Instances is 1 to 3 (default 1 in staging, 2 in production).
	Instances int `yaml:"instances,omitempty"`
	// Environments overrides storage and instances per environment.
	Environments map[string]ResourceOverride `yaml:"environments,omitempty"`
}

// ResourceOverride replaces a resource's values in one environment.
type ResourceOverride struct {
	Storage   string `yaml:"storage,omitempty"`
	Instances int    `yaml:"instances,omitempty"`
}

// count turns a YAML int into the API's int32; a value that does not fit
// becomes -1, which validation refuses.
func count(n int) int32 {
	if n < 0 || n > math.MaxInt32 {
		return -1
	}
	return int32(n)
}

// Declarations returns resources as the API takes them.
func (m *Manifest) Declarations() []*forgev1.ResourceDeclaration {
	out := make([]*forgev1.ResourceDeclaration, 0, len(m.Resources))
	for _, r := range m.Resources {
		d := &forgev1.ResourceDeclaration{Name: r.Name, Type: r.Type, Storage: r.Storage, Instances: count(r.Instances)}
		for env, o := range r.Environments {
			if d.Environments == nil {
				d.Environments = map[string]*forgev1.ResourceOverride{}
			}
			d.Environments[env] = &forgev1.ResourceOverride{Storage: o.Storage, Instances: count(o.Instances)}
		}
		out = append(out, d)
	}
	return out
}

// Service is the catalog part of the manifest.
type Service struct {
	Name         string            `yaml:"name"`
	DisplayName  string            `yaml:"displayName,omitempty"`
	Team         string            `yaml:"team"`
	Owners       []string          `yaml:"owners,omitempty"`
	Lifecycle    string            `yaml:"lifecycle,omitempty"`
	Repo         string            `yaml:"repo,omitempty"`
	Dependencies []string          `yaml:"dependencies,omitempty"`
	Labels       map[string]string `yaml:"labels,omitempty"`
}

var dnsLabel = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// domainName is a lower-case domain of two or more labels.
var domainName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ValidDomain reports whether s is a lower-case domain name such as example.com.
func ValidDomain(s string) bool { return len(s) <= 253 && domainName.MatchString(s) }

// ValidID reports whether s is a valid service id: a lowercase DNS label.
func ValidID(s string) bool { return dnsLabel.MatchString(s) }

// Validate checks required fields and formats.
func (m *Manifest) Validate() error {
	if m.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, m.APIVersion)
	}
	if !ValidID(m.Service.Name) {
		return fmt.Errorf("service.name %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", m.Service.Name)
	}
	if m.Service.Team == "" {
		return errors.New("service.team is required")
	}
	switch m.Service.Lifecycle {
	case "", "experimental", "production", "deprecated":
	default:
		return fmt.Errorf("service.lifecycle %q must be experimental, production or deprecated", m.Service.Lifecycle)
	}
	if r := m.Run; r != nil {
		if r.Port < 0 || r.Port > 65535 {
			return fmt.Errorf("run.port %d must be 1-65535", r.Port)
		}
		if r.Health != "" && !strings.HasPrefix(r.Health, "/") {
			return fmt.Errorf("run.health %q must be an absolute path such as /healthz", r.Health)
		}
		if r.Host != "" && !ValidID(r.Host) {
			return fmt.Errorf("run.host %q must be a lowercase DNS label (a-z, 0-9, '-', max 63), the name before the domain", r.Host)
		}
		if r.Domain != "" && r.Domain != DomainNone && !domainName.MatchString(r.Domain) {
			return fmt.Errorf("run.domain %q must be a domain such as example.com, or %q", r.Domain, DomainNone)
		}
		if err := r.validateOptions(); err != nil {
			return err
		}
	}
	if err := resource.Validate(m.Declarations()); err != nil {
		return fmt.Errorf("resources: %w", err)
	}
	seen := map[string]bool{}
	for i, d := range m.Docs {
		if _, err := docs.CleanPath(d.Path); err != nil {
			return fmt.Errorf("docs[%d].path: %w", i, err)
		}
		if !docs.IsPage(d.Path) {
			return fmt.Errorf("docs[%d].path %q must be a Markdown page (.md) under %s/", i, d.Path, docs.Dir)
		}
		if seen[d.Path] {
			return fmt.Errorf("docs[%d].path %q is listed twice", i, d.Path)
		}
		seen[d.Path] = true
	}
	if len(m.Docs) > docs.MaxPages {
		return fmt.Errorf("docs lists %d pages; at most %d", len(m.Docs), docs.MaxPages)
	}
	return nil
}

// Find walks up from dir to the filesystem root looking for forge.yaml. It
// returns "" and no error when none exists.
func Find(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// Parse decodes and validates forge.yaml content. Unknown fields are errors.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load parses and validates the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// Marshal renders the manifest as YAML.
func (m *Manifest) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Forge service manifest. See 'forge init --help'.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}
