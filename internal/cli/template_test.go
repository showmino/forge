package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run 'make update-golden' to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const lintManifest = "apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n"

var goodLintTemplate = map[string]string{
	"svc/template.yaml":       "apiVersion: forge.dev/v1\nkind: Template\nname: svc\ntitle: Sample\ndescription: Fixture\nparameters:\n  - name: port\n    type: int\n    default: 8080\n",
	"svc/skeleton/forge.yaml": lintManifest,
	"svc/skeleton/main.go":    "package main // port {{ .Params.port }}\n",
}

var badLintTemplate = map[string]string{
	"bad/template.yaml":       "apiVersion: forge.dev/v1\nkind: Template\nname: bad\ntitle: Bad\ndescription: Fixture\nparameters:\n  - name: port\n    type: int\n    default: eighty\n",
	"bad/skeleton/forge.yaml": lintManifest,
}

func TestTemplateLint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Lint is local: it must work with no API and no credentials.
	delete(h.env, "FORGE_API_URL")
	delete(h.env, "FORGE_TOKEN")
	writeTree(t, filepath.Join(h.dir, "repo"), goodLintTemplate)

	r := h.ok("template", "lint", "repo/svc") // one template
	if r.out != "" || !strings.Contains(r.err, "1 template checked, no problems") {
		t.Errorf("clean single template: %+v", r)
	}
	h.ok("template", "lint", "repo") // a repo of templates

	writeTree(t, filepath.Join(h.dir, "repo"), badLintTemplate)
	r = h.fails(1, "template", "lint", "repo")
	golden(t, "template_lint_table", r.out)
	if !strings.Contains(r.err, "1 problem in 2 templates") {
		t.Errorf("summary: %q", r.err)
	}
	r = h.fails(1, "template", "lint", "repo", "-o", "json")
	golden(t, "template_lint_json", r.out)
	r = h.fails(1, "template", "lint", "repo", "-o", "name")
	if r.out != "bad/template.yaml:7\n" {
		t.Errorf("name output: %q", r.out)
	}
}

func TestTemplateLintWrongPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Join(h.dir, "empty", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := h.fails(2, "template", "lint", "empty")
	if !strings.Contains(r.err, "no template.yaml in") {
		t.Errorf("empty dir: %q", r.err)
	}
	h.fails(2, "template", "lint", "does-not-exist")
	h.fails(2, "template", "lint", "a", "b")
}

func TestTemplateLintRenderDir(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), goodLintTemplate)
	h.ok("template", "lint", "repo", "--render-dir", "out")
	data, err := os.ReadFile(filepath.Join(h.dir, "out", "svc", "main.go"))
	if err != nil || string(data) != "package main // port 8080\n" {
		t.Errorf("rendered main.go: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "out", "svc", "forge.yaml")); err != nil {
		t.Error(err)
	}
	h.fails(6, "template", "lint", "repo", "--render-dir", "out") // never overwrites
}

var templateRepo = map[string]string{
	"svc/template.yaml": "apiVersion: forge.dev/v1\nkind: Template\nname: svc\ntitle: Go service\ndescription: Go service fixture\ntags: [go, grpc]\nowner: platform\nparameters:\n" +
		"  - name: port\n    type: int\n    default: 8080\n    description: Port the server listens on\n" +
		"  - name: tier\n    type: enum\n    values: [gold, silver]\n    description: Support tier\n",
	"svc/skeleton/forge.yaml": lintManifest,
	"web/template.yaml":       "apiVersion: forge.dev/v1\nkind: Template\nname: web\ntitle: Web app\ndescription: Web fixture\ntags: [node]\n",
	"web/skeleton/forge.yaml": lintManifest,
}

func TestTemplateListAndGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	h.serveTemplates(filepath.Join(h.dir, "repo"))

	golden(t, "template_list_table", h.ok("template", "list").out)
	golden(t, "template_list_json", h.ok("template", "list", "-o", "json").out)
	if r := h.ok("template", "list", "-o", "name"); r.out != "templates/svc\ntemplates/web\n" {
		t.Errorf("names: %q", r.out)
	}
	if r := h.ok("template", "list", "--tag", "node", "-o", "name"); r.out != "templates/web\n" {
		t.Errorf("--tag: %q", r.out)
	}
	r := h.ok("template", "list", "--tag", "rust")
	if r.out != "" || !strings.Contains(r.err, "No templates found.") {
		t.Errorf("no match, table: %+v", r)
	}
	if r := h.ok("template", "list", "--tag", "rust", "-o", "json"); !strings.Contains(r.out, `"templates": []`) {
		t.Errorf("no match, json: %q", r.out)
	}

	golden(t, "template_get_table", h.ok("template", "get", "svc").out)
	golden(t, "template_get_json", h.ok("template", "get", "templates/svc@dev", "-o", "json").out)
	if r := h.ok("template", "get", "web"); strings.Contains(r.out, "REQUIRED") {
		t.Errorf("a template without parameters prints no parameter table:\n%s", r.out)
	}
}

func TestTemplateGetErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fails(6, "template", "list") // the API has no template source
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	h.serveTemplates(filepath.Join(h.dir, "repo"))
	h.fails(3, "template", "get", "nope")
	h.fails(3, "template", "get", "svc@v1.0.0") // a directory serves only "dev"
	h.fails(3, "template", "list", "--version", "v1.0.0")
	h.fails(2, "template", "get", "Bad_Name")
	h.fails(2, "template", "get")
	delete(h.env, "FORGE_TOKEN")
	h.fails(4, "template", "list")
}
