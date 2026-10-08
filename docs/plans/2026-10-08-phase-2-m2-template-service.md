# Phase 2, Milestone 2: Template Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve templates from the API (`TemplateService.ListTemplates` / `GetTemplate`), read from a local directory or from release tags of `showmino/forge-templates` on GitHub, and add `forge template list` and `forge template get`.

**Architecture:** A `TemplateSource` interface hands the API a repo tree (`fs.FS`) per version. `DirSource` serves a local checkout as version `dev`; `GitHubSource` lists semver tags and downloads, extracts (in memory, safely) and caches each tag's tarball through a small `internal/github` client. The `TemplateService` handlers parse templates with milestone 1's `internal/template` package and return them as protos; the CLI renders them like `service list/get`.

**Tech Stack:** Go 1.24 (`archive/tar`, `compress/gzip`, `io/fs`, `testing/fstest.MapFS` as the in-memory tree), ConnectRPC, buf v1.47.2, GitHub REST API (`/repos/{o}/{r}/tags`, `/repos/{o}/{r}/tarball/{ref}`), Cobra.

**Spec:** `docs/specs/2026-10-08-phase-2-templates-design.md` (section 2 "Protos" for `TemplateService`, "Components" rows for `internal/server/templates.go` and `internal/github`, "Configuration"; section 3 `template list` / `template get`; section 4 errors and security; section 6 milestone 2). Design doc: `docs/index.md`.

## Global Constraints

- Go is not on the system PATH: run commands with `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`. Full gate: `GOTOOLCHAIN=go1.24.13 make test lint` (CI parity; golangci-lint v2.5.0). After editing `proto/`, run `make generate` and commit `gen/`.
- Work on a new branch `phase-2-m2` cut from up-to-date `main`; run `git branch --show-current` before every commit. The owner pushes and merges.
- Proto changes are additive only (`buf breaking` against `main` must pass). Resource names: `templates/{template}`.
- No package-level mutable globals in `internal/cli`; commands use `*Factory`. Only `main` picks exit codes. Every command supports `-o table|json|yaml|name`; JSON is the API resource itself (protojson); an empty list still prints `{"templates": []}`.
- Errors (spec section 4): template or version not found → `NOT_FOUND` (exit 3); templates not configured, or no release tags → `FAILED_PRECONDITION` (exit 6); GitHub unreachable or rate-limited → `UNAVAILABLE` (exit 1); bad name → `INVALID_ARGUMENT` (exit 2).
- Tarball extraction rejects symlinks, hard links, other special files, path traversal and entries over `template.MaxBytes` (10 MiB); the whole extracted tree is capped at 100 MiB and 20,000 files.
- "Newest" means the highest semver tag `vMAJOR.MINOR.PATCH`; other tags (pre-releases, `latest`) are ignored.
- The GitHub token is never logged or returned in errors.
- Commit after every task with the trailer `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

**Refinements of the spec made while planning** (Task 0 records them in the spec):

- `TemplateParameter.default` is named `default_value` in the proto, so it cannot collide with the `default` keyword of proto options and generated code reads clearly (`GetDefaultValue`).
- Template RPCs require an authenticated caller but no permission: templates are not sensitive, and a new permission would change every caller's `auth status`.
- Until the GitHub App exists (milestone 3), forge-api reads the template repo with a read-only token: new flag `--github-token-file` (`FORGE_GITHUB_TOKEN_FILE`). Milestone 3's App settings will take precedence.
- Fetched tags are kept in a `testing/fstest.MapFS`: the standard library's in-memory `fs.FS`, which needs no writable disk (the API's root filesystem is read-only on k3s).
- `cli-smoke.sh` gets template cases in milestone 4, when the k3s API is configured with a template source; until then the k3s API answers `FAILED_PRECONDITION`.

## Review Focus

1. **Real GitHub tarballs** start with a `pax_global_header` entry and wrap every file in an `owner-repo-sha/` directory: both must be handled, or no real release ever loads. Pinned in Task 2's tests.
2. **Hostile or odd tarball entries** (symlinks, hard links, `../` names, a 50 MiB file): rejected with an error naming the entry, never written anywhere or silently skipped. Pinned in Task 2.
3. **Tag ordering and filtering:** `v1.10.0` is newer than `v1.9.0`; `latest`, `v2.0.0-rc.1` and `release-2` are not versions. Pinned in Task 3.
4. **One broken template in a release** (bad `template.yaml`): `ListTemplates` still lists the others and logs a warning; `GetTemplate` of the broken one fails clearly. Pinned in Task 4.
5. **Repeated requests:** a tag's tarball is downloaded once; the tag list refreshes after a minute so a new release shows up without a restart. Pinned in Task 3.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `docs/specs/2026-10-08-phase-2-templates-design.md` (modify) | Record the refinements above |
| `internal/github/github.go` (create) | Minimal GitHub REST client: auth header, `ListTags`, `Tarball`, `ErrNotFound` |
| `internal/server/untar.go` (create) | Safe in-memory extraction of a GitHub tarball into an `fs.FS` |
| `internal/server/template_source.go` (create) | `TemplateSource`, `ErrVersionNotFound`, `DirSource`, `GitHubSource` |
| `proto/forge/v1/template.proto` (create), `gen/forge/v1/...` (generated) | `TemplateService` contract |
| `internal/server/templates.go` (create) | `TemplateService` handlers and proto mapping |
| `internal/server/server.go` (modify) | `Options.Templates`; register the handler |
| `pkg/client/client.go` (modify) | `Client.Templates` |
| `cmd/forge-api/templates.go` (create), `cmd/forge-api/main.go` (modify) | Choosing a template source from flags |
| `internal/cli/template.go` (modify) | `template list`, `template get` |
| `internal/cli/cli_test.go` (modify) | Harness can serve templates |
| `internal/cli/template_test.go`, `internal/cli/testdata/template_*.golden` | CLI tests |
| `README.md`, `.claude/skills/forge/SKILL.md` (modify) | Docs |

---

### Task 0: Branch and spec refinements

**Files:**
- Modify: `docs/specs/2026-10-08-phase-2-templates-design.md`

**Interfaces:** none.

- [ ] **Step 1: Branch from up-to-date main**

```bash
git switch main && git pull --ff-only && git switch -c phase-2-m2 && git branch --show-current
```

Expected: `phase-2-m2`.

- [ ] **Step 2: Edit the spec**

In section 2's `TemplateParameter` proto block change `string default = 4;           // as written in template.yaml` to `string default_value = 4;     // as written in template.yaml`.

In "Configuration", add a row after `--github-app-key-file`:

```markdown
| `--github-token-file` | Path to a read-only GitHub token for the template repo (milestone 2, before the App exists; App settings take precedence) |
```

After the Configuration table's paragraph, add:

```markdown
`TemplateService` requires an authenticated caller but no permission: templates are not sensitive.
```

- [ ] **Step 3: Commit**

