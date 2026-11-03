package template

import (
	"strings"
	"testing"
)

const infraYAML = `apiVersion: forge.dev/v1
kind: Template
name: opentofu
title: OpenTofu
description: Infrastructure code in OpenTofu, deployed by GitHub Actions
type: infrastructure
`

const infraManifest = `apiVersion: forge.dev/v1
service:
  name: {{ .Service.Name }}
  team: {{ .Service.Team }}
infrastructure:
  tool: opentofu
`

func TestInfrastructureTemplate(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, infraYAML)
	if f := tp.Validate(); len(f) != 0 {
		t.Fatalf("findings: %v", f)
	}
	if !tp.IsInfrastructure() || tp.IsFile() {
		t.Errorf("type %q: infrastructure %v, file %v", tp.Type, tp.IsInfrastructure(), tp.IsFile())
	}
	for _, tt := range []struct{ name, add, want string }{
		{"target", "target: my-org/x\n", "target is only for a file template"},
		{"options", "options: [replicas]\n", "options are only for a service template"},
	} {
		if got := messages(mustParse(t, infraYAML+tt.add).Validate()); !strings.Contains(got, tt.want) {
			t.Errorf("%s: findings %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLintInfrastructureTemplate(t *testing.T) {
	t.Parallel()
	files, findings := Lint(repo(map[string]string{
		"opentofu/template.yaml":                             infraYAML,
		"opentofu/skeleton/forge.yaml":                       infraManifest,
		"opentofu/skeleton/infrastructure/terraform/main.tf": "terraform {}\n",
	}), "opentofu")
	if len(findings) > 0 {
		t.Fatalf("findings: %s", messages(findings))
	}
	if len(files) != 2 {
		t.Errorf("files %+v", files)
	}
	// Its forge.yaml must say infrastructure, and a service's must not.
	_, findings = Lint(repo(map[string]string{
		"opentofu/template.yaml":       infraYAML,
		"opentofu/skeleton/forge.yaml": strings.Split(infraManifest, "infrastructure:")[0],
	}), "opentofu")
	if !strings.Contains(messages(findings), "needs infrastructure.tool") {
		t.Errorf("no infrastructure: %s", messages(findings))
	}
	_, findings = Lint(repo(map[string]string{
		"opentofu/template.yaml":       strings.Replace(infraYAML, "type: infrastructure", "type: service", 1),
		"opentofu/skeleton/forge.yaml": infraManifest,
	}), "opentofu")
	if !strings.Contains(messages(findings), "not type: infrastructure") {
		t.Errorf("service with infrastructure: %s", messages(findings))
	}
}
