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
	// has reports whether a list parameter holds v: {% has .Params.envs "prod" %}.
	"has": slices.Contains[[]string],
	// title upper-cases the first letter: "corp" gives "Corp".
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	// bicepString escapes s for a single-quoted Bicep string, interpolation included.
	"bicepString": strings.NewReplacer(`\`, `\\`, `'`, `\'`, "${", `\${`).Replace,
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

// errTooLarge is returned as soon as rendering would exceed its byte budget.
var errTooLarge = fmt.Errorf("rendered output is larger than %d bytes", MaxBytes)

// limitWriter fails once more than left bytes are written, so a looping
// template is stopped while it renders instead of after.
type limitWriter struct {
	b    strings.Builder
	left int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errTooLarge
	}
	w.left -= len(p)
	return w.b.Write(p)
}

func (t *Template) renderString(name, text string, v Values) (string, error) {
	out, _, err := t.renderLimited(name, text, v, MaxBytes)
	return out, err
}

// renderLimited renders text with at most left bytes of output and returns
// the bytes left over.
func (t *Template) renderLimited(name, text string, v Values, left int) (string, int, error) {
	tp, err := t.parse(name, text)
	if err != nil {
		return "", left, err
	}
	w := &limitWriter{left: left}
	if err := tp.Execute(w, v); err != nil {
		if errors.Is(err, errTooLarge) {
			return "", left, errTooLarge
		}
		return "", left, templateError(name, err)
	}
	return w.b.String(), w.left, nil
}

// templateError turns `template: :1:10: executing "" at <.X>: msg` from an
// unnamed template into `name:1:10: <.X>: msg`.
func templateError(name string, err error) error {
	msg := strings.TrimPrefix(err.Error(), "template: :")
	msg = strings.Replace(msg, `executing "" at `, "", 1)
	return fmt.Errorf("%s:%s", name, msg)
}

// notOnWindows are the characters NTFS rejects in file names. Skeleton paths
// and rendered paths avoid them so every repo checks out on Windows.
const notOnWindows = `<>:"|?*\`

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
		if r == "." || r == ".." || strings.ContainsAny(r, "/"+notOnWindows) {
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
	left := MaxBytes            // output budget shared by every file
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > int64(MaxBytes) {
			return fmt.Errorf("%s: the file is larger than %d bytes", src, MaxBytes)
		}
		data, err := fs.ReadFile(skeleton, src)
		if err != nil {
			return err
		}
		if utf8.Valid(data) {
			out, rest, err := t.renderLimited(src, string(data), v, left)
			if err != nil {
				return err
			}
			data, left = []byte(out), rest
		} else {
			if len(data) > left {
				return errTooLarge
			}
			left -= len(data)
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
		name := path.Base(src) // each entry checks only its own name
		if strings.ContainsAny(name, notOnWindows) {
			add(src, fmt.Errorf("the name has a character Windows does not allow (%s); in template actions use `raw strings` and function calls instead of \" and |", notOnWindows))
		}
		if _, err := t.parse(src, name); err != nil {
			add(src, err)
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
