package infrastructure

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const sample = `
namespace: forge-infrastructure
providerConfig: {kind: ClusterProviderConfig, name: github}
workflow: my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main
accessKinds: [managedidentity.azure.m.upbound.io/FederatedIdentityCredential]
targets:
  - name: azure
    title: Azure
    teams: [platform]
    environments:
      production:
        secret: target-azure-production
        githubSecrets:
          AZURE_TENANT_ID: tenantId
          AZURE_SUBSCRIPTION_ID: subscriptionId
          AZURE_CLIENT_ID: clientId
        githubVariables:
          TF_STATE_STORAGE_ACCOUNT: ststate
        maxRepos: 20
        access: |
          apiVersion: managedidentity.azure.m.upbound.io/v1beta1
          kind: FederatedIdentityCredential
          metadata:
            name: {{ .Repo }}-{{ .Environment }}
          spec:
            forProvider:
              audience: [api://AzureADTokenExchange]
              issuer: https://token.actions.githubusercontent.com
              subject: {{ .Subject }}
              parentId: /subscriptions/sub/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id
              resourceGroupName: rg
      staging:
        secret: target-azure-staging
        githubSecrets: {AZURE_CLIENT_ID: clientId}
`

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParse(t *testing.T) {
	c := mustParse(t, sample)
	tg, ok := c.Target("azure")
	if !ok {
		t.Fatal("no target azure")
	}
	if got := strings.Join(tg.EnvironmentNames(), ","); got != "staging,production" {
		t.Errorf("environments = %s, want staging,production", got)
	}
	if !tg.Allows("platform") || tg.Allows("web") {
		t.Errorf("Allows: platform %v, web %v; want true, false", tg.Allows("platform"), tg.Allows("web"))
	}
	if _, ok := c.Target("aws"); ok {
		t.Error("found a target aws")
	}
}

func TestParseRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"unknown field":      {"title: Azure", "titel: Azure", "field titel not found"},
		"bad namespace":      {"namespace: forge-infrastructure", "namespace: Forge", "namespace"},
		"bad provider kind":  {"kind: ClusterProviderConfig", "kind: Config", "providerConfig.kind"},
		"workflow no ref":    {"infrastructure.yml@refs/heads/main", "infrastructure.yml", "workflow"},
		"unknown env":        {"      staging:", "      dev:", `environment "dev"`},
		"github prefix":      {"AZURE_TENANT_ID: tenantId", "GITHUB_TOKEN: tenantId", "GITHUB_"},
		"secret and var":     {"TF_STATE_STORAGE_ACCOUNT: ststate", "AZURE_CLIENT_ID: ststate", "both a secret and a variable"},
		"negative max":       {"maxRepos: 20", "maxRepos: -1", "maxRepos"},
		"template typo":      {"{{ .Repo }}-{{ .Environment }}", "{{ .Repos }}", "access"},
		"access namespace":   {"            name: {{ .Repo }}-{{ .Environment }}", "            name: x\n            namespace: other", "metadata.namespace"},
		"access no name":     {"            name: {{ .Repo }}-{{ .Environment }}", "            labels: {}", "metadata.name"},
		"bad access kind":    {"[managedidentity.azure.m.upbound.io/FederatedIdentityCredential]", "[FederatedIdentityCredential]", "accessKinds"},
		"target named twice": {"targets:\n", "targets:\n  - name: azure\n    environments: {staging: {secret: s}}\n", "named twice"},
	} {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(sample, tc.old, tc.new, 1)
			if s == sample {
				t.Fatalf("%q is not in the sample", tc.old)
			}
			_, err := Parse([]byte(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one with %q", err, tc.want)
			}
		})
	}
}

func TestSubject(t *testing.T) {
	c := mustParse(t, sample)
	want := "repo:my-org/infra-net:environment:production:job_workflow_ref:my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main"
	if got := c.Subject("my-org", "infra-net", "production"); got != want {
		t.Errorf("Subject = %s\nwant %s", got, want)
	}
}

func TestFiles(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	files, err := c.Files(tg, Repo{Org: "my-org", Name: "infra-net", Team: "platform", Environments: []string{"staging", "production"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"infrastructure/infra-net/target.yaml", "infrastructure/infra-net/staging.yaml", "infrastructure/infra-net/production.yaml"} {
		got, ok := files[p]
		if !ok {
			t.Fatalf("no %s in %v", p, files)
		}
		golden(t, filepath.Join("testdata", strings.ReplaceAll(strings.TrimPrefix(p, "infrastructure/"), "/", "_")+".golden"), got)
	}
	if len(files) != 3 {
		t.Errorf("%d files, want 3", len(files))
	}
}

func TestFilesRefuses(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	if _, err := c.Files(tg, Repo{Org: "my-org", Name: "x", Team: "platform"}); err == nil {
		t.Error("no environments: no error")
	}
	if _, err := c.Files(tg, Repo{Org: "my-org", Name: "x", Team: "platform", Environments: []string{"production", "qa"}}); err == nil || !strings.Contains(err.Error(), "has no qa") {
		t.Errorf("unknown environment: err = %v", err)
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run make update-golden)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}
