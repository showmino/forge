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
	if strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, &ParamError{p.Name, "value must be one line, without control characters"}
	}
	switch p.Type {
	case TypeInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, &ParamError{p.Name, fmt.Sprintf("%q is not an int", raw)}
		}
		if p.Min != nil && n < *p.Min {
			return nil, &ParamError{p.Name, fmt.Sprintf("%d must be at least %d", n, *p.Min)}
		}
		if p.Max != nil && n > *p.Max {
			return nil, &ParamError{p.Name, fmt.Sprintf("%d must be at most %d", n, *p.Max)}
		}
		return n, nil
	case TypeList:
		out := []string{}
		if strings.TrimSpace(raw) == "" {
			return out, nil
		}
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if !slices.Contains(p.Values, v) {
				return nil, &ParamError{p.Name, fmt.Sprintf("%q must be one of %s", v, strings.Join(p.Values, ", "))}
			}
			if slices.Contains(out, v) {
				return nil, &ParamError{p.Name, fmt.Sprintf("%q is given twice", v)}
			}
			out = append(out, v)
		}
		return out, nil
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
