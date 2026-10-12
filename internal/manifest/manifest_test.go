package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/docs"
)

func TestParse(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Service.Name != "payments" || m.Service.Team != "billing" {
		t.Errorf("got %+v", m.Service)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  tem: billing\n")); err == nil || !strings.Contains(err.Error(), "tem") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: Bad_Name\n  team: billing\n")); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestRoundTripAndFind(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := &Manifest{APIVersion: APIVersion, Service: Service{Name: "payments", Team: "billing", Lifecycle: "production"}}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Find(sub)
	if err != nil || p != filepath.Join(root, FileName) {
		t.Fatalf("Find = %q, %v", p, err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Service.Name != "payments" || got.Service.Team != "billing" {
		t.Errorf("got %+v", got)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	bad := []Manifest{
		{APIVersion: "v0", Service: Service{Name: "a", Team: "t"}},
		{APIVersion: APIVersion, Service: Service{Name: "Bad_Name", Team: "t"}},
		{APIVersion: APIVersion, Service: Service{Name: "ok"}},
		{APIVersion: APIVersion, Service: Service{Name: "ok", Team: "t", Lifecycle: "alive"}},
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	for _, id := range []string{"a", "payments", "pr-142", "a1"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "1a", "-a", "a-", "A", "a_b"} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	base := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"
	m, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if m.RunPort() != 8080 || m.RunHealth() != "/healthz" {
		t.Errorf("defaults: %d %q", m.RunPort(), m.RunHealth())
	}
	m, err = Parse([]byte(base + "run:\n  port: 9090\n  health: /ready\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.RunPort() != 9090 || m.RunHealth() != "/ready" {
		t.Errorf("set: %d %q", m.RunPort(), m.RunHealth())
	}
	// run.domain: unset means the platform's default, "none" no public address.
	if m.RunDomain() != "" {
		t.Errorf("domain without one: %q", m.RunDomain())
	}
	for domain, want := range map[string]string{"deogratias.cloud": "deogratias.cloud", "none": "none", "a-b.example.co.uk": "a-b.example.co.uk"} {
		d, err := Parse([]byte(base + "run:\n  domain: " + domain + "\n"))
		if err != nil || d.RunDomain() != want {
			t.Errorf("run.domain %s: %q %v", domain, d.RunDomain(), err)
		}
	}
	// run.host: the name before the domain, when it is not the service's own.
	if h, err := Parse([]byte(base + "run:\n  host: forge\n")); err != nil || h.RunHost() != "forge" || m.RunHost() != "" {
		t.Errorf("run.host: %v %v", h, err)
	}
	for _, bad := range []string{"run:\n  port: 70000\n", "run:\n  port: -1\n", "run:\n  health: ready\n", "run:\n  nope: 1\n",
		"run:\n  host: Forge\n", "run:\n  host: forge.showmino.com\n", "run:\n  host: -forge\n", "run:\n  host: \"*\"\n",
		"run:\n  domain: Example.com\n", "run:\n  domain: localhost\n", "run:\n  domain: https://example.com\n", "run:\n  domain: \"*.example.com\"\n", "run:\n  domain: example.com.\n"} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	out, err := (&Manifest{APIVersion: APIVersion, Service: Service{Name: "a", Team: "b"}, Run: &Run{Port: 9090, Health: "/ready"}}).Marshal()
	if err != nil || !strings.Contains(string(out), "run:\n  port: 9090\n  health: /ready\n") {
		t.Errorf("marshal: %s %v", out, err)
	}
}

func TestDocs(t *testing.T) {
	t.Parallel()
	base := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"
	m, err := Parse([]byte(base + "docs:\n  - title: Overview\n    path: index.md\n  - path: api/errors.md\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []docs.Entry{{Title: "Overview", Path: "index.md"}, {Path: "api/errors.md"}}
	if got := m.DocEntries(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("DocEntries = %+v", got)
	}
	for _, bad := range []string{
		"docs:\n  - path: ../README.md\n",
		"docs:\n  - path: /index.md\n",
		"docs:\n  - path: img/a.png\n",
		"docs:\n  - title: x\n",
		"docs:\n  - path: a.md\n  - path: a.md\n",
		"docs:\n  - path: a.md\n    order: 1\n",
	} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestResources(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(`apiVersion: forge.dev/v1
service:
  name: payments
  team: billing
resources:
  - name: main
    type: postgres
    storage: 5Gi
    environments:
      production:
        storage: 20Gi
        instances: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	d := m.Declarations()
	if len(d) != 1 || d[0].GetName() != "main" || d[0].GetType() != "postgres" || d[0].GetStorage() != "5Gi" || d[0].GetInstances() != 0 {
		t.Fatalf("declarations: %v", d)
	}
	if o := d[0].GetEnvironments()["production"]; o.GetStorage() != "20Gi" || o.GetInstances() != 2 {
		t.Errorf("override: %v", o)
	}

	// A manifest without resources has none, and writes none back.
	bare, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Declarations()) != 0 {
		t.Errorf("bare: %v", bare.Declarations())
	}
	out, err := bare.Marshal()
	if err != nil || strings.Contains(string(out), "resources") {
		t.Errorf("marshal: %s %v", out, err)
	}

	for name, body := range map[string]string{
		"a bad size":       "resources:\n  - {name: main, type: postgres, storage: 5GB}\n",
		"an unknown field": "resources:\n  - {name: main, type: postgres, version: 18}\n",
		"an unknown env":   "resources:\n  - {name: main, type: postgres, environments: {prod: {instances: 2}}}\n",
		"a huge instances": "resources:\n  - {name: main, type: postgres, instances: 99999999999}\n",
	} {
		_, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n" + body))
		if err == nil {
			t.Errorf("%s: want an error", name)
		} else if name != "an unknown field" && !strings.Contains(err.Error(), "resources: ") {
			t.Errorf("%s: %v, want it to start with resources:", name, err)
		}
	}
}