```bash
git add docs/specs && git commit -m "spec: default_value, template auth and --github-token-file for milestone 2

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 1: GitHub client

**Files:**
- Create: `internal/github/github.go`
- Test: `internal/github/github_test.go`

**Interfaces:**
- Produces:
  - `const DefaultBaseURL = "https://api.github.com"`
  - `var ErrNotFound error` (wrapped for HTTP 404)
  - `type Client struct { BaseURL string; HTTP *http.Client; Token func(ctx context.Context) (string, error) }`
  - `func StaticToken(token string) func(context.Context) (string, error)`
  - `func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]string, error)` — every tag name, following pages of 100
  - `func (c *Client) Tarball(ctx context.Context, owner, repo, ref string) (io.ReadCloser, error)` — the gzipped tarball body; caller closes

- [ ] **Step 1: Write the failing tests** — `internal/github/github_test.go`:

```go
package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-GitHub-Api-Version") == "" {
			http.Error(w, "bad headers", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			http.Error(w, "want per_page=100", http.StatusBadRequest)
			return
		}
		first, n := 0, 100 // 150 tags over two pages
		if r.URL.Query().Get("page") == "2" {
			first, n = 100, 50
		}
		var items []string
		for i := first; i < first+n; i++ {
			items = append(items, fmt.Sprintf(`{"name":"t%d","commit":{"sha":"x"}}`, i))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/o/r/tarball/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/codeload/o/r/v1.0.0", http.StatusFound) // GitHub redirects to codeload
	})
	mux.HandleFunc("GET /codeload/o/r/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("TARBALL"))
	})
	mux.HandleFunc("GET /repos/o/broken/tags", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestListTags(t *testing.T) {
	t.Parallel()
	c := &Client{BaseURL: fakeGitHub(t).URL, Token: StaticToken("tok")}
	tags, err := c.ListTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 150 || tags[0] != "t0" || tags[149] != "t149" {
		t.Errorf("got %d tags: first %q last %q", len(tags), tags[0], tags[len(tags)-1])
	}
}

func TestTarballFollowsRedirect(t *testing.T) {
	t.Parallel()
	c := &Client{BaseURL: fakeGitHub(t).URL, Token: StaticToken("tok")}
	body, err := c.Tarball(context.Background(), "o", "r", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "TARBALL" {
		t.Errorf("body %q", data)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv := fakeGitHub(t)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}
	if _, err := c.Tarball(context.Background(), "o", "r", "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v", err)
	}
	_, err := c.ListTags(context.Background(), "o", "broken")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("server error: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "tok") {
		t.Errorf("error leaks the token: %v", err)
	}
	failing := &Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", errors.New("no key") }}
	if _, err := failing.ListTags(context.Background(), "o", "r"); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("token error: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/github`
Expected: FAIL to compile (`undefined: Client`).

- [ ] **Step 3: Implement** — `internal/github/github.go`:

```go
// Package github is a small client for the GitHub REST API: what Forge needs
// and nothing more. Milestone 3 adds GitHub App authentication and repo creation.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

const (
	pageSize = 100
	maxPages = 50
)

// ErrNotFound is wrapped by errors for HTTP 404 responses.
var ErrNotFound = errors.New("not found on GitHub")

// Client calls the GitHub REST API.
type Client struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// HTTP defaults to a client with a 60s timeout.
	HTTP *http.Client
	// Token returns the bearer token for each request; nil sends none.
	Token func(ctx context.Context) (string, error)
}

// StaticToken returns a Token func for a fixed token.
func StaticToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token, nil }
}

// get returns the response of a GET that answered 200; the caller closes its body.
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("github token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", path, ErrNotFound)
	}
	return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
}

// ListTags returns every tag name of owner/repo.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]string, error) {
	var names []string
	for page := 1; page <= maxPages; page++ {
		resp, err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/tags?per_page=%d&page=%d", url.PathEscape(owner), url.PathEscape(repo), pageSize, page))
		if err != nil {
			return nil, err
		}
		var tags []struct {
			Name string `json:"name"`
		}
		err = json.NewDecoder(resp.Body).Decode(&tags)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags of %s/%s: %w", owner, repo, err)
		}
		for _, t := range tags {
			names = append(names, t.Name)
		}
		if len(tags) < pageSize {
			break
		}
	}
	return names, nil
}

// Tarball returns the gzipped tarball of owner/repo at ref. GitHub answers
// with a redirect to a signed download URL, which the HTTP client follows
// without forwarding the Authorization header to the other host.
func (c *Client) Tarball(ctx context.Context, owner, repo, ref string) (io.ReadCloser, error) {
	resp, err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/tarball/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref)))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./internal/github -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github && git commit -m "github: minimal REST client for tags and tarballs

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Safe tarball extraction

**Files:**
- Create: `internal/server/untar.go`
- Test: `internal/server/untar_test.go`

**Interfaces:**
- Consumes: `template.MaxBytes`.
- Produces: `func untar(r io.Reader) (fs.FS, error)` (package-private) — the repo tree without GitHub's top-level directory; regular files only, modes `0o644` or `0o755`.

- [ ] **Step 1: Write the failing tests** — `internal/server/untar_test.go`:

```go
package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/template"
)

type entry struct {
	name     string
	typ      byte
	mode     int64
	data     string
	size     int64 // when set, overrides len(data) in the header
	linkname string
}

// tarball builds a gzipped tarball shaped like GitHub's: a pax global header
// first, then everything under one "owner-repo-sha/" directory.
func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	all := append([]entry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		{name: "showmino-forge-templates-abc1234/", typ: tar.TypeDir, mode: 0o755},
	}, entries...)
	for _, e := range all {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.linkname}
		if e.typ == tar.TypeXGlobalHeader {
			h.PAXRecords = map[string]string{"comment": "abc1234"}
		}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.data))
			if e.size > 0 {
				h.Size = e.size
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg && e.size == 0 {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A declared size larger than the data makes Close fail; such tests only
	// need the header to be read, so ignore it.
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

const top = "showmino-forge-templates-abc1234/"

func TestUntar(t *testing.T) {
	t.Parallel()
	fsys, err := untar(bytes.NewReader(tarball(t,
		entry{name: top + "go-grpc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: top + "go-grpc/template.yaml", typ: tar.TypeReg, mode: 0o644, data: "name: go-grpc\n"},
		entry{name: top + "go-grpc/skeleton/run.sh", typ: tar.TypeReg, mode: 0o755, data: "#!/bin/sh\n"},
	)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(fsys, "go-grpc/template.yaml")
	if err != nil || string(data) != "name: go-grpc\n" {
		t.Errorf("template.yaml: %q %v", data, err)
	}
	info, err := fs.Stat(fsys, "go-grpc/skeleton/run.sh")
	if err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("run.sh must stay executable: %v %v", info, err)
	}
	if _, err := fs.Stat(fsys, "pax_global_header"); err == nil {
		t.Error("the pax global header must not become a file")
	}
}

func TestUntarRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		e    entry
		want string
	}{
		"symlink":       {entry{name: top + "link", typ: tar.TypeSymlink, linkname: "/etc/passwd"}, "links and special files"},
		"hard link":     {entry{name: top + "hard", typ: tar.TypeLink, linkname: top + "a"}, "links and special files"},
		"traversal":     {entry{name: top + "../escape", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"absolute":      {entry{name: "/etc/x", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"no top dir":    {entry{name: "loose.txt", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"too large":     {entry{name: top + "big", typ: tar.TypeReg, size: template.MaxBytes + 1}, "larger than"},
		"fifo":          {entry{name: top + "pipe", typ: tar.TypeFifo}, "links and special files"},
	}
	for name, tt := range tests {
		_, err := untar(bytes.NewReader(tarball(t, tt.e)))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", name, err, tt.want)
		}
	}
	if _, err := untar(strings.NewReader("not gzip")); err == nil {
		t.Error("garbage accepted")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server -run Untar`
Expected: FAIL to compile (`undefined: untar`).

- [ ] **Step 3: Implement** — `internal/server/untar.go`:

```go
package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"

	"github.com/showmino/forge/internal/template"
)

// Limits for an extracted template repo.
const (
	maxTreeBytes = 100 << 20
	maxTreeFiles = 20000
)

// untar reads a gzipped tarball as GitHub serves it (an optional pax global
// header, then everything under one "owner-repo-sha/" directory) into an
// in-memory tree. Links, special files, names that leave the repo directory
// and oversized content are errors, never skipped.
//
// fstest.MapFS is the standard library's in-memory fs.FS; the API's root
// filesystem is read-only, so the tree cannot go to disk.
func untar(r io.Reader) (fs.FS, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("read tarball: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := fstest.MapFS{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("tarball entry %s: links and special files are not allowed", h.Name)
		}
		name, ok := inRepo(h.Name)
		if !ok {
			return nil, fmt.Errorf("tarball entry %q is outside the repo directory", h.Name)
		}
		if h.Size > template.MaxBytes {
			return nil, fmt.Errorf("tarball entry %s is larger than %d bytes", name, template.MaxBytes)
		}
		total += int(h.Size)
		if total > maxTreeBytes || len(out) == maxTreeFiles {
			return nil, fmt.Errorf("the template repo is larger than %d bytes or %d files", maxTreeBytes, maxTreeFiles)
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("read tarball entry %s: %w", name, err)
		}
		mode := fs.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		out[name] = &fstest.MapFile{Data: data, Mode: mode}
	}
}

// inRepo drops the top-level directory GitHub puts around every entry. It
// rejects absolute names, entries outside a directory, and anything that is
// not a clean relative path afterwards.
func inRepo(name string) (string, bool) {
	if path.IsAbs(name) {
		return "", false
	}
	topDir, rest, ok := strings.Cut(name, "/")
	if !ok || topDir == "." || topDir == ".." || rest == "" {
		return "", false
	}
	if !fs.ValidPath(rest) {
		return "", false
	}
	return rest, true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./internal/server -run Untar -v`
Expected: PASS. If the "too large" case fails because the tar writer refuses the header, check the message: the test only needs `untar` to see the declared size; adjust the helper, not `untar`.

- [ ] **Step 5: Commit**

```bash
git add internal/server/untar.go internal/server/untar_test.go
git commit -m "server: extract GitHub tarballs in memory, rejecting links and traversal

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Template sources

**Files:**
- Create: `internal/server/template_source.go`
- Test: `internal/server/template_source_test.go`

**Interfaces:**
- Consumes: `github.Client` (Task 1), `untar` (Task 2), `version.Compare` (`internal/version`).
- Produces:
  - `type TemplateSource interface { Versions(ctx context.Context) ([]string, error); Fetch(ctx context.Context, version string) (fs.FS, error) }` — `Versions` newest first; `Fetch` wraps `ErrVersionNotFound` for an unknown version
  - `var ErrVersionNotFound error`
  - `type DirSource struct{ Dir string }` — versions `["dev"]`
  - `type GitHubSource struct{ ...; Now func() time.Time }` and `func NewGitHubSource(c *github.Client, repo string) (*GitHubSource, error)` — `repo` is `owner/name`

- [ ] **Step 1: Write the failing tests** — `internal/server/template_source_test.go`:

```go
package server

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showmino/forge/internal/github"
)

func TestDirSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := DirSource{Dir: dir}
	vs, err := s.Versions(context.Background())
	if err != nil || len(vs) != 1 || vs[0] != "dev" {
		t.Errorf("versions %v %v", vs, err)
	}
	fsys, err := s.Fetch(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(fsys, "README.md"); err != nil || string(data) != "hi" {
		t.Errorf("README.md %q %v", data, err)
	}
	if _, err := s.Fetch(context.Background(), "v1.0.0"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("other version: %v", err)
	}
}

type fakeRepo struct {
	srv      *httptest.Server
	tagCalls atomic.Int32
	tarCalls atomic.Int32
	tags     atomic.Pointer[[]string] // replaced by tests while the server runs
}

func newFakeRepo(t *testing.T, tags ...string) *fakeRepo {
	t.Helper()
	f := &fakeRepo{}
	f.tags.Store(&tags)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/forge-templates/tags", func(w http.ResponseWriter, _ *http.Request) {
		f.tagCalls.Add(1)
		var items []string
		for _, tag := range *f.tags.Load() {
			items = append(items, fmt.Sprintf(`{"name":%q}`, tag))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/showmino/forge-templates/tarball/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.tarCalls.Add(1)
		_, _ = w.Write(tarball(t, entry{name: top + "VERSION", typ: tar.TypeReg, mode: 0o644, data: r.PathValue("ref")}))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRepo) source(t *testing.T) *GitHubSource {
	t.Helper()
	s, err := NewGitHubSource(&github.Client{BaseURL: f.srv.URL}, "showmino/forge-templates")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGitHubSourceVersions(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.9.0", "latest", "v1.10.0", "v2.0.0-rc.1", "release-2", "v0.1.0")
	vs, err := f.source(t).Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(vs, ",") != "v1.10.0,v1.9.0,v0.1.0" {
		t.Errorf("versions %v: want semver tags only, newest first", vs)
	}
}

func TestGitHubSourceCaches(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.0.0")
	s := f.source(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		fsys, err := s.Fetch(ctx, "v1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := fs.ReadFile(fsys, "VERSION"); string(data) != "v1.0.0" {
			t.Errorf("VERSION = %q", data)
		}
	}
	if n := f.tarCalls.Load(); n != 1 {
		t.Errorf("tarball downloaded %d times, want once", n)
	}
	if n := f.tagCalls.Load(); n != 1 {
		t.Errorf("tags listed %d times within a minute, want once", n)
	}

	f.tags.Store(&[]string{"v1.0.0", "v1.1.0"}) // a new release appears
	now = now.Add(2 * time.Minute)
	vs, err := s.Versions(ctx)
	if err != nil || vs[0] != "v1.1.0" {
		t.Errorf("after a minute the new tag must show: %v %v", vs, err)
	}
	if _, err := s.Fetch(ctx, "v9.9.9"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("unknown version: %v", err)
	}
}

func TestNewGitHubSourceRepoName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "forge-templates", "a/b/c", "/x", "x/"} {
		if _, err := NewGitHubSource(&github.Client{}, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server -run 'DirSource|GitHubSource'`
Expected: FAIL to compile (`undefined: DirSource`).

- [ ] **Step 3: Implement** — `internal/server/template_source.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/version"
)

// TemplateSource hands out the template repo's tree by version.
type TemplateSource interface {
	// Versions returns the available versions, newest first.
	Versions(ctx context.Context) ([]string, error)
	// Fetch returns the repo tree at version, or an error wrapping
	// ErrVersionNotFound when there is no such version.
	Fetch(ctx context.Context, version string) (fs.FS, error)
}

// ErrVersionNotFound means a template version does not exist.
var ErrVersionNotFound = errors.New("template version not found")

// DirSource serves a local checkout of the template repo as the single
// version "dev", for development.
type DirSource struct{ Dir string }

// Versions implements TemplateSource.
func (DirSource) Versions(context.Context) ([]string, error) { return []string{"dev"}, nil }

// Fetch implements TemplateSource.
func (s DirSource) Fetch(_ context.Context, v string) (fs.FS, error) {
	if v != "dev" {
		return nil, fmt.Errorf("%w: %s (a template directory only serves \"dev\")", ErrVersionNotFound, v)
	}
	return os.DirFS(s.Dir), nil
}

// tagTTL is how long the tag list is reused, so a new release shows up
// within a minute without a request to GitHub on every call.
const tagTTL = time.Minute

var semverTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// GitHubSource serves the semver tags of a GitHub repo. Each tag's tree is
// downloaded once and kept in memory: tags do not change.
type GitHubSource struct {
	client      *github.Client
	owner, repo string
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time

	mu     sync.Mutex
	tags   []string
	tagsAt time.Time
	trees  map[string]fs.FS
}

// NewGitHubSource returns a source for repo, "owner/name".
func NewGitHubSource(c *github.Client, repo string) (*GitHubSource, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("template repo %q must be owner/name", repo)
	}
	return &GitHubSource{client: c, owner: owner, repo: name, Now: time.Now, trees: map[string]fs.FS{}}, nil
}

// Versions implements TemplateSource.
func (s *GitHubSource) Versions(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, err := s.versionsLocked(ctx)
	return slices.Clone(vs), err
}

func (s *GitHubSource) versionsLocked(ctx context.Context) ([]string, error) {
	if s.tags != nil && s.Now().Sub(s.tagsAt) < tagTTL {
		return s.tags, nil
	}
	all, err := s.client.ListTags(ctx, s.owner, s.repo)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s/%s: %w", s.owner, s.repo, err)
	}
	tags := []string{}
	for _, t := range all {
		if semverTag.MatchString(t) {
			tags = append(tags, t)
		}
	}
	slices.SortFunc(tags, func(a, b string) int { return version.Compare(b, a) })
	s.tags, s.tagsAt = tags, s.Now()
	return tags, nil
}

// Fetch implements TemplateSource.
func (s *GitHubSource) Fetch(ctx context.Context, v string) (fs.FS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tree, ok := s.trees[v]; ok {
		return tree, nil
	}
	tags, err := s.versionsLocked(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(tags, v) {
		return nil, fmt.Errorf("%w: %s", ErrVersionNotFound, v)
	}
	body, err := s.client.Tarball(ctx, s.owner, s.repo, v)
	if err != nil {
		return nil, fmt.Errorf("download %s/%s@%s: %w", s.owner, s.repo, v, err)
	}
	defer body.Close()
	tree, err := untar(body)
	if err != nil {
		return nil, fmt.Errorf("%s/%s@%s: %w", s.owner, s.repo, v, err)
	}
	s.trees[v] = tree
	return tree, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -race ./internal/server -run 'DirSource|GitHubSource' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/template_source.go internal/server/template_source_test.go
git commit -m "server: template sources from a directory or GitHub release tags

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `TemplateService`

**Files:**
- Create: `proto/forge/v1/template.proto`; generated `gen/forge/v1/template.pb.go`, `gen/forge/v1/forgev1connect/template.connect.go`
- Create: `internal/server/templates.go`
- Modify: `internal/server/server.go` (`Options`, `New`)
- Modify: `pkg/client/client.go` (`Client`, `New`)
- Test: `internal/server/templates_test.go`

**Interfaces:**
- Consumes: `TemplateSource`, `ErrVersionNotFound` (Task 3); `template.Parse`, `Template.Validate`, `template.Discover`, `template.FileName` (milestone 1); `manifest.ValidID`.
- Produces:
  - proto `forge.v1.TemplateService` with `ListTemplates`, `GetTemplate`; messages `Template`, `TemplateParameter` (nested enum `Type`), request/response messages as below
  - `server.Options.Templates TemplateSource` (nil = not configured)
  - `client.Client.Templates forgev1connect.TemplateServiceClient`

- [ ] **Step 1: Write the contract** — `proto/forge/v1/template.proto`:

```proto
syntax = "proto3";

package forge.v1;

// TemplateService lists the service templates of a release of the template
// repo. Resource names follow AIP style: "templates/{template}".
service TemplateService {
  // ListTemplates returns the templates of one release, the newest by default.
  rpc ListTemplates(ListTemplatesRequest) returns (ListTemplatesResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // GetTemplate returns one template of one release, the newest by default.
  rpc GetTemplate(GetTemplateRequest) returns (GetTemplateResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

// Template is a service template at one version.
message Template {
  // "templates/{template}".
  string name = 1;
  // Release tag, for example "v1.4.0"; "dev" for a local template directory.
  string version = 2;
  string title = 3;
  string description = 4;
  repeated string tags = 5;
  // Team that owns the template.
  string owner = 6;
  repeated TemplateParameter parameters = 7;
}

// TemplateParameter is one typed input of a template.
message TemplateParameter {
  // Type is the parameter's type.
  enum Type {
    TYPE_UNSPECIFIED = 0;
    TYPE_STRING = 1;
    TYPE_INT = 2;
    TYPE_BOOL = 3;
    TYPE_ENUM = 4;
  }
  string name = 1;
  Type type = 2;
  // True when the parameter has no default.
  bool required = 3;
  // The default as written in template.yaml; it may use built-in values.
  string default_value = 4;
  // RE2 pattern a string value must match.
  string pattern = 5;
  // Allowed values of an enum.
  repeated string values = 6;
  string description = 7;
}

message ListTemplatesRequest {
  // Release tag; empty means the newest.
  string version = 1;
  // Only templates with this tag.
  string tag = 2;
}

message ListTemplatesResponse {
  repeated Template templates = 1;
}

message GetTemplateRequest {
  // "templates/{template}".
  string name = 1;
  // Release tag; empty means the newest.
  string version = 2;
}

message GetTemplateResponse {
  Template template = 1;
}
```

Run: `make generate && buf lint && git status --short gen/`
Expected: `buf lint` silent; new files `gen/forge/v1/template.pb.go` and `gen/forge/v1/forgev1connect/template.connect.go`.

- [ ] **Step 2: Write the failing tests** — `internal/server/templates_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// staticSource serves fixed trees, or fails the way a source can.
type staticSource struct {
	versions []string
	trees    map[string]fs.FS
	err      error
}

func (s staticSource) Versions(context.Context) ([]string, error) { return s.versions, s.err }

func (s staticSource) Fetch(_ context.Context, v string) (fs.FS, error) {
	if s.err != nil {
		return nil, s.err
	}
	if t, ok := s.trees[v]; ok {
		return t, nil
	}
	return nil, ErrVersionNotFound
}

func templateYAML(name, tags string, params string) string {
	return "apiVersion: forge.dev/v1\nkind: Template\nname: " + name + "\ntitle: " + name + " title\ndescription: " + name + " description\ntags: [" + tags + "]\nowner: platform\n" + params
}

func release(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

func templateService(src TemplateSource) (*templateService, *bytes.Buffer) {
	var logs bytes.Buffer
	return &templateService{src: src, log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

var twoReleases = staticSource{
	versions: []string{"v1.1.0", "v1.0.0"},
	trees: map[string]fs.FS{
		"v1.1.0": release(map[string]string{
			"go-grpc/template.yaml": templateYAML("go-grpc", "go, grpc", "parameters:\n  - name: port\n    type: int\n    default: 8080\n    description: Port\n  - name: tier\n    type: enum\n    values: [gold, silver]\n  - name: module\n    type: string\n    pattern: '^[a-z]+$'\n"),
			"node-api/template.yaml": templateYAML("node-api", "node", ""),
			"broken/template.yaml":   "apiVersion: forge.dev/v1\nkind: Template\nname: broken\nnope: 1\n",
			"README.md":              "not a template",
		}),
		"v1.0.0": release(map[string]string{"go-grpc/template.yaml": templateYAML("go-grpc", "go", "")}),
	},
}

func TestListTemplates(t *testing.T) {
	t.Parallel()
	s, logs := templateService(twoReleases)
	resp, err := s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tp := range resp.Msg.GetTemplates() {
		names = append(names, tp.GetName()+"@"+tp.GetVersion())
	}
	if strings.Join(names, ",") != "templates/go-grpc@v1.1.0,templates/node-api@v1.1.0" {
		t.Errorf("newest release, broken one skipped: %v", names)
	}
	if !strings.Contains(logs.String(), "broken") {
		t.Errorf("a broken template must be logged: %q", logs.String())
	}

	resp, err = s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{Tag: "node"}))
	if err != nil || len(resp.Msg.GetTemplates()) != 1 || resp.Msg.GetTemplates()[0].GetName() != "templates/node-api" {
		t.Errorf("tag filter: %v %v", resp, err)
	}
	resp, err = s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{Version: "v1.0.0"}))
	if err != nil || len(resp.Msg.GetTemplates()) != 1 || resp.Msg.GetTemplates()[0].GetVersion() != "v1.0.0" {
		t.Errorf("pinned version: %v %v", resp, err)
	}
}

func TestGetTemplate(t *testing.T) {
	t.Parallel()
	s, _ := templateService(twoReleases)
	resp, err := s.GetTemplate(writerCtx(), connect.NewRequest(&forgev1.GetTemplateRequest{Name: "templates/go-grpc"}))
	if err != nil {
		t.Fatal(err)
	}
	tp := resp.Msg.GetTemplate()
	if tp.GetTitle() != "go-grpc title" || tp.GetOwner() != "platform" || len(tp.GetParameters()) != 3 {
		t.Fatalf("template: %v", tp)
	}
	port, tier, module := tp.GetParameters()[0], tp.GetParameters()[1], tp.GetParameters()[2]
	if port.GetType() != forgev1.TemplateParameter_TYPE_INT || port.GetRequired() || port.GetDefaultValue() != "8080" || port.GetDescription() != "Port" {
		t.Errorf("port: %v", port)
	}
	if tier.GetType() != forgev1.TemplateParameter_TYPE_ENUM || !tier.GetRequired() || strings.Join(tier.GetValues(), ",") != "gold,silver" {
		t.Errorf("tier: %v", tier)
	}
	if module.GetPattern() != "^[a-z]+$" || module.GetType() != forgev1.TemplateParameter_TYPE_STRING {
		t.Errorf("module: %v", module)
	}
	// A bare name works too, as for services.
	if _, err := s.GetTemplate(writerCtx(), connect.NewRequest(&forgev1.GetTemplateRequest{Name: "node-api"})); err != nil {
		t.Errorf("bare name: %v", err)
	}
}

func TestTemplateErrors(t *testing.T) {
	t.Parallel()
	get := func(src TemplateSource, ctx context.Context, name, version string) error {
		s, _ := templateService(src)
		_, err := s.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{Name: name, Version: version}))
		return err
	}
	ctx := writerCtx()
	tests := []struct {
		name string
		err  error
		want connect.Code
		msg  string
	}{
		{"unknown template", get(twoReleases, ctx, "templates/nope", ""), connect.CodeNotFound, "templates/nope not found in v1.1.0"},
		{"unknown version", get(twoReleases, ctx, "templates/go-grpc", "v9.0.0"), connect.CodeNotFound, "v9.0.0"},
		{"broken template", get(twoReleases, ctx, "templates/broken", ""), connect.CodeFailedPrecondition, "nope"},
		{"bad name", get(twoReleases, ctx, "templates/Bad_Name", ""), connect.CodeInvalidArgument, "must look like templates/{template}"},
		{"not configured", get(nil, ctx, "templates/go-grpc", ""), connect.CodeFailedPrecondition, "templates are not configured"},
		{"no releases", get(staticSource{}, ctx, "templates/go-grpc", ""), connect.CodeFailedPrecondition, "no release tags"},
		{"source down", get(staticSource{err: errors.New("github: 503")}, ctx, "templates/go-grpc", ""), connect.CodeUnavailable, "503"},
		{"anonymous", get(twoReleases, context.Background(), "templates/go-grpc", ""), connect.CodeUnauthenticated, ""},
	}
	for _, tt := range tests {
		if connect.CodeOf(tt.err) != tt.want || !strings.Contains(tt.err.Error(), tt.msg) {
			t.Errorf("%s: got %v, want %v containing %q", tt.name, tt.err, tt.want, tt.msg)
		}
	}
}

// The service is reachable through New with any authenticated caller.
func TestTemplateServiceWired(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{Authenticator: StaticAuthenticator{}, Templates: twoReleases}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/server -run Template`
Expected: FAIL to compile (`undefined: templateService`, `unknown field Templates`).

- [ ] **Step 4: Implement** — `internal/server/templates.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

const templatePrefix = "templates/"

// templateService serves TemplateService from a TemplateSource. Any
// authenticated caller may read templates.
type templateService struct {
	src TemplateSource
	log *slog.Logger
}

func authenticated(ctx context.Context) error {
	if _, ok := PrincipalFrom(ctx); !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	return nil
}

// release resolves version (empty = newest) and fetches its tree.
func (s *templateService) release(ctx context.Context, version string) (string, fs.FS, error) {
	if err := authenticated(ctx); err != nil {
		return "", nil, err
	}
	if s.src == nil {
		return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	if version == "" {
		vs, err := s.src.Versions(ctx)
		if err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list template versions: %w", err))
		}
		if len(vs) == 0 {
			return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the template repo has no release tags yet"))
		}
		version = vs[0]
	}
	tree, err := s.src.Fetch(ctx, version)
	if errors.Is(err, ErrVersionNotFound) {
		return "", nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return "", nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return version, tree, nil
}

var errTemplateNotFound = errors.New("template not found")

// loadTemplate parses and validates template name in tree.
func loadTemplate(tree fs.FS, name string) (*template.Template, error) {
	data, err := fs.ReadFile(tree, path.Join(name, template.FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := template.Parse(data)
	if err != nil {
		return nil, err
	}
	if findings := t.Validate(); len(findings) > 0 {
		return nil, errors.New(findings[0].String())
	}
	if t.Name != name {
		return nil, fmt.Errorf("template.yaml names %q, not %q", t.Name, name)
	}
	return t, nil
}

func (s *templateService) ListTemplates(ctx context.Context, req *connect.Request[forgev1.ListTemplatesRequest]) (*connect.Response[forgev1.ListTemplatesResponse], error) {
	version, tree, err := s.release(ctx, req.Msg.GetVersion())
	if err != nil {
		return nil, err
	}
	names, err := template.Discover(tree)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read templates of %s: %w", version, err))
	}
	resp := &forgev1.ListTemplatesResponse{}
	for _, name := range names {
		t, err := loadTemplate(tree, name)
		if err != nil {
			s.log.WarnContext(ctx, "skipping invalid template", "template", name, "version", version, "error", err)
			continue
		}
		if tag := req.Msg.GetTag(); tag != "" && !slices.Contains(t.Tags, tag) {
			continue
		}
		resp.Templates = append(resp.Templates, templateProto(t, version))
	}
	return connect.NewResponse(resp), nil
}

func (s *templateService) GetTemplate(ctx context.Context, req *connect.Request[forgev1.GetTemplateRequest]) (*connect.Response[forgev1.GetTemplateResponse], error) {
	name := strings.TrimPrefix(req.Msg.GetName(), templatePrefix)
	if !manifest.ValidID(name) {
		return nil, invalid(fmt.Sprintf("name %q must look like templates/{template}", req.Msg.GetName()))
	}
	version, tree, err := s.release(ctx, req.Msg.GetVersion())
	if err != nil {
		return nil, err
	}
	t, err := loadTemplate(tree, name)
	if errors.Is(err, errTemplateNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found in %s", templatePrefix, name, version))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s in %s is invalid: %w", templatePrefix, name, version, err))
	}
	return connect.NewResponse(&forgev1.GetTemplateResponse{Template: templateProto(t, version)}), nil
}

var paramTypes = map[template.ParamType]forgev1.TemplateParameter_Type{
	template.TypeString: forgev1.TemplateParameter_TYPE_STRING,
	template.TypeInt:    forgev1.TemplateParameter_TYPE_INT,
	template.TypeBool:   forgev1.TemplateParameter_TYPE_BOOL,
	template.TypeEnum:   forgev1.TemplateParameter_TYPE_ENUM,
}

func templateProto(t *template.Template, version string) *forgev1.Template {
	out := &forgev1.Template{
		Name:        templatePrefix + t.Name,
		Version:     version,
		Title:       t.Title,
		Description: t.Description,
		Tags:        slices.Clone(t.Tags),
		Owner:       t.Owner,
	}
	for _, p := range t.Parameters {
		def, _ := p.DefaultString()
		out.Parameters = append(out.Parameters, &forgev1.TemplateParameter{
			Name:         p.Name,
			Type:         paramTypes[p.Type],
			Required:     p.Required(),
			DefaultValue: def,
			Pattern:      p.Pattern,
			Values:       slices.Clone(p.Values),
			Description:  p.Description,
		})
	}
	return out
}
```

In `internal/server/server.go`: add to `Options` (after `Catalog *Catalog`):

```go
	// Templates serves TemplateService; nil answers FAILED_PRECONDITION.
	Templates TemplateSource
```

and in `New`, after the `AuthServiceHandler` line:

```go
	mux.Handle(forgev1connect.NewTemplateServiceHandler(&templateService{src: o.Templates, log: o.Logger}, opts))
```

In `pkg/client/client.go`: add the field `Templates forgev1connect.TemplateServiceClient` to `Client` (after `Auth`) and in `New` set `Templates: forgev1connect.NewTemplateServiceClient(hc, baseURL, opts...),`.

- [ ] **Step 5: Run the tests**

Run: `go test -count=1 -race ./internal/server ./pkg/... -v -run 'Template|Untar|Source' && go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 6: Run the gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint`
Expected: all ok, `0 issues.`, `buf lint` silent.

```bash
git add proto gen internal/server pkg/client
git commit -m "api: TemplateService lists and gets templates of a release

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: forge-api configuration

**Files:**
- Create: `cmd/forge-api/templates.go`, `cmd/forge-api/templates_test.go`
- Modify: `cmd/forge-api/main.go` (flags in `run`, `server.Options`)

**Interfaces:**
- Consumes: `server.DirSource`, `server.NewGitHubSource`, `server.TemplateSource` (Tasks 3–4); `github.Client`, `github.StaticToken` (Task 1).
- Produces: `func templateSource(dir, repo, tokenFile string) (server.TemplateSource, error)` — dir wins, then token file, else nil.

- [ ] **Step 1: Write the failing test** — `cmd/forge-api/templates_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/server"
)

func TestTemplateSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("  ghp_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if src, err := templateSource(dir, "showmino/forge-templates", token); err != nil {
		t.Fatal(err)
	} else if _, ok := src.(server.DirSource); !ok {
		t.Errorf("a directory wins over a token: %T", src)
	}
	if src, err := templateSource("", "showmino/forge-templates", token); err != nil {
		t.Fatal(err)
	} else if _, ok := src.(*server.GitHubSource); !ok {
		t.Errorf("token file: %T", src)
	}
	if src, err := templateSource("", "showmino/forge-templates", ""); err != nil || src != nil {
		t.Errorf("nothing configured: %v %v", src, err)
	}
	for name, args := range map[string][3]string{
		"missing dir":   {filepath.Join(dir, "nope"), "", ""},
		"missing token": {"", "showmino/forge-templates", filepath.Join(dir, "nope")},
		"empty token":   {"", "showmino/forge-templates", empty},
		"bad repo":      {"", "forge-templates", token},
	} {
		_, err := templateSource(args[0], args[1], args[2])
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "ghp_secret") {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/forge-api`
Expected: FAIL to compile (`undefined: templateSource`).

- [ ] **Step 3: Implement** — `cmd/forge-api/templates.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

// templateSource picks where the API reads templates from: a local checkout
// (development) or the GitHub repo with a read-only token. It returns nil when
// neither is configured; TemplateService then answers FAILED_PRECONDITION.
func templateSource(dir, repo, tokenFile string) (server.TemplateSource, error) {
	switch {
	case dir != "":
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("--templates-dir %s is not a directory", dir)
		}
		return server.DirSource{Dir: dir}, nil
	case tokenFile != "":
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read --github-token-file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return nil, errors.New("--github-token-file is empty")
		}
		return server.NewGitHubSource(&github.Client{Token: github.StaticToken(token)}, repo)
	default:
		return nil, nil
	}
}
```

In `cmd/forge-api/main.go`, add to the `var (...)` flag block:

```go
		templatesDir  = flag.String("templates-dir", env("FORGE_TEMPLATES_DIR", ""), "serve templates from this local checkout as version 'dev' (development)")
		templatesRepo = flag.String("templates-repo", env("FORGE_TEMPLATES_REPO", "showmino/forge-templates"), "GitHub repo (owner/name) whose semver tags are template releases")
		tokenFile     = flag.String("github-token-file", env("FORGE_GITHUB_TOKEN_FILE", ""), "file with a read-only GitHub token for --templates-repo")
```

After the writer-groups loop and before `server.New`:

```go
	templates, err := templateSource(*templatesDir, *templatesRepo, *tokenFile)
	if err != nil {
		return err
	}
	if templates == nil {
		log.Info("templates are not configured; set --templates-dir or --github-token-file")
	}
```

Add `Templates: templates,` as the first field of the existing `server.New(server.Options{...})` literal. The existing `h, err := server.New(...)` still compiles: `h` is new, so `:=` reuses `err`. Run `gofmt -w cmd/forge-api` to align the fields.

- [ ] **Step 4: Run the tests and a smoke start**

Run: `go test -count=1 ./cmd/forge-api && go build -o bin/ ./cmd/... && (bin/forge-api --insecure-dev-token --addr :18081 --templates-dir /nope 2>&1 | tail -1)`
Expected: test PASS; the server exits with `forge-api: --templates-dir /nope is not a directory`.

- [ ] **Step 5: Commit**

```bash
git add cmd/forge-api && git commit -m "forge-api: --templates-dir, --templates-repo and --github-token-file

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `forge template list` and `forge template get`

**Files:**
- Modify: `internal/cli/template.go` (`newTemplateCmd`, new verbs)
- Modify: `internal/cli/cli_test.go` (`harness`: `authn`, `templates` fields; `startAPI`)
- Test: `internal/cli/template_test.go` (append), golden files `internal/cli/testdata/template_list_table.golden`, `template_list_json.golden`, `template_get_table.golden`, `template_get_json.golden`
- Modify: `README.md`, `.claude/skills/forge/SKILL.md`

**Interfaces:**
- Consumes: `client.Client.Templates` (Task 4), `server.DirSource` (Task 3), `writeTree` and `lintManifest` (milestone 1, `internal/cli/template_test.go`).
- Produces: `forge template list [--tag T] [--version V]`, `forge template get <name>[@version]`.

- [ ] **Step 1: Let the harness serve templates** — in `internal/cli/cli_test.go` add two fields to `harness`:

```go
	authn     server.Authenticator
	templates server.TemplateSource
```

In `startAPI`, set `h.authn = authn` as its first statement and add `Templates: h.templates,` to the `server.Options`. Add this method after `startAPI`:

```go
// serveTemplates restarts the in-process API with templates from dir (version "dev").
func (h *harness) serveTemplates(dir string) {
	h.templates = server.DirSource{Dir: dir}
	h.startAPI(h.authn)
}
```

- [ ] **Step 2: Write the failing tests** — append to `internal/cli/template_test.go`:

```go
var templateRepo = map[string]string{
	"svc/template.yaml": "apiVersion: forge.dev/v1\nkind: Template\nname: svc\ntitle: Go service\ndescription: Go service fixture\ntags: [go, grpc]\nowner: platform\nparameters:\n" +
		"  - name: port\n    type: int\n    default: 8080\n    description: Port the server listens on\n" +
		"  - name: tier\n    type: enum\n    values: [gold, silver]\n    description: Support tier\n",
	"svc/skeleton/forge.yaml": lintManifest,
	"web/template.yaml":       "apiVersion: forge.dev/v1\nkind: Template\nname: web\ntitle: Web app\ndescription: Web fixture\ntags: [node]\n",
	"web/skeleton/forge.yaml": lintManifest,
}

func TestTemplateListAndGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	h.serveTemplates(filepath.Join(h.dir, "repo"))

	golden(t, "template_list_table", h.ok("template", "list").out)
	golden(t, "template_list_json", h.ok("template", "list", "-o", "json").out)
	if r := h.ok("template", "list", "-o", "name"); r.out != "templates/svc\ntemplates/web\n" {
		t.Errorf("names: %q", r.out)
	}
	if r := h.ok("template", "list", "--tag", "node", "-o", "name"); r.out != "templates/web\n" {
		t.Errorf("--tag: %q", r.out)
	}
	r := h.ok("template", "list", "--tag", "rust")
	if r.out != "" || !strings.Contains(r.err, "No templates found.") {
		t.Errorf("no match, table: %+v", r)
	}
	if r := h.ok("template", "list", "--tag", "rust", "-o", "json"); !strings.Contains(r.out, `"templates": []`) {
		t.Errorf("no match, json: %q", r.out)
	}

	golden(t, "template_get_table", h.ok("template", "get", "svc").out)
	golden(t, "template_get_json", h.ok("template", "get", "templates/svc@dev", "-o", "json").out)
	if r := h.ok("template", "get", "web"); strings.Contains(r.out, "REQUIRED") {
		t.Errorf("a template without parameters prints no parameter table:\n%s", r.out)
	}
}

func TestTemplateGetErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fails(6, "template", "list") // the API has no template source
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	h.serveTemplates(filepath.Join(h.dir, "repo"))
	h.fails(3, "template", "get", "nope")
	h.fails(3, "template", "get", "svc@v1.0.0") // a directory serves only "dev"
	h.fails(3, "template", "list", "--version", "v1.0.0")
	h.fails(2, "template", "get", "Bad_Name")
	h.fails(2, "template", "get")
	delete(h.env, "FORGE_TOKEN")
	h.fails(4, "template", "list")
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/cli -run 'TemplateListAndGet|TemplateGetErrors'`
Expected: FAIL: `unknown command "list" for "forge template"` (exit 2 where 0 was wanted).

- [ ] **Step 4: Implement** — in `internal/cli/template.go`, change `newTemplateCmd`'s `AddCommand` line to:

```go
	cmd.AddCommand(newTemplateListCmd(f), newTemplateGetCmd(f), newTemplateLintCmd(f))
```

Add these imports to the file: `"strings"`, `"connectrpc.com/connect"`, `forgev1 "github.com/showmino/forge/gen/forge/v1"`. Then add:

```go
func newTemplateListCmd(f *Factory) *cobra.Command {
	var tag, version string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the templates of a release",
		Long:    "List the templates of a release of the template repo, the newest by default.",
		Example: `  forge template list
  forge template list --tag go
  forge template list --version v1.0.0 -o json | jq -r '.templates[].name'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Templates.ListTemplates(ctx, connect.NewRequest(&forgev1.ListTemplatesRequest{Version: version, Tag: tag}))
			if err != nil {
				return err
			}
			ts := resp.Msg.GetTemplates()
			if len(ts) == 0 && format == output.Table {
				f.infof("No templates found.")
				return nil
			}
			v := output.View{Headers: []string{"name", "version", "tags", "description"}, Data: resp.Msg}
			if len(ts) == 0 {
				// protojson omits empty lists; keep '.templates[]' working in jq.
				v.Data = map[string]any{"templates": []any{}}
			}
			for _, t := range ts {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(t.GetName(), "templates/"), t.GetVersion(), strings.Join(t.GetTags(), ", "), t.GetDescription()})
				v.Names = append(v.Names, t.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "only templates with this tag")
	cmd.Flags().StringVar(&version, "version", "", "release tag (default: the newest)")
	return cmd
}

func newTemplateGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>[@version]",
		Short: "Show a template and its parameters",
		Example: `  forge template get go-grpc
  forge template get go-grpc@v1.0.0 -o json | jq '.parameters'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, version, _ := strings.Cut(args[0], "@")
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Templates.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{
				Name:    "templates/" + strings.TrimPrefix(name, "templates/"),
				Version: version,
			}))
			if err != nil {
				return err
			}
			t := resp.Msg.GetTemplate()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{t.GetName()}, Data: t})
			}
			if err := output.Print(f.IO.Out, format, output.View{
				Headers: []string{"field", "value"},
				Rows: [][]string{
					{"name", t.GetName()},
					{"version", t.GetVersion()},
					{"title", t.GetTitle()},
					{"description", t.GetDescription()},
					{"tags", strings.Join(t.GetTags(), ", ")},
					{"owner", t.GetOwner()},
				},
			}); err != nil {
				return err
			}
			if len(t.GetParameters()) == 0 {
				return nil
			}
			params := output.View{Headers: []string{"parameter", "type", "required", "default", "description"}}
			for _, p := range t.GetParameters() {
				required := "no"
				if p.GetRequired() {
					required = "yes"
				}
				params.Rows = append(params.Rows, []string{p.GetName(), paramType(p), required, p.GetDefaultValue(), p.GetDescription()})
			}
			if _, err := fmt.Fprintln(f.IO.Out); err != nil {
				return err
			}
			return output.Print(f.IO.Out, format, params)
		},
	}
}

// paramType reads "int", "string", or "enum (gold, silver)".
func paramType(p *forgev1.TemplateParameter) string {
	s := strings.ToLower(strings.TrimPrefix(p.GetType().String(), "TYPE_"))
	if p.GetType() == forgev1.TemplateParameter_TYPE_ENUM {
		s += " (" + strings.Join(p.GetValues(), ", ") + ")"
	}
	return s
}
```

- [ ] **Step 5: Create the golden files and review them**

Run: `go test ./internal/cli -run TemplateListAndGet -update && cat internal/cli/testdata/template_list_table.golden internal/cli/testdata/template_get_table.golden && head -20 internal/cli/testdata/template_get_json.golden`
Expected: the list table has rows `svc  dev  go, grpc  Go service fixture` and `web  dev  node  Web fixture` under `NAME VERSION TAGS DESCRIPTION`; the get table shows the six fields, a blank line, then `PARAMETER TYPE REQUIRED DEFAULT DESCRIPTION` with `port int no 8080 Port the server listens on` and `tier enum (gold, silver) yes - Support tier` (the printer shows empty cells as `-`; if it does not, an empty cell is fine); the JSON is the `Template` resource (`"name": "templates/svc"`, `"defaultValue": "8080"`, `"type": "TYPE_ENUM"`). Fix the code, not the golden files, if anything differs.

- [ ] **Step 6: Run the CLI tests**

Run: `go test -count=1 ./internal/cli`
Expected: PASS.

- [ ] **Step 7: Document** — in `README.md` change the CLI table row for `template` to:

```markdown
| `template` | `list`, `get`, `lint` (Phase 2 adds `forge create`) |
```

and replace the sentence in the "Templates" section that starts "`forge template lint [dir]` checks them locally" with:

```markdown
`forge template list` and `forge template get <name>[@version]` show the
templates of a release (the newest by default). forge-api reads them from
GitHub with `--github-token-file` (a read-only token for the template repo),
or from a local checkout with `--templates-dir` (served as version `dev`).
`forge template lint [dir]` checks templates locally, with no API or login;
see `docs/specs/2026-10-08-phase-2-templates-design.md` for the format.
```

In `.claude/skills/forge/SKILL.md`, in the Phase 2 status line, replace "then `forge template list/get` and `forge create`;" with "milestone 2 (`TemplateService`, `forge template list/get`, forge-api `--templates-dir` / `--github-token-file`) per `docs/plans/2026-10-08-phase-2-m2-template-service.md`; then `forge create`;".

- [ ] **Step 8: Run the gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint && make update-golden && git status --short`
Expected: gate passes; `make update-golden` changes no existing golden file.

```bash
git add internal/cli README.md .claude/skills/forge/SKILL.md
git commit -m "cli: forge template list and get

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: Check it against the real template repo

**Files:** none (verification only; nothing to commit unless a defect is found, which then gets a failing test first).

**Interfaces:**
- Consumes: everything above; `showmino/forge-templates` tag `v1.0.0`; the owner's `gh` login (repo scope), used only for this local check.

- [ ] **Step 1: Local checkout as version `dev`**

```bash
make build
bin/forge-api --insecure-dev-token --addr :18081 --templates-dir ../forge-templates &
sleep 1
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 template list
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 template get go-grpc
kill %1
```

Expected: `go-grpc  dev  go, grpc  ConnectRPC service in Go with CI, Dockerfile and forge.yaml`; `get` shows parameters `goModule` (string, default `github.com/{% .Repo.Owner %}/{% .Repo.Name %}`) and `port` (int, default `8080`).

- [ ] **Step 2: GitHub release tags**

```bash
T=$(mktemp) && chmod 600 "$T" && gh auth token > "$T"
bin/forge-api --insecure-dev-token --addr :18081 --github-token-file "$T" &
sleep 1
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 template list
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 template get go-grpc@v1.0.0 -o json | head -5
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 template get go-grpc@v0.9.0; echo "exit $?"
kill %1; rm -f "$T"
```

Expected: `go-grpc  v1.0.0  go, grpc  ...` (the newest tag), the JSON shows `"version": "v1.0.0"`, and `v0.9.0` exits 3. The token file is deleted afterwards; the token never appears in output or logs.

- [ ] **Step 3: Record the result**

Note in the ledger that both sources served `go-grpc` from the real repo. If anything failed, write a failing test that reproduces it in the owning task's test file, fix it, and commit with a message naming the defect.

---

## After Milestone 2

Milestone 3 (`internal/github` App auth and repo creation, the streamed `CreateService`, `forge create`) and milestone 4 (k3s and the exit gate) follow, with their own plans. Milestone 3 extends `github.Client` (App JWT and installation tokens as its `Token` func, `CreateRepo`, Git Data API pushes) and reuses `TemplateSource` and `templateProto`.
