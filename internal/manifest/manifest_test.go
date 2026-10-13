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

func TestRunOptions(t *testing.T) {
	t.Parallel()
	base := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"
	full := base + `run:
  replicas: 3
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: 250m, memory: 256Mi}
  env:
    LOG_LEVEL: info
  secrets:
    - name: stripe
      item: vaults/Apps/items/stripe
      env: {STRIPE_KEY: credential, STRIPE_HOOK: webhook-secret}
  jobs:
    - name: cleanup
      schedule: "0 3 * * *"
      command: [node, scripts/cleanup.js]
    - name: hourly
      schedule: "@hourly"
      command: [./report]
  autoscaling:
    max: 6
`
	m, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Run
	if r.Replicas != 3 || r.Resources.Limits.Memory != "256Mi" || r.Env["LOG_LEVEL"] != "info" ||
		len(r.Secrets) != 1 || r.Secrets[0].Vault() != "Apps" || r.Secrets[0].Env["STRIPE_KEY"] != "credential" ||
		len(r.Jobs) != 2 || r.Jobs[0].Command[1] != "scripts/cleanup.js" || r.Autoscaling.Max != 6 {
		t.Errorf("parsed: %+v", r)
	}
	if m.ProductionReplicas() != 3 {
		t.Errorf("replicas: %d", m.ProductionReplicas())
	}
	if d, _ := Parse([]byte(base)); d.ProductionReplicas() != 1 {
		t.Errorf("default replicas: %d", d.ProductionReplicas())
	}

	for name, bad := range map[string]string{
		"replicas 0":           "run:\n  replicas: -1\n",
		"replicas 11":          "run:\n  replicas: 11\n",
		"bad cpu":              "run:\n  resources: {limits: {cpu: lots}}\n",
		"request over limit":   "run:\n  resources: {requests: {memory: 1Gi}, limits: {memory: 512Mi}}\n",
		"over the quota":       "run:\n  replicas: 5\n  resources: {limits: {memory: 1Gi}}\n",
		"autoscaled over":      "run:\n  replicas: 2\n  autoscaling: {max: 9}\n",
		"env lower case":       "run:\n  env: {log_level: info}\n",
		"env PORT":             "run:\n  env: {PORT: \"1\"}\n",
		"env DATABASE_URL":     "run:\n  env: {DATABASE_URL: x}\n",
		"secret name":          "run:\n  secrets: [{name: Stripe, item: vaults/A/items/b, env: {K: v}}]\n",
		"secret item":          "run:\n  secrets: [{name: s, item: A/b, env: {K: v}}]\n",
		"secret item spaces":   "run:\n  secrets: [{name: s, item: vaults/My Vault/items/b, env: {K: v}}]\n",
		"secret no env":        "run:\n  secrets: [{name: s, item: vaults/A/items/b}]\n",
		"secret field":         "run:\n  secrets: [{name: s, item: vaults/A/items/b, env: {K: \"a/b\"}}]\n",
		"secret twice":         "run:\n  secrets: [{name: s, item: vaults/A/items/b, env: {K: v}}, {name: s, item: vaults/A/items/c, env: {L: v}}]\n",
		"env twice":            "run:\n  env: {K: x}\n  secrets: [{name: s, item: vaults/A/items/b, env: {K: v}}]\n",
		"job name":             "run:\n  jobs: [{name: Clean, schedule: \"@daily\", command: [x]}]\n",
		"job schedule":         "run:\n  jobs: [{name: c, schedule: \"every day\", command: [x]}]\n",
		"job schedule 6":       "run:\n  jobs: [{name: c, schedule: \"0 0 3 * * *\", command: [x]}]\n",
		"job no command":       "run:\n  jobs: [{name: c, schedule: \"@daily\"}]\n",
		"job twice":            "run:\n  jobs: [{name: c, schedule: \"@daily\", command: [x]}, {name: c, schedule: \"@daily\", command: [y]}]\n",
		"autoscaling max 11":   "run:\n  autoscaling: {max: 11}\n",
		"autoscaling below":    "run:\n  replicas: 3\n  autoscaling: {max: 3}\n",
		"unknown option field": "run:\n  jobs: [{name: c, schedule: \"@daily\", command: [x], image: y}]\n",
		"requests over quota":  "run:\n  replicas: 4\n  resources: {requests: {memory: 512Mi}, limits: {memory: 512Mi}}\n",
		"request over default": "run:\n  resources: {requests: {memory: 1Gi}}\n",
		"too many pods":        "run:\n  replicas: 10\n  resources: {limits: {cpu: 10m, memory: 16Mi}}\n  jobs: [{name: a, schedule: \"@daily\", command: [x]}, {name: b, schedule: \"@daily\", command: [x]}, {name: c, schedule: \"@daily\", command: [x]}]\nresources:\n  - {name: main, type: postgres, instances: 3}\n",
		"no room for the db":   "run:\n  replicas: 6\nresources:\n  - {name: main, type: postgres}\n",
		"job name too long":    "run:\n  jobs: [{name: " + strings.Repeat("j", 44) + ", schedule: \"@daily\", command: [x]}]\n",
		"secret name too long": "run:\n  secrets: [{name: " + strings.Repeat("s", 41) + ", item: vaults/A/items/b, env: {K: v}}]\n",
	} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	six := "run:\n  jobs:\n"
	for i := range 6 {
		six += "    - {name: j" + string(rune('a'+i)) + ", schedule: \"@daily\", command: [x]}\n"
	}
	if _, err := Parse([]byte(base + six)); err == nil {
		t.Error("six jobs accepted")
	}
	// The same six replicas fit without a database next to them.
	if _, err := Parse([]byte(base + "run:\n  replicas: 6\n")); err != nil {
		t.Errorf("six replicas: %v", err)
	}
	// A limit without a request makes the request the limit, as Kubernetes does.
	if _, err := Parse([]byte(base + "run:\n  resources: {limits: {memory: 1Gi}}\n")); err != nil {
		t.Errorf("limit only: %v", err)
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
