# Phase 2, Milestone 1: Template Format and Lint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Define the Forge template format in a shared Go package, ship `forge template lint`, and publish the first template (`go-grpc` v1.0.0) in `showmino/forge-templates` with CI that lints it and builds what it renders.

**Architecture:** A new pure-Go package `internal/template` parses `template.yaml`, resolves typed parameters, renders `skeleton/` with `text/template` and lints a template, all over an `fs.FS` so the CLI (local directory) and, in milestone 3, the API (a fetched tag) share it. `forge template lint` wraps it with no API dependency. The template repo is a separate Git repo whose CI builds `forge` from source to lint and test-render its templates.

**Tech Stack:** Go 1.24 (`text/template`, `io/fs`, `testing/fstest`), `gopkg.in/yaml.v3`, Cobra; the `go-grpc` template uses connect-go v1.18.1, protobuf v1.36.5, buf v1.47.2, golangci-lint v2.5.0, GitHub Actions.

**Spec:** `docs/specs/2026-10-08-phase-2-templates-design.md` (sections 1, 3 "template lint", 5 and 6, milestone 1). Design doc: `docs/index.md`.

## Global Constraints

- Go is not on the system PATH: run commands with `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`. Run the full gate as `GOTOOLCHAIN=go1.24.13 make test lint` (CI parity; golangci-lint v2.5.0).
- No package-level mutable globals in `internal/cli`; commands get dependencies through `*Factory` (`Deps`). Read-only package vars (regexps, func maps, key sets) are fine, as in `internal/manifest`.
- Commands return errors; only `main` picks exit codes (`internal/exitcode`). Lint findings exit 1 (`exitcode.Unexpected`), usage problems exit 2 (`exitcode.Usagef`).
- Every command supports `-o table|json|yaml|name`; JSON is the data itself.
- `template.yaml` is decoded strictly: unknown fields are an error, including inside `parameters`.
- Template file rules (spec section 1): `name` is a lowercase DNS label equal to the directory name; `title` and `description` required and single-line; parameter names match `^[a-zA-Z][a-zA-Z0-9]*$` and are unique; types `string | int | bool | enum` (`enum` requires `values`); a parameter with no `default` is required; `pattern` is RE2 and only for strings.
- Rendering: `text/template` with the template's delimiters (default `{{ }}`), `Option("missingkey=error")`, functions exactly `lower`, `upper`, `replace`, `trimPrefix`, `trimSuffix`; paths rendered per segment, an empty segment drops the file or directory; symlinks rejected; at most 2,000 files and 10 MiB.
- The render must contain a root `forge.yaml` that `manifest` accepts and that names the requested service and team.
- Parameter values are at most 1 KiB.
- Commit after every task with the repo's attribution trailer: `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

**Deliberate refinements of the spec, made while planning** (the spec is updated to match in Task 0):

- The `go-grpc` template uses delimiters `{%` `%}`, not `[[` `]]`: Go generics (`connect.Request[pkgv1.GreetRequest]`) collide with `[[`. `{%` does not occur in Go, YAML, shell or GitHub Actions, and is valid in file names on every OS.
- Files that are not valid UTF-8 are copied as they are instead of rendered, so binary files (images) cannot break a template.
- Lint renders with the sample service `sample-service` and team `sample-team`, so every template is checked against a name containing a dash.
- The "parameter name is not a built-in" rule is dropped: parameters live under `.Params`, so they cannot shadow built-ins.
- `forge template lint` gets a hidden `--render-dir DIR` flag that writes each template's sample render, so the template repo's CI can build what a template produces.
- The template repo's CI checks out and builds `forge` with the read-only token instead of downloading a tagged release, so no `forge` release is needed in this milestone.

## Review Focus

