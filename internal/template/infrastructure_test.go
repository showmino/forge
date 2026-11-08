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
	} {
		if got := messages(mustParse(t, infraYAML+tt.add).Validate()); !strings.Contains(got, tt.want) {
			t.Errorf("%s: findings %q, want %q", tt.name, got, tt.want)
		}
	}
}

// An infrastructure template may offer run options: its forge.yaml has run
// as well, for the app next to the code.
func TestInfrastructureTemplateWithRun(t *testing.T) {
	t.Parallel()
	yaml := infraYAML + "options: [replicas, env]\n"
	if f := mustParse(t, yaml).Validate(); len(f) != 0 {
		t.Fatalf("findings: %v", f)
	}
	files, findings := Lint(repo(map[string]string{
		"web/template.yaml":       strings.Replace(yaml, "name: opentofu", "name: web", 1),
		"web/skeleton/forge.yaml": infraManifest + "run:\n  port: 3000\n",
	}), "web")
	if len(findings) > 0 || len(files) != 1 {
		t.Fatalf("files %+v, findings: %s", files, messages(findings))
	}
}

// Options without run would offer the create form choices every create
// then refuses.
func TestLintInfrastructureOptionsNeedRun(t *testing.T) {
	t.Parallel()
	_, findings := Lint(repo(map[string]string{
		"opentofu/template.yaml":       infraYAML + "options: [replicas]\n",
		"opentofu/skeleton/forge.yaml": infraManifest,
	}), "opentofu")
	if !strings.Contains(messages(findings), "offers run options, but its forge.yaml has no run") {
		t.Errorf("findings %q", messages(findings))
	}
}

// Lint renders every value of an enum parameter, not only the default: a
// template that picks its files by an enum has each choice checked.
func TestLintRendersEveryEnumValue(t *testing.T) {
	t.Parallel()
	yaml := infraYAML + `parameters:
  - name: iac
    type: enum
    values: [opentofu, bicep]
    default: opentofu
`
	manifest := strings.Replace(infraManifest, "tool: opentofu", "tool: {{ .Params.iac }}", 1)
	files, findings := Lint(repo(map[string]string{
		"opentofu/template.yaml":       yaml,
		"opentofu/skeleton/forge.yaml": manifest,
		"opentofu/skeleton/infrastructure/{{ if eq .Params.iac `opentofu` }}terraform{{ end }}/main.tf": "terraform {}\n",
		"opentofu/skeleton/infrastructure/{{ if eq .Params.iac `bicep` }}bicep{{ end }}/main.bicep":     "param location string\n",
	}), "opentofu")
	if len(findings) > 0 {
		t.Fatalf("findings: %s", messages(findings))
	}
	// The files returned are the default's render.
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "forge.yaml,infrastructure/terraform/main.tf" {
		t.Errorf("files %s", got)
	}

	// bicep's render is broken: its forge.yaml names a tool there is none of.
	_, findings = Lint(repo(map[string]string{
		"opentofu/template.yaml":       yaml,
		"opentofu/skeleton/forge.yaml": strings.Replace(manifest, "{{ .Params.iac }}", `{{ if eq .Params.iac "bicep" }}arm{{ else }}opentofu{{ end }}`, 1),
	}), "opentofu")
	if got := messages(findings); !strings.Contains(got, "with iac=bicep") || !strings.Contains(got, `"arm"`) {
		t.Errorf("findings %q, want the bicep render's", got)
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
