package repositories

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRules(t *testing.T) {
	c, err := Parse([]byte("namespace: forge-repositories\nproviderConfig: {kind: ClusterProviderConfig, name: github}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, infrastructure := range []bool{false, true} {
		data, err := c.Rules("payments", infrastructure)
		if err != nil {
			t.Fatal(err)
		}
		var obj struct {
			Spec struct {
				DeletionPolicy string `yaml:"deletionPolicy"`
				ForProvider    struct {
					Enforcement  string `yaml:"enforcement"`
					BypassActors []any  `yaml:"bypassActors"`
					Rules        []struct {
						Deletion             bool             `yaml:"deletion"`
						NonFastForward       bool             `yaml:"nonFastForward"`
						PullRequest          []map[string]any `yaml:"pullRequest"`
						RequiredStatusChecks []struct {
							Strict        bool `yaml:"strictRequiredStatusChecksPolicy"`
							RequiredCheck []struct {
								Context       string `yaml:"context"`
								IntegrationID int    `yaml:"integrationId"`
							} `yaml:"requiredCheck"`
						} `yaml:"requiredStatusChecks"`
					} `yaml:"rules"`
				} `yaml:"forProvider"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(data, &obj); err != nil {
			t.Fatal(err)
		}
		p := obj.Spec.ForProvider
		if obj.Spec.DeletionPolicy != "Orphan" || p.Enforcement != "active" || len(p.BypassActors) != 0 || len(p.Rules) != 1 {
			t.Fatalf("unsafe ruleset:\n%s", data)
		}
		r := p.Rules[0]
		if !r.Deletion || !r.NonFastForward || len(r.PullRequest) != 1 {
			t.Fatalf("missing protections:\n%s", data)
		}
		if !infrastructure {
			if len(r.RequiredStatusChecks) != 0 {
				t.Fatal("application repo requires infrastructure checks")
			}
			continue
		}
		if len(r.RequiredStatusChecks) != 1 {
			t.Fatalf("no required PR checks:\n%s", data)
		}
		check := r.RequiredStatusChecks[0]
		if !check.Strict || len(check.RequiredCheck) != 1 || check.RequiredCheck[0].Context != InfrastructureCheck || check.RequiredCheck[0].IntegrationID != 15368 {
			t.Fatalf("incorrect required checks:\n%s", data)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, data := range []string{
		"namespace: Bad\nproviderConfig: {kind: ClusterProviderConfig, name: github}",
		"namespace: forge\nproviderConfig: {kind: Bad, name: github}",
		"namespace: forge\nproviderConfig: {kind: ClusterProviderConfig, name: Bad}",
		"namespace: forge\nproviderConfig: {kind: ClusterProviderConfig, name: github}\nunknown: true",
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestRulesRefusesInvalidName(t *testing.T) {
	c, err := Parse([]byte("namespace: forge\nproviderConfig: {kind: ClusterProviderConfig, name: github}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rules("../escape", false); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("invalid repository: %v", err)
	}
}
