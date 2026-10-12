// Package docs holds the rules of a service's documentation: Markdown under
// docs/ in its repo, the order and titles of its pages, which paths may be
// read, and the checks of `forge docs lint`. forge-api and the CLI share it.
package docs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Limits, because a repo's content is not trusted.
const (
	// Dir is the folder of a repo the docs are in.
	Dir = "docs"
	// Start is the start page; a repo without it has no docs.
	Start = "index.md"
	// MaxPageBytes is the largest page that is read.
	MaxPageBytes = 1 << 20
	// MaxAssetBytes is the largest image that is read.
	MaxAssetBytes = 5 << 20
	// MaxPages is the most pages one service may have.
	MaxPages = 500
	// MaxSetBytes is the most all pages of one service may weigh together.
	MaxSetBytes = 32 << 20
)

// assetTypes are the images a page may show. SVG is left out on purpose: it
// can carry script, and the dashboard shows these images to every reader.
var assetTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// CleanPath checks a path relative to docs/ ("runbook.md", "api/errors.md")
// and returns it unchanged when it is one that may be read: no "..", no
// leading "/", nothing a path.Clean would change.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("the path is empty")
	}
	if len(p) > 1024 {
		return "", errors.New("the path is longer than 1024 characters")
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("path %q has a backslash or a NUL", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must be relative to %s/", p, Dir)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must stay inside %s/", p, Dir)
		}
	}
	if path.Clean(p) != p {
		return "", fmt.Errorf("path %q is not in its plain form (%q)", p, path.Clean(p))
	}
	return p, nil
}

// IsPage reports whether p is a Markdown page.
func IsPage(p string) bool { return strings.EqualFold(path.Ext(p), ".md") }

// AssetType returns the media type of an image a page may use.
func AssetType(p string) (string, bool) {
	t, ok := assetTypes[strings.ToLower(path.Ext(p))]
	return t, ok
}

// Entry is one page of the docs: list in forge.yaml.
type Entry struct {
	Title string
	Path  string
}

// Page is one page in the side menu.
type Page struct {
	Path  string
	Title string
}

// Order returns the pages of the side menu. With a list (forge.yaml's
// docs:), the listed pages that exist, in that order and with the titles it
// gives; without one, every page with the start page first and the others by
// path. The start page is always there. titleOf gives a page's own title.
func Order(pages []string, list []Entry, titleOf func(path string) string) []Page {
	exists := make(map[string]bool, len(pages))
	for _, p := range pages {
		exists[p] = true
	}
	var out []Page
	seen := map[string]bool{}
	add := func(p, title string) {
		if seen[p] || !exists[p] {
			return
		}
		seen[p] = true
		if title == "" {
			title = titleOf(p)
		}
		out = append(out, Page{Path: p, Title: title})
	}
	if len(list) > 0 {
		for _, e := range list {
			add(e.Path, e.Title)
		}
		if !seen[Start] && exists[Start] {
			out = append([]Page{{Path: Start, Title: titleOf(Start)}}, out...)
		}
		return out
	}
	add(Start, "")
	rest := slices.Clone(pages)
	sort.Strings(rest)
	for _, p := range rest {
		add(p, "")
	}
	return out
}

// Title is a page's first "# " heading, or its file name without ".md".
func Title(markdown []byte, p string) string {
	var fence fenceState
	sc := bufio.NewScanner(bytes.NewReader(markdown))
	sc.Buffer(make([]byte, 64*1024), MaxPageBytes)
	for sc.Scan() {
		raw := sc.Text()
		if fence.skip(raw) {
			continue
		}
		// Four spaces or a tab make a code block, not a heading.
		if strings.HasPrefix(raw, "    ") || strings.HasPrefix(raw, "\t") {
			continue
		}
		if t, ok := strings.CutPrefix(strings.TrimLeft(raw, " "), "# "); ok {
			if t = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(t), "#")); t != "" {
				return t
			}
		}
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

// fenceState follows fenced code blocks line by line.
type fenceState struct{ open string }

// skip reports whether a line is a fence or inside a fenced block. A block
// ends at a line of the same fence and nothing else: "```go" opens one but
// never closes one.
func (f *fenceState) skip(raw string) bool {
	line := strings.TrimSpace(raw)
	if f.open != "" {
		if strings.HasPrefix(line, f.open) && strings.Trim(line, f.open[:1]) == "" {
			f.open = ""
		}
		return true
	}
	for _, mark := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, mark) {
			f.open = line[:len(line)-len(strings.TrimLeft(line, mark[:1]))]
			return true
		}
	}
	return false
}

// Link is a link or image target found on a line of a page.
type Link struct {
	Line   int
	Target string
	Image  bool
}

