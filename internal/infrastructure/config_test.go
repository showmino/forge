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
              stateContainer: forge-{{ .RepoID }}-{{ .Environment }}
      staging:
        secret: target-azure-staging
        githubSecrets: {AZURE_CLIENT_ID: clientId}
        plan:
          secret: target-azure-staging-plan
          githubSecrets: {AZURE_CLIENT_ID: clientId}
          githubVariables: {TF_STATE_STORAGE_ACCOUNT: planstate}
          access: |
            apiVersion: managedidentity.azure.m.upbound.io/v1beta1
            kind: FederatedIdentityCredential
            metadata:
              name: {{ .Repo }}-{{ .Environment }}-plan
            spec:
              forProvider:
                subject: {{ .Subject }}
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
		"kind not listed":    {"accessKinds: [managedidentity.azure.m.upbound.io/FederatedIdentityCredential]", "accessKinds: []", "accessKinds does not list (add managedidentity.azure.m.upbound.io/FederatedIdentityCredential)"},
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
	want := "job_workflow_ref:my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main:repo:my-org/infra-net:environment:production"
	if got := c.Subject(Repo{Org: "my-org", Name: "infra-net"}, "production"); got != want {
		t.Errorf("Subject = %s\nwant %s", got, want)
	}
	// With GitHub's IDs, as GitHub writes an immutable subject.
	want = "job_workflow_ref:my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main:repo:my-org@155/infra-net@1445:environment:production"
	if got := c.Subject(Repo{Org: "my-org", Name: "infra-net", OwnerID: 155, RepoID: 1445}, "production"); got != want {
		t.Errorf("Subject with IDs = %s\nwant %s", got, want)
	}
}

func TestAccessValuesIncludePlanSubject(t *testing.T) {
	c := mustParse(t, strings.Replace(sample,
		"name: {{ .Repo }}-{{ .Environment }}",
		"name: {{ .Repo }}-{{ .Environment }}\n            annotations:\n              forge.dev/plan-subject: {{ .PlanSubject }}\n              forge.dev/environment-upper: {{ upper .Environment }}",
		1,
	))
	tg, _ := c.Target("azure")
	files, err := c.Files(tg, Repo{
		Org: "my-org", Name: "infra-net", Team: "platform",
		Environments: []string{"production"}, OwnerID: 155, RepoID: 1445,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(files["infrastructure/infra-net/production.yaml"]),
		"forge.dev/plan-subject: job_workflow_ref:"+c.Workflow+":repo:my-org@155/infra-net@1445:pull_request"; !strings.Contains(got, want) {
		t.Fatalf("access template did not receive PlanSubject %q:\n%s", want, got)
	}
	if !strings.Contains(string(files["infrastructure/infra-net/production.yaml"]), "forge.dev/environment-upper: PRODUCTION") {
		t.Fatal("access template upper function did not render the environment")
	}
	if !strings.Contains(string(files["infrastructure/infra-net/production.yaml"]), "stateContainer: forge-1445-production") {
		t.Fatal("access template did not receive RepoID for the isolated state container")
	}
}

func TestPlanObjects(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	r := Repo{Org: "my-org", Name: "infra-net", Team: "platform", Environments: []string{"staging"}, OwnerID: 155, RepoID: 1445}
	files, err := c.Files(tg, r)
	if err != nil {
		t.Fatal(err)
	}
	data := string(files[RepoObjectsPath(r.Name)])
	for _, want := range []string{
		"kind: ActionsSecret", "secretName: FORGE_PLAN_STAGING_AZURE_CLIENT_ID",
		"name: target-azure-staging-plan", "variableName: FORGE_PLAN_STAGING_TF_STATE_STORAGE_ACCOUNT",
		"variableName: FORGE_PLAN_ENVIRONMENTS", `["staging"]`,
		"job_workflow_ref:" + c.Workflow + ":repo:my-org@155/infra-net@1445:pull_request",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, "kind: Environment\n") {
		t.Fatal("plan created a GitHub environment")
	}
}

func TestPlanConfig(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"secret: target-azure-staging-plan", "secret: BAD"},
		{"secret: target-azure-staging-plan", "secret: target-azure-staging"},
		{"githubVariables: {TF_STATE_STORAGE_ACCOUNT: planstate}", "githubVariables: {GITHUB_TOKEN: token}"},
	} {
		_, err := Parse([]byte(strings.Replace(sample, tc.old, tc.new, 1)))
		if err == nil {
			t.Fatal("invalid plan accepted")
		}
	}
}

func TestFiles(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	files, err := c.Files(tg, Repo{Org: "my-org", Name: "infra-net", Team: "platform", Environments: []string{"staging", "production"}, OwnerID: 155, RepoID: 1445})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"infrastructure/infra-net/target.yaml", "infrastructure/infra-net/staging.yaml", "infrastructure/infra-net/production.yaml", "infrastructure/infra-net/repo.yaml"} {
		got, ok := files[p]
		if !ok {
			t.Fatalf("no %s in %v", p, files)
		}
		golden(t, filepath.Join("testdata", strings.ReplaceAll(strings.TrimPrefix(p, "infrastructure/"), "/", "_")+".golden"), got)
	}
	if len(files) != 4 {
		t.Errorf("%d files, want 4", len(files))
	}
}

func TestFilesRefuses(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	if _, err := c.Files(tg, Repo{Org: "my-org", Name: "x", Team: "platform"}); err == nil {
		t.Error("no environments: no error")
	}
	if _, err := c.Files(tg, Repo{Org: "my-org", Name: "x", Team: "platform", Environments: []string{"production", "qa"}, DryRun: true}); err == nil || !strings.Contains(err.Error(), "has no qa") {
		t.Errorf("unknown environment: err = %v", err)
	}
}

// Only a dry run renders without GitHub's IDs.
func TestFilesNeedIDs(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	r := Repo{Org: "my-org", Name: "x", Team: "platform", Environments: []string{"production"}}
	if _, err := c.Files(tg, r); err == nil || !strings.Contains(err.Error(), "IDs are unknown") {
		t.Errorf("without IDs: %v", err)
	}
	r.DryRun = true
	if _, err := c.Files(tg, r); err != nil {
		t.Errorf("dry run: %v", err)
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
