package template

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	"forge.yaml":                      {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {% .Service.Name %}\n  team: {% .Service.Team %}\n")},
	"go.mod":                          {Data: []byte("module {% .Params.goModule %}\n")},
	"cmd/{% .Service.Name %}/main.go": {Data: []byte("package main // port {% .Params.port %}\nvar _ connect.Request[{% .Service.Name | replace \"-\" \"\" %}v1.Req]\n")},
	".github/workflows/ci.yml":        {Data: []byte("image: ghcr.io/{% .Repo.Owner %}/x:${{ github.sha }} # {{ not.ours }}\n")},
	"{% if .Params.debug %}docs{% end %}/index.md": {Data: []byte("# {% .Service.Name | upper %}\n")},
	"run.sh":   {Data: []byte("#!/bin/sh\necho {% .Service.Name %}\n"), Mode: 0o755},
	"logo.bin": {Data: []byte{0xff, 0xfe, '{', '%', ' ', 0x00}},
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
		"missing param": {fstest.MapFS{"a.txt": {Data: []byte("{% .Params.nope %}")}}, `map has no entry for key "nope"`},
		"collision":     {fstest.MapFS{"{% .Service.Name %}.txt": {Data: []byte("a")}, "billing-api.txt": {Data: []byte("b")}}, "both render to billing-api.txt"},
		"dot segment":   {fstest.MapFS{"{% \"..\" %}/x": {Data: []byte("a")}}, `renders to ".."`},
		// The key must not contain "/" itself (MapFS would split it), so the slash is character 47.
		"slash segment": {fstest.MapFS{"{% printf \"a%cb\" 47 %}": {Data: []byte("a")}}, `renders to "a/b"`},
		// Skeleton paths contain the delimiters; errors must quote them intact.
		"error in a templated path": {fstest.MapFS{"{% .Service.Name %}/a.txt": {Data: []byte("{% .Params.nope %}")}}, `{% .Service.Name %}/a.txt:1:10: <.Params.nope>: map has no entry for key "nope"`},
		"symlink":                   {fstest.MapFS{"link": {Data: []byte("x"), Mode: fs.ModeSymlink}}, "symlinks are not allowed"},
		"too large":                 {fstest.MapFS{"big": {Data: []byte(strings.Repeat("a", MaxBytes+1))}}, "larger than"},
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

// The size limit must bound memory while rendering, not only be checked after:
// a template can loop, and forge-api renders templates from a repo.
func TestRenderStopsAtTheSizeLimit(t *testing.T) {
	tp := mustParse(t, validYAML)
	v := renderValues(t, tp)
	loop := "{% range 20000 %}{% range 1000 %}xxxxxxxxxx{% end %}{% end %}" // 200 MB if unbounded
	for name, fsys := range map[string]fstest.MapFS{
		"file content": {"a.txt": {Data: []byte(loop)}},
		"path segment": {loop: {Data: []byte("x")}},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := tp.Render(fsys, v)
		runtime.ReadMemStats(&after)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("%s: got %v, want a size error", name, err)
		}
		// Bounded work costs a few times the limit (builder growth, per-iteration
		// allocations); unbounded rendering of this loop costs over 1 GiB.
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8*MaxBytes {
			t.Errorf("%s: rendering allocated %d MiB before failing; the limit is %d MiB", name, grew>>20, MaxBytes>>20)
		}
	}
}

func TestRenderRejectsOversizedSourceBeforeReading(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	_, err := tp.Render(fstest.MapFS{"big.bin": {Data: make([]byte, MaxBytes+1)}}, renderValues(t, tp))
	if err == nil || !strings.Contains(err.Error(), "big.bin: the file is larger than") {
		t.Errorf("got %v", err)
	}
}

func TestCheckSyntax(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	fsys := fstest.MapFS{
		"ok.txt":         {Data: []byte("{% .Service.Name %}")},
		"bad.txt":        {Data: []byte("{% .Service.Name ")},
		"{% if %}/x.txt": {Data: []byte("x")},
		"link":           {Data: []byte("x"), Mode: fs.ModeSymlink},
		"logo.bin":       {Data: []byte{0xff, '{', '%'}},
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
	if strings.Contains(all, "%!") {
		t.Errorf("a path with delimiters mangled an error message:\n%s", all)
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
