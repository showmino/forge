package manifest

import (
	"strings"
	"testing"
)

const chartManifest = `apiVersion: forge.dev/v1
service:
  name: podinfo
  team: web
chart:
  repo: oci://ghcr.io/stefanprodan/charts
  name: podinfo
  version: 6.7.1
  values:
    replicaCount: 2
    ui: {message: hello}
  expose: {service: podinfo, port: 9898}
  environments:
    production:
      values: {replicaCount: 3}
run:
  domain: example.com
`

func TestChartManifest(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(chartManifest))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Chart
	if c == nil || c.Repo != "oci://ghcr.io/stefanprodan/charts" || c.Name != "podinfo" || c.Version != "6.7.1" ||
		c.Expose.Service != "podinfo" || c.Expose.Port != 9898 || m.RunDomain() != "example.com" {
		t.Fatalf("chart: %+v", c)
	}
	if got := c.EnvValues("production")["replicaCount"]; got != 3 {
		t.Errorf("production values: %v", got)
	}
	if got := c.EnvValues("staging"); got != nil {
		t.Errorf("staging values: %v", got)
	}
	if !m.IsChart() {
		t.Error("IsChart")
	}
}

func TestChartManifestRefuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ from, to, want string }{
		"http repo":         {"oci://ghcr.io/stefanprodan/charts", "http://charts.example.com", "chart.repo"},
		"repo with a login": {"oci://ghcr.io/stefanprodan/charts", "https://user:pw@charts.example.com", "chart.repo"},
		"repo with a query": {"oci://ghcr.io/stefanprodan/charts", "https://charts.example.com/?x=1", "chart.repo"},
		"bad name":          {"  name: podinfo\n  version", "  name: Pod/Info\n  version", "chart.name"},
		"version range":     {"version: 6.7.1", "version: ^6.7.0", "chart.version"},
		"latest":            {"version: 6.7.1", "version: latest", "chart.version"},
		"no version":        {"  version: 6.7.1\n", "", "chart.version"},
		"bad expose":        {"service: podinfo, port: 9898", "service: Pod_Info, port: 9898", "chart.expose.service"},
		"no port":           {"service: podinfo, port: 9898", "service: podinfo", "chart.expose.port"},
		"another env":       {"    production:\n", "    qa:\n", "chart.environments"},
		"with an image":     {"run:\n  domain: example.com\n", "run:\n  image: nginx:1.27\n", "run.image"},
		"with replicas":     {"run:\n  domain: example.com\n", "run:\n  replicas: 2\n", "run.replicas"},
		"with a port":       {"run:\n  domain: example.com\n", "run:\n  port: 80\n", "run.port"},
		"with env":          {"run:\n  domain: example.com\n", "run:\n  env: {A: b}\n", "run.env"},
		"with a database":   {"run:\n  domain: example.com\n", "resources:\n  - {type: postgres, name: main}\n", "resources"},
	} {
		data := strings.Replace(chartManifest, tc.from, tc.to, 1)
		if data == chartManifest {
			t.Fatalf("%s: nothing replaced", name)
		}
		_, err := Parse([]byte(data))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error about %s", name, err, tc.want)
		}
	}
}

func TestSetChart(t *testing.T) {
	t.Parallel()
	data := []byte("# kept\n" + chartManifest)
	out, err := SetChartVersion(data, "6.8.0")
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetChartValues(out, map[string]any{"replicaCount": 4})
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if m.Chart.Version != "6.8.0" || m.Chart.Values["replicaCount"] != 4 || m.Chart.Values["ui"] != nil || m.Chart.EnvValues("production")["replicaCount"] != 3 {
		t.Errorf("chart: %+v", m.Chart)
	}
	if !strings.HasPrefix(string(out), "# kept\n") {
		t.Errorf("comment lost:\n%s", out)
	}
	if _, err := SetChartVersion([]byte("apiVersion: forge.dev/v1\nservice: {name: a, team: b}\n"), "1.0.0"); err == nil {
		t.Error("set a chart version without a chart")
	}
}
