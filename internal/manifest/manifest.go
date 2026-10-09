// Package manifest reads and writes forge.yaml, the per-repo source of truth
// for a service's catalog entry. Phase 1 covers the catalog fields; Phase 3
// adds run, how a preview environment runs the service.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
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
}

// Defaults for a manifest without run settings.
const (
	DefaultRunPort   = 8080
	DefaultRunHealth = "/healthz"
)

// Run says how preview environments run the service.
type Run struct {
	// Port the service listens on for HTTP.
	Port int `yaml:"port,omitempty"`
	// Health is the HTTP path probed for readiness and liveness.
	Health string `yaml:"health,omitempty"`
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
