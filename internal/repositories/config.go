// Package repositories renders Crossplane rulesets for repositories Forge creates.
package repositories

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/manifest"
)

// InfrastructureCheck is the reusable workflow's fail-closed PR gate.
const InfrastructureCheck = "infrastructure / pr-checks"

// Config is the Crossplane destination of repository protection.
type Config struct {
	Namespace      string                        `yaml:"namespace"`
	ProviderConfig infrastructure.ProviderConfig `yaml:"providerConfig"`
}

// Load reads a repository protection configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse checks a repository protection configuration.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if !manifest.ValidID(c.Namespace) {
		return nil, fmt.Errorf("namespace %q must be a Kubernetes namespace name", c.Namespace)
	}
	if c.ProviderConfig.Kind != "ProviderConfig" && c.ProviderConfig.Kind != "ClusterProviderConfig" {
		return nil, fmt.Errorf("providerConfig.kind must be ProviderConfig or ClusterProviderConfig")
	}
	if !manifest.ValidID(c.ProviderConfig.Name) {
		return nil, fmt.Errorf("providerConfig.name must be a Kubernetes name")
	}
	return &c, nil
}

// Path is kept independently of a service's deploys: removing a catalog entry
// must not make its repository writable without a pull request.
func Path(repo string) string { return "repositories/" + repo + "/rules.yaml" }

// Rules requires pull requests on main, without administrator or App bypasses.
func (c *Config) Rules(repo string, infrastructure bool) ([]byte, error) {
	if !manifest.ValidID(repo) {
		return nil, fmt.Errorf("repository %q must be a Forge repository name", repo)
	}
	rules := map[string]any{
		"deletion":       true,
		"nonFastForward": true,
		"pullRequest":    []any{map[string]any{"requiredApprovingReviewCount": 0}},
	}
	if infrastructure {
		rules["requiredStatusChecks"] = []any{map[string]any{
			"strictRequiredStatusChecksPolicy": true,
			"requiredCheck": []any{map[string]any{
				"context":       InfrastructureCheck,
				"integrationId": 15368, // GitHub Actions, not a user-supplied commit status.
			}},
		}}
	}
	obj := map[string]any{
		"apiVersion": "repo.github.m.upbound.io/v1alpha1",
		"kind":       "RepositoryRuleset",
		"metadata": map[string]any{
			"name":   repo + "-main",
			"labels": map[string]string{"forge.dev/service": repo},
		},
		"spec": map[string]any{
			"managementPolicies": []string{"Observe", "Create", "Update", "LateInitialize"},
			"providerConfigRef":  c.ProviderConfig,
			"forProvider": map[string]any{
				"repository":   repo,
				"name":         "forge-main",
				"target":       "branch",
				"enforcement":  "active",
				"bypassActors": []any{},
				"conditions": []any{map[string]any{
					"refName": []any{map[string]any{
						"include": []string{"refs/heads/main"},
						"exclude": []string{},
					}},
				}},
				"rules": []any{rules},
			},
		},
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
