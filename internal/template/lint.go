package template

import (
	"fmt"
	"io/fs"
	"maps"
	"path"
	"strconv"
)

// SampleValues are the built-in values lint renders with. The service name
// contains a dash on purpose: templates must cope with one.
func SampleValues(t *Template) Values {
	return Values{
		Service:  ServiceValues{Name: "sample-service", Team: "sample-team", Owners: []string{"owner@example.com"}, Staging: true, Production: true},
		Repo:     RepoValues{Owner: "sample-org", Name: "sample-service", URL: "https://github.com/sample-org/sample-service"},
		Template: TemplateValues{Name: t.Name, Version: "v0.0.0"},
	}
}

// sample is the value lint uses for a required parameter.
func sample(p Parameter) string {
	switch p.Type {
	case TypeInt:
		switch {
		case p.Min != nil:
			return strconv.Itoa(*p.Min)
		case p.Max != nil && *p.Max < 1:
			return strconv.Itoa(*p.Max)
		}
		return "1"
	case TypeBool:
		return "false"
	case TypeEnum, TypeList:
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
	files, found := t.lintRender(skel, v)
	add(found)
	if len(found) > 0 {
		return nil, out
	}
	// Every other value of an enum too: a template may pick its files by one.
	for _, p := range t.Parameters {
		if p.Type != TypeEnum {
			continue
		}
		for _, value := range p.Values {
			if value == params[p.Name] {
				continue
			}
			v.Params = maps.Clone(params)
			v.Params[p.Name] = value
			_, found := t.lintRender(skel, v)
			for i := range found {
				found[i].Message = fmt.Sprintf("with %s=%s: %s", p.Name, value, found[i].Message)
			}
			add(found)
		}
	}
	if len(out) > 0 {
		return nil, out
	}
	return files, nil
}

// lintRender renders skel with v and checks what came out: a file
// template's paths, or a repo's forge.yaml.
func (t *Template) lintRender(skel fs.FS, v Values) ([]File, []Finding) {
	files, err := t.Render(skel, v)
	if err != nil {
		return nil, []Finding{{File: SkeletonDir, Message: err.Error()}}
	}
	if t.IsFile() {
		if len(files) == 0 {
			return nil, []Finding{{File: SkeletonDir, Message: "renders no files"}}
		}
		if err := CheckFilePaths(files); err != nil {
			return nil, []Finding{{File: SkeletonDir, Message: err.Error()}}
		}
		return files, nil
	}
	if err := t.CheckManifest(files, v.Service.Name, v.Service.Team); err != nil {
		return nil, []Finding{{File: path.Join(SkeletonDir, "forge.yaml"), Message: err.Error()}}
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