var (
	inlineLink = regexp.MustCompile(`(!?)\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+["'(][^)]*)?\)`)
	refDef     = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	inlineCode = regexp.MustCompile("`[^`]*`")
)

// Links returns the inline links, images and link definitions of a page,
// outside code.
func Links(markdown []byte) []Link {
	var out []Link
	var fence fenceState
	sc := bufio.NewScanner(bytes.NewReader(markdown))
	sc.Buffer(make([]byte, 64*1024), MaxPageBytes)
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		if fence.skip(raw) {
			continue
		}
		if m := refDef.FindStringSubmatch(raw); m != nil {
			out = append(out, Link{Line: n, Target: m[1]})
			continue
		}
		for _, m := range inlineLink.FindAllStringSubmatch(inlineCode.ReplaceAllString(raw, ""), -1) {
			out = append(out, Link{Line: n, Target: m[2], Image: m[1] == "!"})
		}
	}
	return out
}

// Resolve turns a link on page `from` into the path under docs/ it points
// at. It answers "" without an error for a link that leaves the docs on
// purpose: another site, a mail address, an anchor on the same page.
func Resolve(from, target string) (string, error) {
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "//") {
		return "", nil
	}
	if u, err := url.Parse(target); err == nil && u.Scheme != "" {
		return "", nil
	}
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if t, err := url.PathUnescape(target); err == nil {
		target = t
	}
	if strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("%q starts at the repo's root: link relative to the page instead", target)
	}
	p := path.Join(path.Dir(from), target)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%q points outside %s/", target, Dir)
	}
	return p, nil
}

// Finding is one problem lint found, in a file of the repo.
type Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.File, f.Message)
}

// Lint checks the docs of the repo at the root of fsys, as forge-api will
// read them: the start page exists, the pages forge.yaml lists exist, pages
// and images are within the limits, and links between pages resolve.
func Lint(fsys fs.FS, list []Entry) []Finding {
	var out []Finding
	bad := func(file string, line int, format string, args ...any) {
		out = append(out, Finding{File: file, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	files := map[string]fs.FileInfo{}
	err := fs.WalkDir(fsys, Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			bad(p, 0, "is a symbolic link: GitHub serves the link's own text, not the file it points at")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, Dir+"/")] = info
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		bad(Dir, 0, "there is no %s/ folder: add %s/%s to give the service docs", Dir, Dir, Start)
		return out
	}
	if err != nil {
		bad(Dir, 0, "%v", err)
		return out
	}
	if _, ok := files[Start]; !ok {
		bad(Dir+"/"+Start, 0, "the start page is missing: a repo without it has no docs")
	}
	var pages []string
	total := int64(0)
	for p, info := range files {
		if !IsPage(p) {
			continue
		}
		pages = append(pages, p)
		if _, err := CleanPath(p); err != nil {
			bad(Dir+"/"+p, 0, "%v", err)
		}
		if info.Size() > MaxPageBytes {
			bad(Dir+"/"+p, 0, "the page is %d bytes; pages are read up to %d", info.Size(), MaxPageBytes)
		} else {
			total += info.Size()
		}
	}
	sort.Strings(pages)
	if len(pages) > MaxPages {
		bad(Dir, 0, "%d pages; at most %d are read", len(pages), MaxPages)
	}
	if total > MaxSetBytes {
		bad(Dir, 0, "the pages are %d bytes together; at most %d are read", total, MaxSetBytes)
	}
	for _, e := range list {
		if _, err := CleanPath(e.Path); err != nil {
			bad("forge.yaml", 0, "docs: %v", err)
		} else if !IsPage(e.Path) {
			bad("forge.yaml", 0, "docs: %q is not a Markdown page (.md)", e.Path)
		} else if _, ok := files[e.Path]; !ok {
			bad("forge.yaml", 0, "docs: %s/%s does not exist", Dir, e.Path)
		}
	}
	for _, p := range pages {
		data, err := fs.ReadFile(fsys, Dir+"/"+p)
		if err != nil {
			bad(Dir+"/"+p, 0, "%v", err)
			continue
		}
		for _, l := range Links(data) {
			to, err := Resolve(p, l.Target)
			if err != nil {
				bad(Dir+"/"+p, l.Line, "%v", err)
				continue
			}
			if to == "" || to == "." {
				continue
			}
			info, ok := files[to]
			switch {
			case !ok:
				bad(Dir+"/"+p, l.Line, "%q: %s/%s does not exist", l.Target, Dir, to)
			case l.Image:
				if _, ok := AssetType(to); !ok {
					bad(Dir+"/"+p, l.Line, "%q: images are png, jpeg, gif or webp", l.Target)
				} else if info.Size() > MaxAssetBytes {
					bad(Dir+"/"+p, l.Line, "%q is %d bytes; images are read up to %d", l.Target, info.Size(), MaxAssetBytes)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}
