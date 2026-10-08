package template

import (
	"strings"
	"testing"
	"testing/fstest"
)

func repo(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return fsys
}

const goodTemplate = `apiVersion: forge.dev/v1
kind: Template
name: svc
title: Sample
description: Fixture
parameters:
  - name: port
    type: int
    default: 8080
  - name: module
    type: string
    pattern: '^[a-z0-9./-]+$'
`

const goodManifest = "apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n"

func messages(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.String())
	}
	return strings.Join(out, "\n")
}

func TestLintClean(t *testing.T) {
	t.Parallel()
	files, findings := Lint(repo(map[string]string{
		"svc/template.yaml":       goodTemplate,
		"svc/skeleton/forge.yaml": goodManifest,
		"svc/skeleton/main.go":    "package main // {{ .Params.port }} {{ .Params.module }}\n",
	}), "svc")
	if len(findings) > 0 {
		t.Fatalf("findings: %s", messages(findings))
	}
	// A required string with a pattern gets the sample "sample"; the sample
	// service name has a dash on purpose.
	want := map[string]string{"forge.yaml": "  name: sample-service\n", "main.go": "8080 sample"}
	for _, f := range files {
		if w, ok := want[f.Path]; ok && !strings.Contains(string(f.Data), w) {
			t.Errorf("%s = %q, want it to contain %q", f.Path, f.Data, w)
		}
	}
	if len(files) != 2 {
		t.Errorf("want 2 rendered files, got %d", len(files))
	}
}

func TestLintFindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"no template.yaml", map[string]string{"svc/skeleton/a": "x"}, []string{"svc/template.yaml: "}},
		{"strict parse", map[string]string{"svc/template.yaml": goodTemplate + "extra: 1\n", "svc/skeleton/forge.yaml": goodManifest}, []string{"field extra not found"}},
		{"name differs from dir", map[string]string{"other/template.yaml": goodTemplate, "other/skeleton/forge.yaml": goodManifest}, []string{`other/template.yaml: name "svc" must equal the directory name "other"`}},
		{"no skeleton", map[string]string{"svc/template.yaml": goodTemplate}, []string{"svc/skeleton: skeleton/ must exist and not be empty"}},
		{"bad default", map[string]string{"svc/template.yaml": strings.Replace(goodTemplate, "default: 8080", "default: eighty", 1), "svc/skeleton/forge.yaml": goodManifest}, []string{`svc/template.yaml:7: parameter "port": "eighty" is not an int`}},
		{"sample fails pattern", map[string]string{"svc/template.yaml": strings.Replace(goodTemplate, "'^[a-z0-9./-]+$'", "'^x'", 1), "svc/skeleton/forge.yaml": goodManifest}, []string{`svc/template.yaml:10: parameter "module"`, "give it a default"}},
		{"two broken files", map[string]string{"svc/template.yaml": goodTemplate, "svc/skeleton/forge.yaml": goodManifest, "svc/skeleton/a.txt": "{{ .X ", "svc/skeleton/b.txt": "{{ end }}"}, []string{"svc/skeleton/a.txt: ", "svc/skeleton/b.txt: "}},
		{"execution error", map[string]string{"svc/template.yaml": goodTemplate, "svc/skeleton/forge.yaml": goodManifest, "svc/skeleton/a.txt": "{{ .Params.nope }}"}, []string{`svc/skeleton: `, `"nope"`}},
		{"no forge.yaml", map[string]string{"svc/template.yaml": goodTemplate, "svc/skeleton/a.txt": "x"}, []string{"does not render a forge.yaml"}},
		{"forge.yaml wrong team", map[string]string{"svc/template.yaml": goodTemplate, "svc/skeleton/forge.yaml": strings.Replace(goodManifest, "{{ .Service.Team }}", "platform", 1)}, []string{`svc/skeleton/forge.yaml: rendered forge.yaml names service "sample-service" and team "platform"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := strings.SplitN(func() string {
				for k := range tt.files {
					return k
				}
				return ""
			}(), "/", 2)[0]
			files, findings := Lint(repo(tt.files), dir)
			got := messages(findings)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("want a finding containing %q, got:\n%s", w, got)
				}
			}
			if files != nil {
				t.Error("a template with findings must not return a render")
			}
			for _, f := range findings {
				if f.Template != dir {
					t.Errorf("finding %q has Template %q, want %q", f, f.Template, dir)
				}
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	got, err := Discover(repo(map[string]string{
		"b/template.yaml": "x", "a/template.yaml": "x", "notes/readme.md": "x", "README.md": "x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("got %v", got)
	}
}
