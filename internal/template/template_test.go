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
		"inside a param":  strings.Replace(validYAML, "    default: 8080", "    defaults: 8080", 1),
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
		{"type", "    type: bool", "    type: float", `type must be string, int, bool, enum or list`},
		{"enum values", "    values: [gold, silver]", "    values: []", "needs values"},
		{"pattern type", "    type: int\n    default: 8080", "    type: int\n    pattern: '^1'\n    default: 8080", "pattern is only allowed for type string"},
		{"bad pattern", `pattern: '^[a-z0-9./-]+$'`, `pattern: '('`, "invalid pattern"},
		{"unknown option", `delimiters: ["{%", "%}"]`, "delimiters: [\"{%\", \"%}\"]\noptions: [replicas, port]", `option "port" must be one of`},
		{"option twice", `delimiters: ["{%", "%}"]`, "delimiters: [\"{%\", \"%}\"]\noptions: [jobs, jobs]", `option "jobs" must be one of`},
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
	if tp, err := Parse([]byte(strings.Replace(validYAML, `delimiters: ["{%", "%}"]`, "delimiters: [\"{%\", \"%}\"]\noptions: [replicas, secrets, jobs]", 1))); err != nil || len(tp.Validate()) != 0 || len(tp.Options) != 3 {
		t.Errorf("options: %v %v", tp, err)
	}
	if got := (Finding{File: "a/template.yaml", Line: 3, Message: "bad"}).String(); got != "a/template.yaml:3: bad" {
		t.Errorf("with line: %q", got)
	}
	if got := (Finding{File: "a/skeleton", Message: "bad"}).String(); got != "a/skeleton: bad" {
		t.Errorf("without line: %q", got)
	}
}
