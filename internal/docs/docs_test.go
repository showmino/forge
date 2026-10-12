package docs

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCleanPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"index.md", "api/errors.md", "img/a b.png", ".well-known.md"} {
		if got, err := CleanPath(ok); err != nil || got != ok {
			t.Errorf("CleanPath(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "/index.md", "../forge.yaml", "a/../../b.md", "a/./b.md", "a//b.md", "a/", `a\b.md`, "a\x00.md", strings.Repeat("a", 1025)} {
		if _, err := CleanPath(bad); err == nil {
			t.Errorf("CleanPath(%q) accepted", bad)
		}
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()
	if !IsPage("a/B.MD") || IsPage("a.png") || IsPage("md") {
		t.Error("IsPage")
	}
	if typ, ok := AssetType("a/b.PNG"); !ok || typ != "image/png" {
		t.Errorf("AssetType(png) = %q, %v", typ, ok)
	}
	// SVG can carry script: not served.
	for _, p := range []string{"a.html", "a/b.svg"} {
		if _, ok := AssetType(p); ok {
			t.Errorf("AssetType accepted %s", p)
		}
	}
}

func TestTitle(t *testing.T) {
	t.Parallel()
	page := "Intro text\n```sh\n# not a heading\n```go\n# still code\n```\n\n    # indented code\n## Second\n  #   Payments runbook  ##\n# Later\n"
	if got := Title([]byte(page), "runbook.md"); got != "Payments runbook" {
		t.Errorf("Title = %q", got)
	}
	if got := Title([]byte("no heading\n#tag\n"), "api/error-codes.md"); got != "error-codes" {
		t.Errorf("Title without a heading = %q", got)
	}
}

func TestOrder(t *testing.T) {
	t.Parallel()
	titles := map[string]string{"index.md": "Payments", "runbook.md": "Runbook", "api/errors.md": "Errors", "adr/1.md": "ADR 1"}
	titleOf := func(p string) string { return titles[p] }
	pages := []string{"runbook.md", "api/errors.md", "index.md", "adr/1.md"}
	show := func(ps []Page) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Path+"="+p.Title)
		}
		return strings.Join(s, " ")
	}

	if got := show(Order(pages, nil, titleOf)); got != "index.md=Payments adr/1.md=ADR 1 api/errors.md=Errors runbook.md=Runbook" {
		t.Errorf("without a list: %s", got)
	}
	// The list sets the order and titles; a listed page that does not exist
	// is left out, and the start page is always first.
	list := []Entry{{Path: "runbook.md", Title: "When it breaks"}, {Path: "gone.md"}, {Path: "api/errors.md"}}
	if got := show(Order(pages, list, titleOf)); got != "index.md=Payments runbook.md=When it breaks api/errors.md=Errors" {
		t.Errorf("with a list: %s", got)
	}
	list = []Entry{{Path: "runbook.md"}, {Path: "index.md", Title: "Home"}}
	if got := show(Order(pages, list, titleOf)); got != "runbook.md=Runbook index.md=Home" {
		t.Errorf("listing the start page: %s", got)
	}
	if got := Order([]string{"a.md"}, nil, titleOf); len(got) != 1 {
		t.Errorf("without a start page: %v", got)
	}
}

func TestLinksAndResolve(t *testing.T) {
	t.Parallel()
	page := "See [the runbook](runbook.md#alerts) and ![diagram](img/flow.png \"Flow\").\n" +
		"`[not](code.md)`\n```\n[nor](fenced.md)\n```\n[ref]: <api/errors.md>\n[site](https://example.com) [mail](mailto:a@b.c) [here](#top)\n"
	got := Links([]byte(page))
	var s []string
	for _, l := range got {
		s = append(s, l.Target)
	}
	if strings.Join(s, " ") != "runbook.md#alerts img/flow.png api/errors.md https://example.com mailto:a@b.c #top" {
		t.Errorf("Links = %v", s)
	}
	if !got[1].Image || got[0].Image || got[2].Line != 6 {
		t.Errorf("Links = %+v", got)
	}

	for _, c := range []struct{ from, target, want string }{
		{"index.md", "runbook.md#alerts", "runbook.md"},
		{"api/errors.md", "../index.md", "index.md"},
		{"api/errors.md", "codes%20list.md?x=1", "api/codes list.md"},
		{"index.md", "https://example.com", ""},
		{"index.md", "#top", ""},
		{"index.md", "//cdn.example.com/a.png", ""},
	} {
		if got, err := Resolve(c.from, c.target); err != nil || got != c.want {
			t.Errorf("Resolve(%q, %q) = %q, %v", c.from, c.target, got, err)
		}
	}
	for _, target := range []string{"../forge.yaml", "/docs/index.md"} {
		if _, err := Resolve("index.md", target); err == nil {
			t.Errorf("Resolve(%q) accepted", target)
		}
	}
}

func TestLint(t *testing.T) {
	t.Parallel()
	good := fstest.MapFS{
		"docs/index.md":       {Data: []byte("# Payments\nSee [runbook](runbook.md) and ![flow](img/flow.png).\n")},
		"docs/runbook.md":     {Data: []byte("# Runbook\nBack to [the start](index.md#top). Docs at [GitHub](https://github.com).\n")},
		"docs/img/flow.png":   {Data: []byte("png")},
		"docs/notes/draft.md": {Data: []byte("draft")},
	}
	if got := Lint(good, []Entry{{Path: "runbook.md"}}); len(got) != 0 {
		t.Errorf("Lint(good) = %v", got)
	}

	bad := fstest.MapFS{
		"docs/runbook.md":  {Data: []byte("# Runbook\n\n[missing](gone.md)\n![page](notes.html)\n[out](../forge.yaml)\n")},
		"docs/notes.html":  {Data: []byte("<p>")},
		"docs/huge.md":     {Data: make([]byte, MaxPageBytes+1)},
		"docs/img/big.png": {Data: make([]byte, MaxAssetBytes+1)},
		"docs/big.md":      {Data: []byte("![big](img/big.png)")},
	}
	var got []string
	for _, f := range Lint(bad, []Entry{{Path: "api.md"}, {Path: "../x.md"}, {Path: "notes.html"}}) {
		got = append(got, f.String())
	}
	want := []string{
		`docs/big.md:1: "img/big.png" is 5242881 bytes; images are read up to 5242880`,
		"docs/huge.md: the page is 1048577 bytes; pages are read up to 1048576",
		"docs/index.md: the start page is missing: a repo without it has no docs",
		`docs/runbook.md:3: "gone.md": docs/gone.md does not exist`,
		`docs/runbook.md:4: "notes.html": images are png, jpeg, gif or webp`,
		`docs/runbook.md:5: "../forge.yaml" points outside docs/`,
		"forge.yaml: docs: docs/api.md does not exist",
		`forge.yaml: docs: path "../x.md" must stay inside docs/`,
		`forge.yaml: docs: "notes.html" is not a Markdown page (.md)`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Lint(bad) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if got := Lint(fstest.MapFS{"README.md": {}}, nil); len(got) != 1 || !strings.Contains(got[0].Message, "no docs/ folder") {
		t.Errorf("Lint(no docs) = %v", got)
	}
}