1. **Service names with dashes** (`billing-api`): Go and proto identifiers cannot contain `-`, so a template that pastes `.Service.Name` into a package name breaks. Pinned by the lint sample name `sample-service` (Task 5 test) and by building the rendered `go-grpc` in Task 7 and its CI.
2. **Skeleton files that contain other templating syntax** (`${{ github.sha }}` in workflows, `{{ }}` in Helm-like files, `Request[T]` generics): must survive rendering byte for byte. Pinned in the Task 4 golden render fixture.
3. **Binary files in the skeleton** that happen to contain delimiter bytes: must be copied unchanged. Pinned in the Task 4 fixture (`logo.bin`).
4. **A typo inside a parameter** (`defualt: 8080`): strict decoding must still reject it, although parameters use a custom decoder. Pinned in Task 2.
5. **Pointing lint at the wrong place** (a directory with no templates, a path that does not exist): exit 2 with a message that says what was expected, never "0 problems". Pinned in Task 6.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/manifest/manifest.go` (modify) | Add `Parse([]byte)`, used by `Load` and by template checks on rendered bytes |
| `internal/template/template.go` (create) | Types (`Template`, `Parameter`, `Finding`), constants, strict `Parse`, field rules (`Validate`) |
| `internal/template/params.go` (create) | `Values`, `Parameter.Convert`, `Template.ResolveParams`, `ParamError` |
| `internal/template/render.go` (create) | Function map, `File`, `Template.Render`, `Template.CheckSyntax`, `CheckManifest` |
| `internal/template/lint.go` (create) | `SampleValues`, `Lint`, `Discover` |
| `internal/template/*_test.go`, `testdata/render.golden` (create) | Tests |
| `internal/cli/template.go` (create) | `forge template` group and `lint` verb |
| `internal/cli/root.go` (modify) | Register the `template` group under "Core commands" |
| `internal/cli/template_test.go`, `internal/cli/testdata/*.golden` (create) | CLI tests and golden output |
| `Makefile`, `README.md`, `.claude/skills/forge/SKILL.md` (modify) | `update-golden` covers the new packages; docs |
| `~/code/forge-templates/` (new repo `showmino/forge-templates`) | `README.md`, `go-grpc/template.yaml`, `go-grpc/skeleton/**`, `.github/workflows/lint.yml` |

---

### Task 0: Align the spec with the planning refinements

**Files:**
- Modify: `docs/specs/2026-10-08-phase-2-templates-design.md`

**Interfaces:** none.

- [ ] **Step 1: Replace the `[[ ]]` delimiters in the spec's examples with `{% %}`**

In section 1 (`template.yaml` example) change `delimiters: ["[[", "]]"]` to `delimiters: ["{%", "%}"]` and `"github.com/[[ .Repo.Owner ]]/[[ .Repo.Name ]]"` to `"github.com/{% .Repo.Owner %}/{% .Repo.Name %}"`. In section 3 (`template get` example) change the default shown the same way. In section 5 change every `[[ … ]]` to `{% … %}`.

- [ ] **Step 2: Record the refinements**

Under "Rendering" in section 1, add the line:

```markdown
- Files that are not valid UTF-8 are copied unchanged instead of rendered.
```

Under "Lint" step 5, change "the sample service `example` with team `example`" to "the sample service `sample-service` with team `sample-team` (a name with a dash, so templates are checked against one)". In the `parameters[].name` row remove ", not a built-in name". In "Needs from the owner", replace the milestone 1 CI row's proposal with: "a read-only fine-grained token for `showmino/forge` (Contents: read) as the org secret `FORGE_READ_TOKEN`; the template repo's CI checks out and builds `forge` with it." In the `forge template lint` section add: "A hidden `--render-dir DIR` flag also writes each template's sample render to `DIR/<template>` (used by the template repo's CI)."

- [ ] **Step 3: Commit**

```bash
git add docs/specs/2026-10-08-phase-2-templates-design.md
git commit -m "spec: {% %} delimiters for go-grpc, binary files, sample names, render-dir

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 1: `manifest.Parse`

**Files:**
- Modify: `internal/manifest/manifest.go` (the `Load` function)
- Test: `internal/manifest/manifest_test.go`

**Interfaces:**
- Produces: `func Parse(data []byte) (*Manifest, error)` — strict decode plus `Validate`. `Load(path)` keeps its signature and wraps `Parse` errors with the path.

- [ ] **Step 1: Write the failing test** (append to `internal/manifest/manifest_test.go`)

```go
func TestParse(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Service.Name != "payments" || m.Service.Team != "billing" {
		t.Errorf("got %+v", m.Service)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  tem: billing\n")); err == nil || !strings.Contains(err.Error(), "tem") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: Bad_Name\n  team: billing\n")); err == nil {
		t.Error("invalid name accepted")
	}
}
```

Add `"strings"` to the test file's imports if it is not there.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/manifest -run TestParse -v`
Expected: FAIL to compile with `undefined: Parse`.

- [ ] **Step 3: Implement** — replace `Load` in `internal/manifest/manifest.go` with:

```go
// Parse decodes and validates forge.yaml content. Unknown fields are errors.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load parses and validates the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}
```

- [ ] **Step 4: Run the package and CLI tests**

Run: `go test ./internal/manifest ./internal/cli`
Expected: PASS (the CLI test for a broken `forge.yaml` still finds "forge.yaml" and "tem" in the message).

- [ ] **Step 5: Commit**

```bash
git add internal/manifest
git commit -m "manifest: add Parse for forge.yaml bytes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Template types, strict parsing and field rules

**Files:**
- Create: `internal/template/template.go`
- Test: `internal/template/template_test.go`

**Interfaces:**
- Consumes: `manifest.ValidID(string) bool`.
- Produces:
  - constants `FileName = "template.yaml"`, `SkeletonDir = "skeleton"`, `APIVersion = "forge.dev/v1"`, `Kind = "Template"`, `MaxFiles = 2000`, `MaxBytes = 10 << 20`
  - `type ParamType string` with `TypeString`, `TypeInt`, `TypeBool`, `TypeEnum`
  - `type Template struct { APIVersion, Kind, Name, Title, Description string; Tags []string; Owner string; Delimiters []string; Parameters []Parameter }`
  - `type Parameter struct { Name string; Type ParamType; Default any; Pattern string; Values []string; Description string; Line int }`
  - `func (p Parameter) Required() bool`, `func (p Parameter) DefaultString() (string, bool)`
  - `type Finding struct { Template, File string; Line int; Message string }` with JSON tags `template`, `file`, `line,omitempty`, `message`, and `func (f Finding) String() string`
  - `func Parse(data []byte) (*Template, error)`
  - `func (t *Template) Delims() (left, right string)`
  - `func (t *Template) Validate() []Finding` (findings have `File: FileName`; `Template` empty — `Lint` fills it)

- [ ] **Step 1: Write the failing tests** — create `internal/template/template_test.go`:

```go
package template

import (
	"strings"
	"testing"
)

const validYAML = `apiVersion: forge.dev/v1
kind: Template
name: svc
title: Sample service
description: Fixture for tests
tags: [go, grpc]
owner: platform
delimiters: ["{%", "%}"]
parameters:
  - name: goModule
    type: string
    default: "github.com/{% .Repo.Owner %}/{% .Repo.Name %}"
    pattern: '^[a-z0-9./-]+$'
    description: Go module path
  - name: port
    type: int
    default: 8080
  - name: tier
    type: enum
    values: [gold, silver]
  - name: debug
    type: bool
    default: false
`

func TestParseValid(t *testing.T) {
	t.Parallel()
	tp, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if tp.Name != "svc" || len(tp.Parameters) != 4 {
		t.Fatalf("got %+v", tp)
	}
	if l, r := tp.Delims(); l != "{%" || r != "%}" {
		t.Errorf("delims %q %q", l, r)
	}
	port := tp.Parameters[1]
	if d, ok := port.DefaultString(); !ok || d != "8080" || port.Required() {
		t.Errorf("port default %q %v required=%v", d, ok, port.Required())
	}
	if port.Line != 15 {
		t.Errorf("port line = %d, want 15", port.Line)
	}
	if !tp.Parameters[2].Required() {
		t.Error("tier has no default, so it is required")
	}
	if f := tp.Validate(); len(f) != 0 {
		t.Errorf("valid template has findings: %v", f)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"top level":       strings.Replace(validYAML, "owner: platform", "ownr: platform", 1),
		"inside a param":  strings.Replace(validYAML, "    default: 8080", "    defualt: 8080", 1),
		"param not a map": strings.Replace(validYAML, "  - name: port\n    type: int\n    default: 8080\n", "  - port\n", 1),
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(nil); err == nil {
		t.Error("empty document accepted")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, from, to, want string
	}{
		{"api version", "apiVersion: forge.dev/v1", "apiVersion: forge.dev/v2", `apiVersion must be "forge.dev/v1"`},
		{"kind", "kind: Template", "kind: Service", `kind must be "Template"`},
		{"name", "name: svc", "name: Svc_1", "lowercase DNS label"},
		{"title", "title: Sample service", "title: ''", "title is required"},
		{"description", "description: Fixture for tests", "description: ''", "description is required"},
		{"tag", "tags: [go, grpc]", "tags: [Go]", `tag "Go"`},
		{"delimiters", `delimiters: ["{%", "%}"]`, `delimiters: ["{%", "{%"]`, "delimiters must be two different"},
		{"param name", "  - name: port", "  - name: 9port", `parameter "9port": name must be`},
		{"duplicate", "  - name: debug", "  - name: port", `parameter "port" is defined twice`},
		{"type", "    type: bool", "    type: float", `type must be string, int, bool or enum`},
		{"enum values", "    values: [gold, silver]", "    values: []", "needs values"},
		{"pattern type", "    type: int\n    default: 8080", "    type: int\n    pattern: '^1'\n    default: 8080", "pattern is only allowed for type string"},
		{"bad pattern", `pattern: '^[a-z0-9./-]+$'`, `pattern: '('`, "invalid pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := strings.Replace(validYAML, tt.from, tt.to, 1)
			if doc == validYAML {
				t.Fatalf("replacement %q not found", tt.from)
			}
			tp, err := Parse([]byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			var msgs []string
			for _, f := range tp.Validate() {
				msgs = append(msgs, f.String())
			}
			if !strings.Contains(strings.Join(msgs, "\n"), tt.want) {
				t.Errorf("want a finding containing %q, got %q", tt.want, msgs)
			}
		})
	}
}

func TestFindingString(t *testing.T) {
	t.Parallel()
	if got := (Finding{File: "a/template.yaml", Line: 3, Message: "bad"}).String(); got != "a/template.yaml:3: bad" {
		t.Errorf("with line: %q", got)
	}
	if got := (Finding{File: "a/skeleton", Message: "bad"}).String(); got != "a/skeleton: bad" {
		t.Errorf("without line: %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/template -v`
Expected: FAIL to compile (`undefined: Parse`).

- [ ] **Step 3: Implement** — create `internal/template/template.go`:

```go
// Package template parses, lints and renders Forge service templates: a
// template.yaml with typed parameters and a skeleton/ directory rendered with
// Go text/template. It does no I/O of its own; callers pass an fs.FS, so the
// CLI (a local directory) and the API (a fetched tag) share one implementation.
package template

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/showmino/forge/internal/manifest"
)

// Layout and schema of a template directory.
const (
	FileName    = "template.yaml"
	SkeletonDir = "skeleton"
	APIVersion  = "forge.dev/v1"
	Kind        = "Template"
)

// Render limits.
const (
	MaxFiles = 2000
	MaxBytes = 10 << 20
)

// ParamType is a parameter's type.
type ParamType string

// Parameter types.
const (
	TypeString ParamType = "string"
	TypeInt    ParamType = "int"
	TypeBool   ParamType = "bool"
	TypeEnum   ParamType = "enum"
)

// Template is the content of template.yaml.
type Template struct {
	APIVersion  string      `yaml:"apiVersion"`
	Kind        string      `yaml:"kind"`
	Name        string      `yaml:"name"`
	Title       string      `yaml:"title"`
	Description string      `yaml:"description"`
	Tags        []string    `yaml:"tags,omitempty"`
	Owner       string      `yaml:"owner,omitempty"`
	Delimiters  []string    `yaml:"delimiters,omitempty"`
	Parameters  []Parameter `yaml:"parameters,omitempty"`
}

// Parameter is one typed input of a template.
type Parameter struct {
	Name        string    `yaml:"name"`
	Type        ParamType `yaml:"type"`
	Default     any       `yaml:"default,omitempty"`
	Pattern     string    `yaml:"pattern,omitempty"`
	Values      []string  `yaml:"values,omitempty"`
	Description string    `yaml:"description,omitempty"`
	// Line is where the parameter starts in template.yaml, for findings.
	Line int `yaml:"-"`
}

var parameterKeys = map[string]bool{"name": true, "type": true, "default": true, "pattern": true, "values": true, "description": true}

// UnmarshalYAML records the parameter's line. yaml.v3 does not pass strict
// decoding into custom unmarshalers, so unknown keys are checked here.
func (p *Parameter) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a parameter must be a mapping with name and type", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		if k := n.Content[i]; !parameterKeys[k.Value] {
			return fmt.Errorf("line %d: field %s not found in type template.Parameter", k.Line, k.Value)
		}
	}
	type plain Parameter
	if err := n.Decode((*plain)(p)); err != nil {
		return err
	}
	p.Line = n.Line
	return nil
}

// Required reports whether the parameter has no default.
func (p Parameter) Required() bool { return p.Default == nil }

// DefaultString returns the default as written, and whether there is one.
func (p Parameter) DefaultString() (string, bool) {
	if p.Default == nil {
		return "", false
	}
	return fmt.Sprint(p.Default), true
}

// Finding is one problem lint found.
type Finding struct {
	Template string `json:"template"`
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.File, f.Message)
}

// Parse decodes template.yaml strictly: unknown fields are errors.
func Parse(data []byte) (*Template, error) {
	var t Template
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("template.yaml is empty")
		}
		return nil, err
	}
	return &t, nil
}

// Delims returns the template's action delimiters.
func (t *Template) Delims() (left, right string) {
	if len(t.Delimiters) == 2 {
		return t.Delimiters[0], t.Delimiters[1]
	}
	return "{{", "}}"
}

var (
	identifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*$`)
	tagWord    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Validate checks the field rules of template.yaml. Defaults are checked by
// Lint, because a string default may use built-in values.
func (t *Template) Validate() []Finding {
	var out []Finding
	add := func(line int, format string, args ...any) {
		out = append(out, Finding{File: FileName, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	if t.APIVersion != APIVersion {
		add(0, "apiVersion must be %q, got %q", APIVersion, t.APIVersion)
	}
	if t.Kind != Kind {
		add(0, "kind must be %q, got %q", Kind, t.Kind)
	}
	if !manifest.ValidID(t.Name) {
		add(0, "name %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", t.Name)
	}
	if strings.TrimSpace(t.Title) == "" || strings.Contains(t.Title, "\n") {
		add(0, "title is required and must be one line")
	}
	if strings.TrimSpace(t.Description) == "" || strings.Contains(t.Description, "\n") {
		add(0, "description is required and must be one line")
	}
	for _, tag := range t.Tags {
		if !tagWord.MatchString(tag) {
			add(0, "tag %q must be a lowercase word", tag)
		}
	}
	if t.Delimiters != nil && (len(t.Delimiters) != 2 || t.Delimiters[0] == "" || t.Delimiters[1] == "" || t.Delimiters[0] == t.Delimiters[1]) {
		add(0, "delimiters must be two different, non-empty strings")
	}
	seen := map[string]bool{}
	for _, p := range t.Parameters {
		if !identifier.MatchString(p.Name) {
			add(p.Line, "parameter %q: name must be letters and digits, starting with a letter", p.Name)
		}
		if seen[p.Name] {
			add(p.Line, "parameter %q is defined twice", p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case TypeString, TypeInt, TypeBool:
			if len(p.Values) > 0 {
				add(p.Line, "parameter %q: values are only allowed for type enum", p.Name)
			}
		case TypeEnum:
			if len(p.Values) == 0 {
				add(p.Line, "parameter %q: type enum needs values", p.Name)
			}
		default:
			add(p.Line, "parameter %q: type must be string, int, bool or enum, got %q", p.Name, p.Type)
		}
		if p.Pattern != "" {
			if p.Type != TypeString {
				add(p.Line, "parameter %q: pattern is only allowed for type string", p.Name)
			} else if _, err := regexp.Compile(p.Pattern); err != nil {
				add(p.Line, "parameter %q: invalid pattern: %v", p.Name, err)
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/template -v`
Expected: PASS. If `port.Line` is not 15, count the lines of `validYAML` (line 1 is `apiVersion`) and fix the test's expectation, not the code: `Line` must be the line of `- name: port`.

- [ ] **Step 5: Commit**

```bash
git add internal/template
git commit -m "template: types, strict template.yaml parsing and field rules

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Typed parameters

**Files:**
- Create: `internal/template/params.go`
- Test: `internal/template/params_test.go`

**Interfaces:**
- Consumes: `Template`, `Parameter` (Task 2). This task also creates `render.go` with `funcs`, `parse` and `renderString`; Task 4 replaces that file with a version that keeps all three unchanged.
- Produces:
  - `type Values struct { Service ServiceValues; Repo RepoValues; Template TemplateValues; Params map[string]any }`
  - `type ServiceValues struct { Name, Team string; Owners []string }`, `type RepoValues struct { Owner, Name, URL string }`, `type TemplateValues struct { Name, Version string }`
  - `const MaxParamLen = 1024`
  - `type ParamError struct { Name, Msg string }` (`Error()` = `parameter "<name>": <msg>`)
  - `func (p Parameter) Convert(raw string) (any, error)` — `int` → `int`, `bool` → `bool`, others → `string`
  - `func (t *Template) ResolveParams(v Values, raw map[string]string) (map[string]any, error)`
  - in `render.go`: `func (t *Template) parse(name, text string) (*gotemplate.Template, error)`, `func (t *Template) renderString(name, text string, v Values) (string, error)` and the function map `funcs`

- [ ] **Step 1: Write the failing tests** — create `internal/template/params_test.go`:

```go
package template

import (
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) *Template {
	t.Helper()
	tp, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func sampleBuiltins() Values {
	return Values{
		Service:  ServiceValues{Name: "billing-api", Team: "billing"},
		Repo:     RepoValues{Owner: "showmino", Name: "billing-api", URL: "https://github.com/showmino/billing-api"},
		Template: TemplateValues{Name: "svc", Version: "v1.0.0"},
	}
}

func TestResolveParams(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	got, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"tier": "gold", "debug": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got["goModule"] != "github.com/showmino/billing-api" {
		t.Errorf("goModule default rendered with built-ins: %v", got["goModule"])
	}
	if got["port"] != 8080 || got["debug"] != true || got["tier"] != "gold" {
		t.Errorf("typed values: %#v", got)
	}
}

func TestResolveParamsErrors(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	tests := []struct {
		raw  map[string]string
		want string
	}{
		{map[string]string{}, `parameter "tier": required`},
		{map[string]string{"tier": "gold", "nope": "x"}, `parameter "nope": unknown parameter`},
		{map[string]string{"tier": "bronze"}, `"bronze" must be one of gold, silver`},
		{map[string]string{"tier": "gold", "port": "http"}, `"http" is not an int`},
		{map[string]string{"tier": "gold", "debug": "maybe"}, `"maybe" is not a bool`},
		{map[string]string{"tier": "gold", "goModule": "Bad Module"}, `does not match`},
		{map[string]string{"tier": "gold", "goModule": strings.Repeat("a", MaxParamLen+1)}, "longer than 1024 bytes"},
	}
	for _, tt := range tests {
		_, err := tp.ResolveParams(sampleBuiltins(), tt.raw)
		var pe *ParamError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: got %v, want a ParamError containing %q", tt.raw, err, tt.want)
		}
	}
}

func TestResolveParamsBadDefaultTemplate(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, strings.Replace(validYAML, "{% .Repo.Name %}", "{% .Repo.Nope %}", 1))
	if _, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"tier": "gold"}); err == nil || !strings.Contains(err.Error(), "goModule") {
		t.Errorf("default using an unknown field: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/template -run ResolveParams -v`
Expected: FAIL to compile (`undefined: Values`).

- [ ] **Step 3: Implement**

Create `internal/template/params.go`:

```go
package template

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// MaxParamLen caps one parameter value.
const MaxParamLen = 1024

// Values are what a template renders with.
type Values struct {
	Service  ServiceValues
	Repo     RepoValues
	Template TemplateValues
	// Params are the typed parameters: int, bool or string.
	Params map[string]any
}

// ServiceValues describe the service being created.
type ServiceValues struct {
	Name   string
	Team   string
	Owners []string
}

// RepoValues describe the repo being created.
type RepoValues struct {
	Owner string
	Name  string
	URL   string
}

// TemplateValues identify the template being rendered.
type TemplateValues struct {
	Name    string
	Version string
}

// ParamError is a parameter value that does not satisfy its definition.
type ParamError struct {
	Name string
	Msg  string
}

func (e *ParamError) Error() string { return fmt.Sprintf("parameter %q: %s", e.Name, e.Msg) }

// Convert parses raw as p's type and checks its pattern or allowed values.
func (p Parameter) Convert(raw string) (any, error) {
	if len(raw) > MaxParamLen {
		return nil, &ParamError{p.Name, fmt.Sprintf("value is longer than %d bytes", MaxParamLen)}
	}
	switch p.Type {
	case TypeInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, &ParamError{p.Name, fmt.Sprintf("%q is not an int", raw)}
		}
		return n, nil
	case TypeBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, &ParamError{p.Name, fmt.Sprintf("%q is not a bool (true or false)", raw)}
		}
		return b, nil
	case TypeEnum:
		if !slices.Contains(p.Values, raw) {
			return nil, &ParamError{p.Name, fmt.Sprintf("%q must be one of %s", raw, strings.Join(p.Values, ", "))}
		}
		return raw, nil
	default:
		if p.Pattern != "" {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return nil, &ParamError{p.Name, fmt.Sprintf("invalid pattern: %v", err)}
			}
			if !re.MatchString(raw) {
				return nil, &ParamError{p.Name, fmt.Sprintf("%q does not match %s", raw, p.Pattern)}
			}
		}
		return raw, nil
	}
}

// ResolveParams turns raw string values (from --set or the API) into typed
// parameters. Unknown names are rejected; a missing value takes its default,
// rendered with v's built-in values; every value is converted and checked.
func (t *Template) ResolveParams(v Values, raw map[string]string) (map[string]any, error) {
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.ContainsFunc(t.Parameters, func(p Parameter) bool { return p.Name == name }) {
			return nil, &ParamError{name, "unknown parameter"}
		}
	}
	out := make(map[string]any, len(t.Parameters))
	for _, p := range t.Parameters {
		s, ok := raw[p.Name]
		if !ok {
			d, has := p.DefaultString()
			if !has {
				return nil, &ParamError{p.Name, "required"}
			}
			r, err := t.renderString("default of "+p.Name, d, v)
			if err != nil {
				return nil, &ParamError{p.Name, fmt.Sprintf("default: %v", err)}
			}
			s = r
		}
		val, err := p.Convert(s)
		if err != nil {
			return nil, err
		}
		out[p.Name] = val
	}
	return out, nil
}
```

Create `internal/template/render.go` with the shared rendering primitive (Task 4 adds the rest of the file):

```go
package template

import (
	"strings"
	gotemplate "text/template"
)

// funcs is the whole function set templates may use: nothing reads files,
// runs commands or reaches the network.
var funcs = gotemplate.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"replace":    func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
}

func (t *Template) parse(name, text string) (*gotemplate.Template, error) {
	left, right := t.Delims()
	return gotemplate.New(name).Delims(left, right).Option("missingkey=error").Funcs(funcs).Parse(text)
}

func (t *Template) renderString(name, text string, v Values) (string, error) {
	tp, err := t.parse(name, text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tp.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/template -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/template
git commit -m "template: typed parameters with defaults, patterns and enums

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Rendering a skeleton

**Files:**
- Modify: `internal/template/render.go`
- Test: `internal/template/render_test.go`, `internal/template/testdata/render.golden`
- Modify: `Makefile` (`update-golden`)

**Interfaces:**
- Consumes: `Template`, `Values`, `renderString`, `parse` (Tasks 2–3); `manifest.Parse`, `manifest.FileName` (Task 1).
- Produces:
  - `type File struct { Path string; Mode fs.FileMode; Data []byte }` — `Path` is slash-separated and relative; `Mode` is `0o644` or `0o755`
  - `func (t *Template) Render(skeleton fs.FS, v Values) ([]File, error)` — `skeleton` is the template's `skeleton/` directory (`fs.Sub`); files sorted by `Path`
  - `func (t *Template) CheckSyntax(skeleton fs.FS) []Finding` — parses every path segment and UTF-8 file without executing; reports symlinks and non-regular files; `File` is relative to `skeleton`
  - `func CheckManifest(files []File, service, team string) error`

- [ ] **Step 1: Write the failing tests** — create `internal/template/render_test.go`:

```go
package template

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run 'make update-golden' to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// skeleton exercises every rendering rule: built-ins and typed params, a path
// segment that renders empty, other templating syntax that must survive, Go
// generics next to the delimiters, an executable file and a binary file.
var skeleton = fstest.MapFS{
	"forge.yaml":                          {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {% .Service.Name %}\n  team: {% .Service.Team %}\n")},
	"go.mod":                              {Data: []byte("module {% .Params.goModule %}\n")},
	"cmd/{% .Service.Name %}/main.go":     {Data: []byte("package main // port {% .Params.port %}\nvar _ connect.Request[{% .Service.Name | replace \"-\" \"\" %}v1.Req]\n")},
	".github/workflows/ci.yml":            {Data: []byte("image: ghcr.io/{% .Repo.Owner %}/x:${{ github.sha }} # {{ not.ours }}\n")},
	"{% if .Params.debug %}docs{% end %}/index.md": {Data: []byte("# {% .Service.Name | upper %}\n")},
	"run.sh":                              {Data: []byte("#!/bin/sh\necho {% .Service.Name %}\n"), Mode: 0o755},
	"logo.bin":                            {Data: []byte{0xff, 0xfe, '{', '%', ' ', 0x00}},
}

func renderValues(t *testing.T, tp *Template) Values {
	t.Helper()
	v := sampleBuiltins()
	params, err := tp.ResolveParams(v, map[string]string{"tier": "gold"})
	if err != nil {
		t.Fatal(err)
	}
	v.Params = params
	return v
}

func dump(files []File) []byte {
	var b strings.Builder
	for _, f := range files {
		data := string(f.Data)
		if !utf8.Valid(f.Data) {
			data = fmt.Sprintf("%q\n", f.Data)
		}
		fmt.Fprintf(&b, "== %s %#o\n%s", f.Path, f.Mode, data)
	}
	return []byte(b.String())
}

func TestRenderGolden(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	files, err := tp.Render(skeleton, renderValues(t, tp))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "render", dump(files))
}

func TestRenderErrors(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	v := renderValues(t, tp)
	tests := map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"missing param":   {fstest.MapFS{"a.txt": {Data: []byte("{% .Params.nope %}")}}, `map has no entry for key "nope"`},
		"collision":       {fstest.MapFS{"{% .Service.Name %}.txt": {Data: []byte("a")}, "billing-api.txt": {Data: []byte("b")}}, "both render to billing-api.txt"},
		"dot segment":     {fstest.MapFS{"{% \"..\" %}/x": {Data: []byte("a")}}, `renders to ".."`},
		// The key must not contain "/" itself (MapFS would split it), so the slash comes from replace.
		"slash segment":   {fstest.MapFS{"{% replace \"x\" \"/\" \"axb\" %}": {Data: []byte("a")}}, `renders to "a/b"`},
		"symlink":         {fstest.MapFS{"link": {Data: []byte("x"), Mode: fs.ModeSymlink}}, "symlinks are not allowed"},
		"too large":       {fstest.MapFS{"big": {Data: []byte(strings.Repeat("a", MaxBytes+1))}}, "larger than"},
	}
	for name, tt := range tests {
		if _, err := tp.Render(tt.fsys, v); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", name, err, tt.want)
		}
	}
}

func TestRenderTooManyFiles(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	fsys := fstest.MapFS{}
	for i := 0; i <= MaxFiles; i++ {
		fsys[fmt.Sprintf("f%d", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	if _, err := tp.Render(fsys, renderValues(t, tp)); err == nil || !strings.Contains(err.Error(), "more than 2000 files") {
		t.Errorf("got %v", err)
	}
}

func TestCheckSyntax(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	fsys := fstest.MapFS{
		"ok.txt":            {Data: []byte("{% .Service.Name %}")},
		"bad.txt":           {Data: []byte("{% .Service.Name ")},
		"{% if %}/x.txt":    {Data: []byte("x")},
		"link":              {Data: []byte("x"), Mode: fs.ModeSymlink},
		"logo.bin":          {Data: []byte{0xff, '{', '%'}},
	}
	got := tp.CheckSyntax(fsys)
	var msgs []string
	for _, f := range got {
		msgs = append(msgs, f.String())
	}
	all := strings.Join(msgs, "\n")
	// A broken directory name is reported once, for the directory.
	for _, want := range []string{"bad.txt: ", "{% if %}: ", "link: symlinks are not allowed"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing finding %q in:\n%s", want, all)
		}
	}
	if len(got) != 3 {
		t.Errorf("want 3 findings (ok.txt and logo.bin are fine), got %d:\n%s", len(got), all)
	}
}

func TestCheckManifest(t *testing.T) {
	t.Parallel()
	ok := []File{{Path: "forge.yaml", Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: billing-api\n  team: billing\n")}}
	if err := CheckManifest(ok, "billing-api", "billing"); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := CheckManifest(ok, "other", "billing"); err == nil || !strings.Contains(err.Error(), `want "other"`) {
		t.Errorf("wrong name: %v", err)
	}
	if err := CheckManifest(nil, "a", "b"); err == nil || !strings.Contains(err.Error(), "does not render a forge.yaml") {
		t.Errorf("missing: %v", err)
	}
	bad := []File{{Path: "forge.yaml", Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: a\n  tem: b\n")}}
	if err := CheckManifest(bad, "a", "b"); err == nil || !strings.Contains(err.Error(), "tem") {
		t.Errorf("invalid: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/template -run 'Render|CheckSyntax|CheckManifest' -v`
Expected: FAIL to compile (`tp.Render undefined`).

- [ ] **Step 3: Implement** — replace `internal/template/render.go` with:

```go
package template

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	gotemplate "text/template"
	"unicode/utf8"

	"github.com/showmino/forge/internal/manifest"
)

// funcs is the whole function set templates may use: nothing reads files,
// runs commands or reaches the network.
var funcs = gotemplate.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"replace":    func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
}

// File is one rendered file.
type File struct {
	// Path is slash-separated and relative to the new repo's root.
	Path string
	// Mode is 0o755 for executables and 0o644 otherwise.
	Mode fs.FileMode
	Data []byte
}

func (t *Template) parse(name, text string) (*gotemplate.Template, error) {
	left, right := t.Delims()
	return gotemplate.New(name).Delims(left, right).Option("missingkey=error").Funcs(funcs).Parse(text)
}

func (t *Template) renderString(name, text string, v Values) (string, error) {
	tp, err := t.parse(name, text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tp.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// renderPath renders src one segment at a time. It returns "" when a segment
// renders empty, which drops the file or directory.
func (t *Template) renderPath(src string, v Values) (string, error) {
	segs := strings.Split(src, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		r, err := t.renderString(src, s, v)
		if err != nil {
			return "", fmt.Errorf("path %s: %w", src, err)
		}
		if r == "" {
			return "", nil
		}
		if r == "." || r == ".." || strings.ContainsAny(r, `/\`) {
			return "", fmt.Errorf("path %s: a segment renders to %q", src, r)
		}
		out = append(out, r)
	}
	return strings.Join(out, "/"), nil
}

// Render renders every file in skeleton (the template's skeleton/ directory)
// with v. Files that are not valid UTF-8 are copied unchanged.
func (t *Template) Render(skeleton fs.FS, v Values) ([]File, error) {
	var files []File
	from := map[string]string{} // rendered path -> source path
	total := 0
	err := fs.WalkDir(skeleton, ".", func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if src == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are not allowed", src)
		}
		dst, err := t.renderPath(src, v)
		if err != nil {
			return err
		}
		if dst == "" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed", src)
		}
		if prev, ok := from[dst]; ok {
			return fmt.Errorf("%s and %s both render to %s", prev, src, dst)
		}
		from[dst] = src
		if len(files) == MaxFiles {
			return fmt.Errorf("the skeleton renders more than %d files", MaxFiles)
		}
		data, err := fs.ReadFile(skeleton, src)
		if err != nil {
			return err
		}
		if utf8.Valid(data) {
			out, err := t.renderString(src, string(data), v)
			if err != nil {
				return err
			}
			data = []byte(out)
		}
		total += len(data)
		if total > MaxBytes {
			return fmt.Errorf("rendered output is larger than %d bytes", MaxBytes)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, File{Path: dst, Mode: mode, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// CheckSyntax parses every path segment and every UTF-8 file in skeleton
// without executing them, so lint can report each broken file, not just the
// first. Findings' File is relative to skeleton.
func (t *Template) CheckSyntax(skeleton fs.FS) []Finding {
	var out []Finding
	add := func(file string, err error) { out = append(out, Finding{File: file, Message: err.Error()}) }
	err := fs.WalkDir(skeleton, ".", func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if src == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			add(src, errors.New("symlinks are not allowed"))
			return nil
		}
		if _, err := t.parse(src, path.Base(src)); err != nil {
			add(src, err) // each entry checks only its own name
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			add(src, errors.New("only regular files are allowed"))
			return nil
		}
		data, err := fs.ReadFile(skeleton, src)
		if err != nil {
			add(src, err)
			return nil
		}
		if utf8.Valid(data) {
			if _, err := t.parse(src, string(data)); err != nil {
				add(src, err)
			}
		}
		return nil
	})
	if err != nil {
		add(".", err)
	}
	return out
}

// CheckManifest requires files to contain a valid forge.yaml at the root that
// names the given service and team.
func CheckManifest(files []File, service, team string) error {
	i := slices.IndexFunc(files, func(f File) bool { return f.Path == manifest.FileName })
	if i < 0 {
		return errors.New("the skeleton does not render a forge.yaml at its root")
	}
	m, err := manifest.Parse(files[i].Data)
	if err != nil {
		return fmt.Errorf("rendered forge.yaml: %w", err)
	}
	if m.Service.Name != service || m.Service.Team != team {
		return fmt.Errorf("rendered forge.yaml names service %q and team %q; want %q and %q", m.Service.Name, m.Service.Team, service, team)
	}
	return nil
}
```

`CheckSyntax` checks each directory and file entry's own name (`path.Base`) once, so a broken directory name is reported for the directory, not again for every file below it. Unlike `Render`, it keeps walking after a problem, so lint lists every broken file.

- [ ] **Step 4: Create the golden file and review it by eye**

Run: `go test ./internal/template -run TestRenderGolden -update && cat internal/template/testdata/render.golden`
Expected content (check each line; this is the behaviour the spec requires):

```
== .github/workflows/ci.yml 0644
image: ghcr.io/showmino/x:${{ github.sha }} # {{ not.ours }}
== cmd/billing-api/main.go 0644
package main // port 8080
var _ connect.Request[billingapiv1.Req]
== forge.yaml 0644
apiVersion: forge.dev/v1
service:
  name: billing-api
  team: billing
== go.mod 0644
module github.com/showmino/billing-api
== logo.bin 0644
"\xff\xfe{% \x00"
== run.sh 0755
#!/bin/sh
echo billing-api
```

`docs/index.md` is absent because `debug` defaults to `false`. If the file differs, fix the code, delete the golden file and regenerate.

- [ ] **Step 5: Run all template tests**

Run: `go test ./internal/template -v`
Expected: PASS.

- [ ] **Step 6: Make `make update-golden` cover the new golden files** — in `Makefile` change the `update-golden` recipe line to:

```make
	go test ./internal/output ./internal/template ./internal/cli -update
```

(The recipe line starts with a tab. `./internal/cli` gets its golden files in Task 6; until then `-update` is accepted by the flag defined there.) Run `make update-golden` only after Task 6 exists; for now run `go test ./internal/template -update` when needed.

- [ ] **Step 7: Commit**

```bash
git add internal/template Makefile
git commit -m "template: render skeletons, check syntax and the rendered forge.yaml

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Lint and discovery

**Files:**
- Create: `internal/template/lint.go`
- Test: `internal/template/lint_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–4.
- Produces:
  - `func SampleValues(t *Template) Values` — `Service{Name: "sample-service", Team: "sample-team", Owners: ["owner@example.com"]}`, `Repo{Owner: "sample-org", Name: "sample-service", URL: "https://github.com/sample-org/sample-service"}`, `Template{Name: t.Name, Version: "v0.0.0"}`, `Params` nil
  - `func Lint(fsys fs.FS, dir string) ([]File, []Finding)` — `dir` is the template's directory name inside `fsys` (never `"."`); returns the sample render when there are no findings; every finding has `Template: dir` and `File` prefixed with `dir/`
  - `func Discover(fsys fs.FS) ([]string, error)` — direct subdirectories of `fsys` that contain `template.yaml`, sorted

- [ ] **Step 1: Write the failing tests** — create `internal/template/lint_test.go`:

```go
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
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/template -run 'Lint|Discover' -v`
Expected: FAIL to compile (`undefined: Lint`).

- [ ] **Step 3: Implement** — create `internal/template/lint.go`:

```go
package template

import (
	"fmt"
	"io/fs"
	"path"
)

// SampleValues are the built-in values lint renders with. The service name
// contains a dash on purpose: templates must cope with one.
func SampleValues(t *Template) Values {
	return Values{
		Service:  ServiceValues{Name: "sample-service", Team: "sample-team", Owners: []string{"owner@example.com"}},
		Repo:     RepoValues{Owner: "sample-org", Name: "sample-service", URL: "https://github.com/sample-org/sample-service"},
		Template: TemplateValues{Name: t.Name, Version: "v0.0.0"},
	}
}

// sample is the value lint uses for a required parameter.
func sample(p Parameter) string {
	switch p.Type {
	case TypeInt:
		return "1"
	case TypeBool:
		return "false"
	case TypeEnum:
		if len(p.Values) > 0 {
			return p.Values[0]
		}
	}
	return "sample"
}

// sampleParams resolves every parameter from its default, or a sample for
// required ones, reporting each parameter that fails.
func (t *Template) sampleParams(v Values) (map[string]any, []Finding) {
	params := map[string]any{}
	var out []Finding
	for _, p := range t.Parameters {
		raw, hasDefault := p.DefaultString()
		if hasDefault {
			r, err := t.renderString("default of "+p.Name, raw, v)
			if err != nil {
				out = append(out, Finding{File: FileName, Line: p.Line, Message: fmt.Sprintf("parameter %q: default: %v", p.Name, err)})
				continue
			}
			raw = r
		} else {
			raw = sample(p)
		}
		val, err := p.Convert(raw)
		if err != nil {
			msg := err.Error()
			if !hasDefault {
				msg += fmt.Sprintf(" (lint renders required parameters with the sample %q; give it a default)", raw)
			}
			out = append(out, Finding{File: FileName, Line: p.Line, Message: msg})
			continue
		}
		params[p.Name] = val
	}
	return params, out
}

// Lint checks the template in directory dir of fsys and returns its sample
// render, or every problem found. dir must be the template's directory name.
func Lint(fsys fs.FS, dir string) ([]File, []Finding) {
	var out []Finding
	add := func(found []Finding) {
		for _, f := range found {
			f.Template = dir
			f.File = path.Join(dir, f.File)
			out = append(out, f)
		}
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, FileName))
	if err != nil {
		add([]Finding{{File: FileName, Message: err.Error()}})
		return nil, out
	}
	t, err := Parse(data)
	if err != nil {
		add([]Finding{{File: FileName, Message: err.Error()}})
		return nil, out
	}
	add(t.Validate())
	if t.Name != dir {
		add([]Finding{{File: FileName, Message: fmt.Sprintf("name %q must equal the directory name %q", t.Name, dir)}})
	}
	skelDir := path.Join(dir, SkeletonDir)
	if entries, err := fs.ReadDir(fsys, skelDir); err != nil || len(entries) == 0 {
		add([]Finding{{File: SkeletonDir, Message: "skeleton/ must exist and not be empty"}})
		return nil, out
	}
	if len(out) > 0 {
		return nil, out // rendering needs a valid template.yaml
	}
	skel, err := fs.Sub(fsys, skelDir)
	if err != nil {
		add([]Finding{{File: SkeletonDir, Message: err.Error()}})
		return nil, out
	}
	syntax := t.CheckSyntax(skel)
	for i := range syntax {
		syntax[i].File = path.Join(SkeletonDir, syntax[i].File)
	}
	add(syntax)
	v := SampleValues(t)
	params, pf := t.sampleParams(v)
	add(pf)
	if len(out) > 0 {
		return nil, out
	}
	v.Params = params
	files, err := t.Render(skel, v)
	if err != nil {
		add([]Finding{{File: SkeletonDir, Message: err.Error()}})
		return nil, out
	}
	if err := CheckManifest(files, v.Service.Name, v.Service.Team); err != nil {
		add([]Finding{{File: path.Join(SkeletonDir, "forge.yaml"), Message: err.Error()}})
		return nil, out
	}
	return files, nil
}

// Discover returns the direct subdirectories of fsys that hold a template.yaml.
func Discover(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(fsys, path.Join(e.Name(), FileName)); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}
```

- [ ] **Step 4: Run all template tests**

Run: `go test ./internal/template -v`
Expected: PASS. The "bad default" case expects line 7 (`  - name: port` in `goodTemplate`) and "sample fails pattern" expects line 10 (`  - name: module`); if a line number differs, recount `goodTemplate` and fix the test.

- [ ] **Step 5: Run the gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint`
Expected: `0 issues.` and all packages ok.

```bash
git add internal/template
git commit -m "template: lint templates and discover them in a directory

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `forge template lint`

**Files:**
- Create: `internal/cli/template.go`
- Modify: `internal/cli/root.go` (the "core" loop in `NewRoot`)
- Test: `internal/cli/template_test.go`, `internal/cli/testdata/template_lint_table.golden`, `internal/cli/testdata/template_lint_json.golden`
- Modify: `README.md` (CLI table and a "Templates" section), `.claude/skills/forge/SKILL.md` (status)

**Interfaces:**
- Consumes: `template.Lint`, `template.Discover`, `template.FileName`, `template.Finding`, `template.File` (Tasks 2–5); `Factory.Format`, `Factory.Getwd`, `Factory.infof`, `group`, `exitcode`, `output`.
- Produces: `func newTemplateCmd(f *Factory) *cobra.Command` (the `template` group; milestone 2 adds `list` and `get` to it).

- [ ] **Step 1: Write the failing tests** — create `internal/cli/template_test.go`:

```go
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
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cli -run TemplateLint -v`
Expected: FAIL: `unknown command "template" for "forge"` (exit 2 where 0 was wanted).

- [ ] **Step 3: Implement** — create `internal/cli/template.go`:

```go
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/template"
)

func newTemplateCmd(f *Factory) *cobra.Command {
	cmd := group("template", "Inspect and lint service templates", "templates")
	cmd.Long = `Templates scaffold new services with 'forge create'. A template is a directory
with a template.yaml (typed parameters) and a skeleton/ rendered with Go
text/template. The platform team releases them by tagging the template repo.`
	cmd.AddCommand(newTemplateLintCmd(f))
	return cmd
}

func newTemplateLintCmd(f *Factory) *cobra.Command {
	var renderDir string
	cmd := &cobra.Command{
		Use:   "lint [dir]",
		Short: "Check templates for errors, without the API",
		Long: `Check one template (a directory with a template.yaml) or every template in a
directory of templates (default: the current directory). Lint checks
template.yaml and its parameters, parses every skeleton file and path, renders
the template with sample values and checks the rendered forge.yaml. It needs
no API and no login.

Exits 0 when there are no problems and 1 when there are.`,
		Example: `  forge template lint                  # every template in the current directory
  forge template lint go-grpc
  forge template lint -o json | jq -r '.findings[].message'`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			wd, err := f.Getwd()
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			fsys, names, err := findTemplates(absFrom(wd, dir))
			if err != nil {
				return err
			}
			if renderDir != "" {
				renderDir = absFrom(wd, renderDir)
			}
			var findings []template.Finding
			for _, name := range names {
				files, found := template.Lint(fsys, name)
				findings = append(findings, found...)
				if renderDir != "" && len(found) == 0 {
					if err := writeRender(filepath.Join(renderDir, name), files); err != nil {
						return err
					}
				}
			}
			if err := printFindings(f, format, findings); err != nil {
				return err
			}
			if len(findings) > 0 {
				return exitcode.New(exitcode.Unexpected, "%s in %s", plural(len(findings), "problem"), plural(len(names), "template"))
			}
			f.infof("%s checked, no problems.", plural(len(names), "template"))
			return nil
		},
	}
	cmd.Flags().StringVar(&renderDir, "render-dir", "", "also write each clean template's sample render to DIR/<template>")
	_ = cmd.Flags().MarkHidden("render-dir")
	return cmd
}

func absFrom(wd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(wd, p)
}

// findTemplates returns dir itself when it holds a template.yaml, else its
// direct subdirectories that do, with an fs.FS rooted at their parent.
func findTemplates(dir string) (fs.FS, []string, error) {
	if _, err := os.Stat(filepath.Join(dir, template.FileName)); err == nil {
		return os.DirFS(filepath.Dir(dir)), []string{filepath.Base(dir)}, nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil, exitcode.Usagef("%s is not a directory", dir)
	}
	fsys := os.DirFS(dir)
	names, err := template.Discover(fsys)
	if err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return nil, nil, exitcode.Usagef("no %s in %s or its subdirectories", template.FileName, dir).
			WithHint("pass a template directory, or a directory of templates")
	}
	return fsys, names, nil
}

func writeRender(dir string, files []template.File) error {
	if _, err := os.Stat(dir); err == nil {
		return exitcode.New(exitcode.Conflict, "%s already exists", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, file := range files {
		p := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // a source tree for the user to build
			return err
		}
		if err := os.WriteFile(p, file.Data, file.Mode); err != nil { //nolint:gosec // modes come from the template: 0644 or 0755
			return err
		}
	}
	return nil
}

func printFindings(f *Factory, format output.Format, findings []template.Finding) error {
	if findings == nil {
		findings = []template.Finding{}
	}
	if format == output.Table {
		for _, fd := range findings {
			if _, err := fmt.Fprintln(f.IO.Out, fd.String()); err != nil {
				return err
			}
		}
		return nil
	}
	names := make([]string, 0, len(findings))
	for _, fd := range findings {
		n := fd.File
		if fd.Line > 0 {
			n = fmt.Sprintf("%s:%d", fd.File, fd.Line)
		}
		names = append(names, n)
	}
	return output.Print(f.IO.Out, format, output.View{
		Names: names,
		Data:  struct {
			Findings []template.Finding `json:"findings"`
		}{findings},
	})
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
```

In `internal/cli/root.go`, change the core command loop to:

```go
	for _, c := range []*cobra.Command{newServiceCmd(f), newTemplateCmd(f)} {
```

- [ ] **Step 4: Create the golden files and review them**

Run: `go test ./internal/cli -run TestTemplateLint -update && cat internal/cli/testdata/template_lint_table.golden internal/cli/testdata/template_lint_json.golden`
Expected table golden:

```
bad/template.yaml:7: parameter "port": "eighty" is not an int
```

Expected JSON golden:

```json
{
  "findings": [
    {
      "template": "bad",
      "file": "bad/template.yaml",
      "line": 7,
      "message": "parameter \"port\": \"eighty\" is not an int"
    }
  ]
}
```

If either differs, fix the code (not the golden) and regenerate.

- [ ] **Step 5: Run the CLI tests**

Run: `go test ./internal/cli -v -run 'Template|Usage|Version'`
Expected: PASS.

- [ ] **Step 6: Document** — in `README.md`'s "Using the CLI" table add the row:

```markdown
| `template` | `lint` (Phase 2: `list`, `get`, and `forge create`) |
```

and after the "Using the CLI" section add:

```markdown
## Templates

Templates live in [`showmino/forge-templates`](https://github.com/showmino/forge-templates):
one directory per template with a `template.yaml` (typed parameters) and a
`skeleton/` rendered with Go `text/template`, released by repo-wide semver
tags. `forge template lint [dir]` checks them locally; see
`docs/specs/2026-10-08-phase-2-templates-design.md` for the format.
```

In `.claude/skills/forge/SKILL.md`, under "Status and roadmap", change the Phase 2 bullet's first sentence to start with "**Phase 2 (in progress):** milestone 1 (`internal/template`, `forge template lint`, `go-grpc` in `showmino/forge-templates`) per `docs/plans/2026-10-08-phase-2-m1-template-lint.md`;" and keep the rest.

- [ ] **Step 7: Run the gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint && make update-golden && git status --short`
Expected: gate passes; `make update-golden` leaves no changes in `git status` for existing golden files.

```bash
git add internal/cli Makefile README.md .claude/skills/forge/SKILL.md
git commit -m "cli: forge template lint

Checks one template or a directory of templates locally, with table,
json and name output; exits 1 on findings and 2 when there is no
template. A hidden --render-dir writes each template's sample render
for the template repo's CI.

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: The `go-grpc` template

**Files** (all in the new repo `~/code/forge-templates`, remote `showmino/forge-templates`):
- Create: `README.md`, `go-grpc/template.yaml`, and under `go-grpc/skeleton/`: `forge.yaml`, `README.md`, `.gitignore`, `go.mod`, `go.sum`, `buf.yaml`, `buf.gen.yaml`, `proto/{% .Service.Name | replace "-" "" %}/v1/greeter.proto`, `cmd/{% .Service.Name %}/main.go`, `internal/server/greeter.go`, `internal/server/greeter_test.go`, `Dockerfile`, `Makefile`, `.golangci.yml`, `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `forge template lint --render-dir` (Task 6), built as `~/code/forge/bin/forge`.
- Produces: tag `v1.0.0` of `showmino/forge-templates` containing template `go-grpc` (milestone 2's `GitHubSource` reads it).

The proto package name is the service name without dashes (`billing-api` → `billingapi.v1`, Go package `billingapiv1`): Go and proto identifiers cannot contain `-`. Every Go and proto file defines `$pkg` on its first line for this.

- [ ] **Step 1: Clone the empty repo**

```bash
cd ~/code && git clone git@github.com:showmino/forge-templates.git && cd forge-templates
```

Expected: "You appear to have cloned an empty repository."

- [ ] **Step 2: Write `go-grpc/template.yaml`**

```yaml
apiVersion: forge.dev/v1
kind: Template
name: go-grpc
title: Go gRPC service
description: ConnectRPC service in Go with CI, Dockerfile and forge.yaml
tags: [go, grpc]
owner: platform
# {% %} because Go generics clash with [[ ]] and GitHub Actions uses ${{ }}.
delimiters: ["{%", "%}"]
parameters:
  - name: goModule
    type: string
    default: "github.com/{% .Repo.Owner %}/{% .Repo.Name %}"
    pattern: '^[a-z0-9][a-z0-9./_-]*$'
    description: Go module path
  - name: port
    type: int
    default: 8080
    description: Port the server listens on
```

- [ ] **Step 3: Write the skeleton's project files**

`go-grpc/skeleton/forge.yaml`:

```yaml
# Forge service manifest. See 'forge init --help'.
apiVersion: forge.dev/v1
service:
  name: {% .Service.Name %}
  team: {% .Service.Team %}
{%- if .Service.Owners %}
  owners:
{%- range .Service.Owners %}
    - {% . %}
{%- end %}
{%- end %}
  lifecycle: experimental
  repo: {% .Repo.URL %}
```

`go-grpc/skeleton/.gitignore`:

```
/bin/
/gen/
```

`go-grpc/skeleton/go.mod`:

```
module {% .Params.goModule %}

go 1.24

require (
	connectrpc.com/connect v1.18.1
	google.golang.org/protobuf v1.36.5
)
```

`go-grpc/skeleton/buf.yaml`:

```yaml
version: v2
modules:
  - path: proto
lint:
  use: [STANDARD]
breaking:
  use: [FILE]
```

`go-grpc/skeleton/buf.gen.yaml`:

```yaml
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: {% .Params.goModule %}/gen
clean: true
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
inputs:
  - directory: proto
```

`go-grpc/skeleton/.golangci.yml`:

```yaml
version: "2"
linters:
  enable: [bodyclose, errorlint, gocritic, gosec, misspell, noctx, unconvert]
  exclusions:
    generated: strict
formatters:
  enable: [gofmt, goimports]
```

`go-grpc/skeleton/Makefile` (recipe lines start with a tab):

```make
.DEFAULT_GOAL := build
BIN := bin

.PHONY: tools generate build test lint image

tools: ## Install buf and the protoc plugins at the versions CI uses
	go install github.com/bufbuild/buf/cmd/buf@v1.47.2
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1

generate: ## Generate gen/ from proto/ (gen/ is not committed)
	buf generate

build: generate
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/ ./cmd/...

test: generate
	go test -race ./...

lint: generate
	golangci-lint run ./...
	buf lint

image: generate
	docker build -t {% .Service.Name %}:dev .
```

`go-grpc/skeleton/Dockerfile`:

```dockerfile
# Run 'make generate' first: gen/ is generated, not committed.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/{% .Service.Name %} ./cmd/{% .Service.Name %}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/{% .Service.Name %} /{% .Service.Name %}
USER 65532:65532
EXPOSE {% .Params.port %}
ENTRYPOINT ["/{% .Service.Name %}"]
```

`go-grpc/skeleton/.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: make tools generate
      - run: go test -race ./...
      - uses: golangci/golangci-lint-action@v8
        with:
          version: v2.5.0
      - run: buf lint

  image:
    if: github.event_name == 'push'
    needs: test
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: make tools generate
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build and push
        run: |
          image=ghcr.io/{% .Repo.Owner %}/{% .Service.Name %}:${GITHUB_SHA::7}
          docker build -t "$image" .
          docker push "$image"
```

`go-grpc/skeleton/README.md` (the first line defines `$pkg` and renders nothing):

````markdown
{%- $pkg := .Service.Name | replace "-" "" -%}
# {% .Service.Name %}

Owned by **{% .Service.Team %}**. Scaffolded by `forge create` from the
`{% .Template.Name %}` template ({% .Template.Version %}).

## Develop

```sh
make tools        # once: buf and the protoc plugins
make test lint    # generates gen/ first
go run ./cmd/{% .Service.Name %}
curl -s localhost:{% .Params.port %}/{% $pkg %}.v1.GreeterService/Greet \
  -H 'Content-Type: application/json' -d '{"name":"you"}'
```

The API is defined in `proto/`; `gen/` is generated and not committed. CI
tests every push and, on `main`, pushes
`ghcr.io/{% .Repo.Owner %}/{% .Service.Name %}:<short-sha>`.
````

- [ ] **Step 4: Write the Go and proto files**

`go-grpc/skeleton/proto/{% .Service.Name | replace "-" "" %}/v1/greeter.proto` (create the directory with that literal name):

```proto
syntax = "proto3";

package {% .Service.Name | replace "-" "" %}.v1;

// GreeterService is an example; replace it with your service's API.
service GreeterService {
  // Greet returns a greeting for name.
  rpc Greet(GreetRequest) returns (GreetResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

message GreetRequest {
  string name = 1;
}

message GreetResponse {
  string greeting = 1;
}
```

`go-grpc/skeleton/cmd/{% .Service.Name %}/main.go`:

```go
{%- $pkg := .Service.Name | replace "-" "" -%}
// Command {% .Service.Name %} serves the {% .Service.Name %} API over Connect,
// gRPC and gRPC-Web.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"{% .Params.goModule %}/gen/{% $pkg %}/v1/{% $pkg %}v1connect"
	"{% .Params.goModule %}/internal/server"
)

func main() {
	addr := flag.String("addr", ":{% .Params.port %}", "listen address")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*addr, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(addr string, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle({% $pkg %}v1connect.NewGreeterServiceHandler(&server.Greeter{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC clients without TLS
	srv := &http.Server{Addr: addr, Handler: mux, Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

`go-grpc/skeleton/internal/server/greeter.go`:

```go
{%- $pkg := .Service.Name | replace "-" "" -%}
// Package server implements the {% .Service.Name %} API.
package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	{% $pkg %}v1 "{% .Params.goModule %}/gen/{% $pkg %}/v1"
)

// Greeter implements GreeterService.
type Greeter struct{}

// Greet returns a greeting for the request's name.
func (*Greeter) Greet(_ context.Context, req *connect.Request[{% $pkg %}v1.GreetRequest]) (*connect.Response[{% $pkg %}v1.GreetResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	return connect.NewResponse(&{% $pkg %}v1.GreetResponse{Greeting: "Hello, " + name + "!"}), nil
}
```

`go-grpc/skeleton/internal/server/greeter_test.go`:

```go
{%- $pkg := .Service.Name | replace "-" "" -%}
package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	{% $pkg %}v1 "{% .Params.goModule %}/gen/{% $pkg %}/v1"
	"{% .Params.goModule %}/gen/{% $pkg %}/v1/{% $pkg %}v1connect"
	"{% .Params.goModule %}/internal/server"
)

func TestGreet(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle({% $pkg %}v1connect.NewGreeterServiceHandler(&server.Greeter{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := {% $pkg %}v1connect.NewGreeterServiceClient(srv.Client(), srv.URL)

	resp, err := client.Greet(context.Background(), connect.NewRequest(&{% $pkg %}v1.GreetRequest{Name: "Forge"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetGreeting(); got != "Hello, Forge!" {
		t.Errorf("greeting = %q", got)
	}
	_, err = client.Greet(context.Background(), connect.NewRequest(&{% $pkg %}v1.GreetRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty name: %v", err)
	}
}
```

- [ ] **Step 5: Lint and render with the sample service**

```bash
cd ~/code/forge-templates
~/code/forge/bin/forge template lint . --render-dir /tmp/forge-render
```

Expected: stderr `1 template checked, no problems.`; `/tmp/forge-render/go-grpc/cmd/sample-service/main.go` exists and imports `.../gen/sampleservice/v1/sampleservicev1connect`.

- [ ] **Step 6: Produce `go.sum` and build the rendered service**

```bash
cd /tmp/forge-render/go-grpc
make tools generate
go mod tidy
cp go.sum ~/code/forge-templates/go-grpc/skeleton/go.sum
diff <(tail -n +2 go.mod) <(tail -n +2 ~/code/forge-templates/go-grpc/skeleton/go.mod)
GOTOOLCHAIN=go1.24.13 make test lint
```

Expected: the `diff` (everything after the `module` line) prints nothing, and `make test lint` passes (`ok .../internal/server`, `0 issues.`, `buf lint` silent). If the `diff` prints lines, `go mod tidy` changed the requirements: copy them into the skeleton's `go.mod`, keeping its `module {% .Params.goModule %}` line. `go.sum` holds only dependency hashes, so it is not templated. Then rerun Step 5 with a fresh `--render-dir` and repeat this step until it passes unchanged.

- [ ] **Step 7: Run it once by hand**

```bash
cd /tmp/forge-render/go-grpc && go run ./cmd/sample-service -addr :18080 &
sleep 3
curl -s localhost:18080/sampleservice.v1.GreeterService/Greet -H 'Content-Type: application/json' -d '{"name":"Forge"}'
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/healthz
kill %1
```

Expected: `{"greeting":"Hello, Forge!"}` and `200`.

- [ ] **Step 8: Write the repo README** — `~/code/forge-templates/README.md`:

````markdown
# Forge templates

Service templates for `forge create`. Each directory is one template: a
`template.yaml` with typed parameters and a `skeleton/` rendered with Go
`text/template` (this repo uses `{% %}` delimiters). The format is specified in
`showmino/forge`, `docs/specs/2026-10-08-phase-2-templates-design.md`.

Releases are semver tags on the whole repo; `forge create` uses the newest
unless a version is pinned (`--template go-grpc@v1.0.0`).

## Check a change

```sh
forge template lint . --render-dir /tmp/render   # lint and render with sample values
cd /tmp/render/go-grpc && make tools test lint   # build what the template produces
```

CI runs both on every pull request. Tag only after CI passes on `main`.
````

- [ ] **Step 9: Commit, push and tag**

```bash
cd ~/code/forge-templates
git add -A
git commit -m "go-grpc template v1.0.0

ConnectRPC service with buf, tests, Dockerfile, forge.yaml and GitHub
Actions CI that tests every push and pushes an image on main.

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push -u origin main
```

Do not tag yet: Task 8's CI must pass on `main` first.

---

### Task 8: Template repo CI and the `v1.0.0` tag

**Blocked on the owner:** the org secret `FORGE_READ_TOKEN` (a fine-grained token with Contents: read on `showmino/forge`). Until it exists the workflow fails at the second checkout. Also, until branch `phase-2` is merged into `forge`'s `main`, set the repo variable `FORGE_REF` to `phase-2` (`gh variable set FORGE_REF --body phase-2 -R showmino/forge-templates`) so CI builds a `forge` that has `template lint`.

**Files:**
- Create: `~/code/forge-templates/.github/workflows/lint.yml`

**Interfaces:**
- Consumes: `forge template lint --render-dir` (Task 6), the skeleton's `make tools test lint` (Task 7).
- Produces: a green CI run on `main`, then tag `v1.0.0`.

- [ ] **Step 1: Write the workflow**

```yaml
name: lint
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          path: templates
      - uses: actions/checkout@v4
        with:
          repository: showmino/forge
          ref: ${{ vars.FORGE_REF || 'main' }}
          token: ${{ secrets.FORGE_READ_TOKEN }}
          path: forge
      - uses: actions/setup-go@v5
        with:
          go-version-file: forge/go.mod
      - name: Build forge
        working-directory: forge
        run: go build -o "$RUNNER_TEMP/forge" ./cmd/forge
      - name: Lint and render every template
        run: '"$RUNNER_TEMP/forge" template lint templates --render-dir "$RUNNER_TEMP/render"'
      - name: Install golangci-lint
        run: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
      - name: Build and test each rendered template
        run: |
          for dir in "$RUNNER_TEMP"/render/*/; do
            echo "::group::$dir"
            (cd "$dir" && make tools test lint)
            echo "::endgroup::"
          done
