package manifest

import (
	"strings"
	"testing"
)

const infraManifest = `apiVersion: forge.dev/v1
service:
  name: infra-net
  team: platform
infrastructure:
  tool: opentofu
`

func TestInfrastructureManifest(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(infraManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsInfrastructure() || m.Infrastructure.Tool != "opentofu" {
		t.Fatalf("infrastructure = %+v", m.Infrastructure)
	}
	plain, err := Parse([]byte(strings.Split(infraManifest, "infrastructure:")[0]))
	if err != nil {
		t.Fatal(err)
	}
	if plain.IsInfrastructure() {
		t.Error("a manifest without infrastructure is an infrastructure repo")
	}
}

// An infrastructure repo may also run an app on Forge: its forge.yaml has
// both run and infrastructure.
func TestInfrastructureManifestWithRun(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte(infraManifest + "run: {port: 3000, health: /api/healthz}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsInfrastructure() || !m.Runs() || m.RunPort() != 3000 {
		t.Errorf("infrastructure %v, runs %v, port %d", m.IsInfrastructure(), m.Runs(), m.RunPort())
	}
	if plain, _ := Parse([]byte(infraManifest)); plain.Runs() {
		t.Error("an infrastructure repo without run runs")
	}
}

func TestInfrastructureManifestRefuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ add, want string }{
		"unknown tool": {"", "infrastructure.tool"},
		"with chart":   {"chart: {repo: oci://ghcr.io/x, name: x, version: 1.0.0, expose: {service: x, port: 80}}\n", "no chart"},
		"with db":      {"resources: [{name: db, type: postgres}]\n", "no resources"},
		"unknown key":  {"infrastructure2: {}\n", "infrastructure2"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := infraManifest + tc.add
			if name == "unknown tool" {
				s = strings.Replace(infraManifest, "opentofu", "pulumi", 1)
			}
			_, err := Parse([]byte(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one with %q", err, tc.want)
			}
		})
	}
}
