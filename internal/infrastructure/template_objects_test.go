package infrastructure

import (
	"path/filepath"
	"strings"
	"testing"
)

// sampleTemplateObjects is a target whose templates make every object of an
// environment (objects: template), for example one instance of a kro
// ResourceGraphDefinition that makes the GitHub environment and the cloud
// access.
const sampleTemplateObjects = `
namespace: forge-infrastructure
providerConfig: {kind: ClusterProviderConfig, name: github}
workflow: my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main
accessKinds: [kro.run/CloudEnvironment]
targets:
  - name: cloud
    objects: template
    environments:
      staging:
        access: |
          apiVersion: kro.run/v1alpha1
          kind: CloudEnvironment
          metadata:
            name: {{ .Repo }}-{{ .Environment }}
          spec:
            repo: {{ .Repo }}
            repoID: "{{ .RepoID }}"
            environment: {{ .Environment }}
            deploySubject: {{ .Subject }}
            planSubject: {{ .PlanSubject }}
      production:
        maxRepos: 5
        access: |
          apiVersion: kro.run/v1alpha1
          kind: CloudEnvironment
          metadata:
            name: {{ .Repo }}-{{ .Environment }}
          spec:
            repo: {{ .Repo }}
            repoID: "{{ .RepoID }}"
            environment: {{ .Environment }}
            deploySubject: {{ .Subject }}
            planSubject: {{ .PlanSubject }}
`

func TestTemplateObjectsFiles(t *testing.T) {
	c := mustParse(t, sampleTemplateObjects)
	tg, ok := c.Target("cloud")
	if !ok {
		t.Fatal("no target cloud")
	}
	if !tg.TemplateObjects() {
		t.Fatal("TemplateObjects() = false for objects: template")
	}
	files, err := c.Files(tg, Repo{Org: "my-org", Name: "infra-net", Team: "platform", Environments: []string{"staging", "production"}, OwnerID: 155, RepoID: 1445})
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"staging", "production"} {
		got := string(files["infrastructure/infra-net/"+env+".yaml"])
		// Only the template's object: no GitHub environment, secret or
		// variable of Forge's own.
		if n := strings.Count(got, "\nkind: "); n != 1 || !strings.Contains(got, "kind: CloudEnvironment") {
			t.Errorf("%s has %d objects, want the template's one:\n%s", env, n, got)
		}
		if !strings.Contains(got, "forge.dev/service: infra-net") || !strings.Contains(got, "forge.dev/deploy-env: "+env) {
			t.Errorf("%s: the template's object lacks Forge's labels:\n%s", env, got)
		}
	}
	// The repo keeps only Forge's two environment lists.
	repo := string(files["infrastructure/infra-net/repo.yaml"])
	if n := strings.Count(repo, "kind: ActionsVariable"); n != 2 || strings.Count(repo, "\nkind: ") != 2 {
		t.Errorf("repo.yaml: want FORGE_ENVIRONMENTS and FORGE_PLAN_ENVIRONMENTS only:\n%s", repo)
	}
	for _, p := range []string{"infrastructure/infra-net/staging.yaml", "infrastructure/infra-net/production.yaml", "infrastructure/infra-net/repo.yaml"} {
		golden(t, filepath.Join("testdata", "template-objects_"+filepath.Base(p)+".golden"), files[p])
	}
}

// The default is unchanged: Forge writes the GitHub objects.
func TestTemplateObjectsDefault(t *testing.T) {
	c := mustParse(t, sample)
	tg, _ := c.Target("azure")
	if tg.TemplateObjects() {
		t.Error("TemplateObjects() = true without objects: template")
	}
	ok := strings.Replace(sample, "    title: Azure\n", "    title: Azure\n    objects: forge\n", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("objects: forge: %v", err)
	}
}

func TestTemplateObjectsRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"unknown value": {"objects: template", "objects: kro", `objects "kro" must be forge or template`},
		"github secrets": {"      production:\n        maxRepos: 5\n", "      production:\n        maxRepos: 5\n        githubSecrets: {AZURE_CLIENT_ID: clientId}\n",
			"githubSecrets: with objects: template the access template makes them"},
		"github variables": {"      production:\n        maxRepos: 5\n", "      production:\n        maxRepos: 5\n        githubVariables: {AZURE_LOCATION: here}\n",
			"githubVariables: with objects: template the access template makes them"},
		"plan block": {"      production:\n        maxRepos: 5\n", "      production:\n        maxRepos: 5\n        plan: {secret: p}\n",
			"plan: with objects: template the access template makes the plan access"},
	} {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(sampleTemplateObjects, tc.old, tc.new, 1)
			if s == sampleTemplateObjects {
				t.Fatalf("%q is not in the sample", tc.old)
			}
			_, err := Parse([]byte(s))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one with %q", err, tc.want)
			}
		})
	}
	// An environment without an access template would make nothing at all.
	empty := `
namespace: forge-infrastructure
providerConfig: {kind: ClusterProviderConfig, name: github}
workflow: my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main
targets:
  - name: cloud
    objects: template
    environments:
      production: {maxRepos: 1}
`
	if _, err := Parse([]byte(empty)); err == nil || !strings.Contains(err.Error(), "access: with objects: template it makes every object") {
		t.Errorf("no access template: err = %v", err)
	}
}
