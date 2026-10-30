package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Chart is forge.yaml's chart: a published Helm chart the service runs
// instead of an image (phase 11). Such a service has no repo: its forge.yaml
// lives in the deploy repo.
type Chart struct {
	// Repo is the chart repository: oci://<registry>/<path> or https://<host>/<path>.
	Repo string `yaml:"repo" json:"repo"`
	// Name is the chart's name in Repo.
	Name string `yaml:"name" json:"name"`
	// Version is one exact version of it, such as 6.7.1.
	Version string `yaml:"version" json:"version"`
	// Values are the chart's values in every environment.
	Values map[string]any `yaml:"values,omitempty" json:"values,omitempty"`
	// Expose is the chart's Service that gets the service's address.
	Expose ChartExpose `yaml:"expose" json:"expose"`
	// Environments holds values of one environment, merged over Values.
	Environments map[string]ChartEnv `yaml:"environments,omitempty" json:"environments,omitempty"`
}

// ChartExpose names a Service the chart makes and its port.
type ChartExpose struct {
	Service string `yaml:"service" json:"service"`
	Port    int    `yaml:"port" json:"port"`
}

// ChartEnv is chart.environments.<env>.
type ChartEnv struct {
	Values map[string]any `yaml:"values,omitempty" json:"values,omitempty"`
}

// MaxChartValues is the most bytes chart values may take, written as YAML.
const MaxChartValues = 128 << 10

var (
	chartName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?$`)
	// An exact SemVer 2 version, with an optional leading v: no ranges.
	chartVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

// IsChart reports whether the service runs a chart.
func (m *Manifest) IsChart() bool { return m.Chart != nil }

// EnvValues is chart.environments.<env>.values, nil without any.
func (c *Chart) EnvValues(env string) map[string]any {
	if c == nil {
		return nil
	}
	return c.Environments[env].Values
}

// ValidChartRepo checks a chart repository: oci:// or https://, with a host,
// and no login, query or fragment in it.
func ValidChartRepo(repo string) error {
	u, err := url.Parse(repo)
	if err != nil || (u.Scheme != "oci" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(repo, "/") {
		return fmt.Errorf("chart.repo %q must be oci://<registry>/<path> or https://<host>/<path>, with no login, query or trailing slash", repo)
	}
	if u.Scheme == "oci" && strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("chart.repo %q must name a path in the registry, such as oci://ghcr.io/my-org/charts", repo)
	}
	return nil
}

// ValidChartVersion checks a chart version: one exact version, not a range.
func ValidChartVersion(v string) error {
	if !chartVersion.MatchString(v) {
		return fmt.Errorf("chart.version %q must be one exact version such as 6.7.1, not a range or latest", v)
	}
	return nil
}

func (c *Chart) validate() error {
	if err := ValidChartRepo(c.Repo); err != nil {
		return err
	}
	if !chartName.MatchString(c.Name) {
		return fmt.Errorf("chart.name %q must be a chart's name: lowercase letters, digits, '.', '_' and '-'", c.Name)
	}
	if err := ValidChartVersion(c.Version); err != nil {
		return err
	}
	if !ValidID(c.Expose.Service) {
		return fmt.Errorf("chart.expose.service %q must name the chart's Service that gets the address (a lowercase DNS label)", c.Expose.Service)
	}
	if c.Expose.Port < 1 || c.Expose.Port > 65535 {
		return fmt.Errorf("chart.expose.port %d must be the Service's port, 1-65535", c.Expose.Port)
	}
	size := 0
	for env, e := range c.Environments {
		if env != "staging" && env != "production" {
			return fmt.Errorf("chart.environments.%s: the environments are staging and production", env)
		}
		data, err := yaml.Marshal(e.Values)
		if err != nil {
			return fmt.Errorf("chart.environments.%s.values: %w", env, err)
		}
		size += len(data)
	}
	data, err := yaml.Marshal(c.Values)
	if err != nil {
		return fmt.Errorf("chart.values: %w", err)
	}
	if size += len(data); size > MaxChartValues {
		return fmt.Errorf("chart values take %d bytes; at most %d", size, MaxChartValues)
	}
	return nil
}

// checkChart checks what goes with chart: of the run options only the
// address (run.domain, run.host, run.staging) applies, since the chart
// decides how it runs; and Forge's databases cannot reach its pods.
func (m *Manifest) checkChart() error {
	if m.Chart == nil {
		return nil
	}
	if err := m.Chart.validate(); err != nil {
		return err
	}
	if len(m.Resources) > 0 {
		return errors.New("resources: a service that runs a chart has no Forge databases; the chart brings what it needs")
	}
	r := m.Run
	if r == nil {
		return nil
	}
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"image", r.Image != ""}, {"port", r.Port != 0}, {"health", r.Health != ""}, {"writable", len(r.Writable) > 0},
		{"replicas", r.Replicas != 0}, {"resources", r.Resources != nil}, {"env", len(r.Env) > 0},
		{"secrets", len(r.Secrets) > 0}, {"jobs", len(r.Jobs) > 0}, {"autoscaling", r.Autoscaling != nil},
	} {
		if f.set {
			return fmt.Errorf("run.%s does not apply to a service that runs a chart: set it in chart.values", f.name)
		}
	}
	return nil
}

// SetChartVersion sets chart.version, keeping the rest of forge.yaml.
func SetChartVersion(data []byte, version string) ([]byte, error) {
	if err := ValidChartVersion(version); err != nil {
		return nil, err
	}
	return editChart(data, func(chart *yaml.Node) error {
		setScalar(chart, "version", version)
		return nil
	})
}

// SetChartValues replaces chart.values, keeping the rest of forge.yaml.
func SetChartValues(data []byte, values map[string]any) ([]byte, error) {
	return editChart(data, func(chart *yaml.Node) error {
		removeKey(chart, "values")
		if len(values) == 0 {
			return nil
		}
		var v yaml.Node
		if err := v.Encode(values); err != nil {
			return err
		}
		v.Style = 0
		// Before expose, where forge.yaml has it.
		at := slices.IndexFunc(chart.Content, func(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Value == "expose" })
		pair := []*yaml.Node{{Kind: yaml.ScalarNode, Value: "values"}, &v}
		if at < 0 || at%2 != 0 {
			chart.Content = append(chart.Content, pair...)
		} else {
			chart.Content = slices.Insert(chart.Content, at, pair...)
		}
		return nil
	})
}

func editChart(data []byte, edit func(chart *yaml.Node) error) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("forge.yaml is not a mapping")
	}
	chart := mapValue(doc.Content[0], "chart")
	if chart == nil || chart.Kind != yaml.MappingNode {
		return nil, errors.New("forge.yaml has no chart")
	}
	chart.Style = 0
	if err := edit(chart); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if _, err := Parse(b.Bytes()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func setScalar(m *yaml.Node, key, value string) {
	if v := mapValue(m, key); v != nil && v.Kind == yaml.ScalarNode {
		v.Value, v.Tag, v.Style = value, "", 0
		return
	}
	removeKey(m, key)
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}
