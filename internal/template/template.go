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
	"slices"
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
	// TypeList is several of Values, given comma-separated; it renders as []string.
	TypeList ParamType = "list"
)

// Template types.
const (
	// TypeService makes a new repo and registers a service.
	TypeService = "service"
	// TypeFile adds the rendered files to Target by pull request.
	TypeFile = "file"
)

// Template is the content of template.yaml.
type Template struct {
	APIVersion  string   `yaml:"apiVersion"`
	Kind        string   `yaml:"kind"`
	Name        string   `yaml:"name"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
	Owner       string   `yaml:"owner,omitempty"`
	// Type is TypeService (also when empty) or TypeFile.
	Type string `yaml:"type,omitempty"`
	// Target is the "owner/repo" a file template adds its files to.
	Target     string      `yaml:"target,omitempty"`
	Delimiters []string    `yaml:"delimiters,omitempty"`
	Parameters []Parameter `yaml:"parameters,omitempty"`
	// Options are the run options of forge.yaml (manifest.Options) the create
	// form offers for this template.
	Options []string `yaml:"options,omitempty"`
}

// Parameter is one typed input of a template.
type Parameter struct {
	Name        string    `yaml:"name"`
	Type        ParamType `yaml:"type"`
	Default     any       `yaml:"default,omitempty"`
	Pattern     string    `yaml:"pattern,omitempty"`
	Values      []string  `yaml:"values,omitempty"`
	Description string    `yaml:"description,omitempty"`
	// Label, Placeholder and Group are for forms: a display name, an example
	// value and a heading to show the parameter under.
	Label       string `yaml:"label,omitempty"`
	Placeholder string `yaml:"placeholder,omitempty"`
	Group       string `yaml:"group,omitempty"`
	// Min and Max bound an int.
	Min *int `yaml:"min,omitempty"`
	Max *int `yaml:"max,omitempty"`
	// Line is where the parameter starts in template.yaml, for findings.
	Line int `yaml:"-"`
}

var parameterKeys = map[string]bool{
	"name": true, "type": true, "default": true, "pattern": true, "values": true, "description": true,
	"label": true, "placeholder": true, "group": true, "min": true, "max": true,
}

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
// A list's default is comma-separated, as a list is given.
func (p Parameter) DefaultString() (string, bool) {
	if p.Default == nil {
		return "", false
	}
	if l, ok := p.Default.([]any); ok {
		s := make([]string, len(l))
		for i, v := range l {
			s[i] = fmt.Sprint(v)
		}
		return strings.Join(s, ","), true
	}
	return fmt.Sprint(p.Default), true
}

// IsFile reports whether t adds files to an existing repo.
func (t *Template) IsFile() bool { return t.Type == TypeFile }

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
	ownerRepo  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
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
	for i, o := range t.Options {
		if !manifest.ValidOption(o) || slices.Contains(t.Options[:i], o) {
			add(0, "option %q must be one of %s, listed once", o, strings.Join(manifest.Options, ", "))
		}
	}
	switch t.Type {
	case "", TypeService:
		if t.Target != "" {
			add(0, "target is only for a file template")
		}
	case TypeFile:
		if t.Target == "" {
			add(0, "a file template needs target: the owner/repo its files go to")
		} else if !ownerRepo.MatchString(t.Target) {
			add(0, "target %q must be owner/repo", t.Target)
		}
		if len(t.Options) > 0 {
			add(0, "options are only for a service template")
		}
	default:
		add(0, "type must be %q or %q, got %q", TypeService, TypeFile, t.Type)
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
		case TypeList:
			if len(p.Values) == 0 {
				add(p.Line, "parameter %q: type list needs values", p.Name)
			}
			if d, ok := p.Default.([]any); ok {
				for _, v := range d {
					if !slices.Contains(p.Values, fmt.Sprint(v)) {
						add(p.Line, "parameter %q: default %q must be one of %s", p.Name, fmt.Sprint(v), strings.Join(p.Values, ", "))
					}
				}
			} else if p.Default != nil {
				add(p.Line, "parameter %q: the default of a list is a list, such as [a, b]", p.Name)
			}
		default:
			add(p.Line, "parameter %q: type must be string, int, bool, enum or list, got %q", p.Name, p.Type)
		}
		if (p.Min != nil || p.Max != nil) && p.Type != TypeInt {
			add(p.Line, "parameter %q: min and max are only allowed for type int", p.Name)
		}
		if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
			add(p.Line, "parameter %q: min is above max", p.Name)
		}
		if strings.Contains(p.Label+p.Placeholder+p.Group, "\n") {
			add(p.Line, "parameter %q: label, placeholder and group must be one line", p.Name)
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
