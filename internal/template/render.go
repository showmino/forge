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

// parse parses text under an empty template name; name is only used for
// errors. text/template puts the template name into a format string for
// parse errors, which would mangle the delimiters in skeleton paths.
func (t *Template) parse(name, text string) (*gotemplate.Template, error) {
	left, right := t.Delims()
	tp, err := gotemplate.New("").Delims(left, right).Option("missingkey=error").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, templateError(name, err)
	}
	return tp, nil
}

func (t *Template) renderString(name, text string, v Values) (string, error) {
	tp, err := t.parse(name, text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tp.Execute(&b, v); err != nil {
		return "", templateError(name, err)
	}
	return b.String(), nil
}

// templateError turns `template: :1:10: executing "" at <.X>: msg` from an
// unnamed template into `name:1:10: <.X>: msg`.
func templateError(name string, err error) error {
	msg := strings.TrimPrefix(err.Error(), "template: :")
	msg = strings.Replace(msg, `executing "" at `, "", 1)
	return fmt.Errorf("%s:%s", name, msg)
}

// renderPath renders src one segment at a time. It returns "" when a segment
// renders empty, which drops the file or directory.
func (t *Template) renderPath(src string, v Values) (string, error) {
	segs := strings.Split(src, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		r, err := t.renderString(src, s, v)
		if err != nil {
			return "", err // already names src
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