```

- [ ] **Step 2: Push and watch the run**

```bash
cd ~/code/forge-templates
git add .github/workflows/lint.yml
git commit -m "ci: lint templates and build what they render

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push
gh run watch -R showmino/forge-templates --exit-status "$(gh run list -R showmino/forge-templates -L1 --json databaseId -q '.[0].databaseId')"
```

Expected: the run succeeds. If it fails at "Build forge" with a checkout error, the token or `FORGE_REF` is missing (see "Blocked on the owner").

- [ ] **Step 3: Tag the release**

```bash
git tag -a v1.0.0 -m "go-grpc v1.0.0"
git push origin v1.0.0
```

- [ ] **Step 4: Prove a broken template fails CI** (then discard it)

```bash
git switch -c ci-check
sed -i 's/default: 8080/default: eighty/' go-grpc/template.yaml
git commit -am "ci check: must fail" && git push -u origin ci-check
gh pr create -R showmino/forge-templates --title "CI check (do not merge)" --body "Must fail lint." --head ci-check
gh pr checks --watch -R showmino/forge-templates ci-check; echo "exit $?"
gh pr close ci-check -R showmino/forge-templates --delete-branch
git switch main && git branch -D ci-check
```

Expected: the check fails with `go-grpc/template.yaml:16: parameter "port": "eighty" is not an int` in the log and `exit 1`; the PR is closed and the branch deleted.

---

## After Milestone 1

Milestone 2 (`TemplateService`, `forge template list / get`), milestone 3 (`internal/github`, `CreateService`, `forge create`) and milestone 4 (k3s and the exit gate) get their own plans, written against the interfaces this milestone produced: `template.Parse`, `Template.ResolveParams`, `Template.Render`, `CheckManifest`, `Lint`, `Discover` and the `template` CLI group.
