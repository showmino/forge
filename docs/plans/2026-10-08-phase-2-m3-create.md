# Phase 2, Milestone 3: `forge create` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `forge create <name> --template <t> --team <team>` renders a template, creates a private GitHub repo in `showmino`, pushes the first commit (which starts its CI) and registers the service in the catalog, streaming progress; failures after the repo exists roll it back.

**Architecture:** A server-streaming `ServiceCatalogService.CreateService` RPC runs a pipeline in `internal/server/create.go`: check, fetch (milestone 2's `TemplateSource`), render (milestone 1's `internal/template`), create repo, push, register. GitHub work goes through a `RepoHost` interface, implemented by `internal/github.Client` authenticated as the `mino-forge` App (milestone 2's `github.App`) and by fakes in tests. Outcomes are kept per `request_id` so a retry never creates a second repo. The CLI validates parameters against `GetTemplate` first, prompts on a terminal, and prints the stream.

**Tech Stack:** Go 1.24, ConnectRPC server streaming, buf v1.47.2, GitHub REST API (`POST /orgs/{org}/repos`, `GET|DELETE /repos/{o}/{r}`, Git Data API `git/ref`, `git/blobs`, `git/trees`, `git/commits`, `git/refs`), Cobra.

**Spec:** `docs/specs/2026-10-08-phase-2-templates-design.md` (section 2 "Protos" `CreateService`, "The create pipeline", "GitHub App"; section 3 `forge create`; section 4 errors, security, testing; section 6 milestone 3). Design doc: `docs/index.md`.

## Global Constraints

- Go is on PATH (`~/.local/go`); full gate `GOTOOLCHAIN=go1.24.13 make test lint` (CI parity, golangci-lint v2.5.0). After editing `proto/`, run `make generate` and commit `gen/`; `buf breaking --against '.git#branch=main'` must pass.
- Work on branch `phase-2-m3` cut from up-to-date `main`; run `git branch --show-current` before every commit. The owner pushes and merges.
- No package-level mutable globals in `internal/cli`; commands use `*Factory`. Only `main` picks exit codes. Every command supports `-o table|json|yaml|name`.
- Errors (spec section 4): bad parameter, service id, team or owner → `INVALID_ARGUMENT` (2); template or version not found → `NOT_FOUND` (3); `services/{id}` or `showmino/{id}` exists → `ALREADY_EXISTS` (6); templates or GitHub not configured → `FAILED_PRECONDITION` (6); same `request_id` still running → `ABORTED` (6); GitHub unavailable before any side effect → `UNAVAILABLE` (1); a step fails after the repo exists → `CreateFailure` event (7); `--timeout` → 8.
- Pipeline steps and their order: check (no event), `fetch`, `render`, [validate_only: `CreatePlan` and stop], `create_repo`, `push`, `register`, then the `Service`. Each named step is sent as `CreateStep` STARTED then DONE.
- New repos are private, in the org `--github-org` (default `showmino`), named exactly the service id. Only callers with `services.write` may create.
- Never log or return the App key or installation tokens. The key file `mino-forge.2026-10-08.private-key.pem` (repo root, ignored) is never printed or committed.
- Creating a real repo on GitHub happens only in Task 7, after the owner confirms in that session.
- Commit after every task with the trailer `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

**Refinements of the spec made while planning** (Task 0 records them):

- The streamed message is `CreateServiceResponse`, not `CreateServiceEvent`: buf's STANDARD lint (used by this repo) requires an RPC's response message to be named `<Rpc>Response`. Its `oneof event` is unchanged.
- GitHub's Git Data API rejects writes to an empty repository, so `create_repo` creates the repo with `auto_init` (GitHub's initial commit), and `push` writes one commit with no parents and force-updates the default branch to it: the new repo's history starts with the scaffold.
- Text files go into the tree inline (`content`); only non-UTF-8 files are uploaded as blobs, so a typical scaffold costs four API calls, not one per file.
- Once `create_repo` starts, the pipeline runs to its end even if the client disconnects (`context.WithoutCancel`, bounded by the HTTP client's timeout), so the stored outcome is complete when the CLI retries with the same `request_id`.
- `forge create` pins `template_version` to the version `GetTemplate` resolved, so a release published between validation and creation cannot change what is rendered.

## Review Focus

1. **The client disconnects mid-create** (Ctrl-C, network drop after `create_repo`): the server still finishes or rolls back; a retry with the same `request_id` replays the outcome instead of creating a second repo. Pinned in Task 4.
2. **Binary and executable skeleton files** must arrive on GitHub byte-exact and with mode `100755` where executable. Pinned in Task 2.
3. **A brand-new repo's branch is not readable yet** (GitHub answers 404 or 409 for a moment): the push waits instead of failing. Pinned in Task 2.
4. **The name is free in the catalog but taken on GitHub** (a hand-made repo `showmino/payments`): exit 6 before anything is created. Pinned in Task 3.
5. **Rollback itself fails** (delete denied): the user is told the repo still exists and must be removed before retrying. Pinned in Tasks 4 and 6.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `docs/specs/2026-10-08-phase-2-templates-design.md` (modify) | Record the refinements |
| `internal/github/github.go` (modify) | Request bodies (`do` takes a JSON body), `doJSON`, `StatusError` |
| `internal/github/repos.go` (create) | `Repo`, `File`, `ErrRepoExists`, `RepoExists`, `CreateOrgRepo`, `DeleteRepo`, `PushFiles` |
| `internal/github/app.go` (modify) | `call` uses `doJSON` |
| `proto/forge/v1/catalog.proto` (modify), `gen/` | `CreateService` and its messages |
| `internal/server/templates.go` (modify) | `resolveRelease` and `templateFromRelease`, shared with create |
| `internal/server/create.go` (create) | `RepoHost`, `catalogService`, `creator`: validation, conflicts, pipeline, rollback, idempotency |
| `internal/server/catalog.go` (modify) | `Catalog.has`; the handler assertion moves to `catalogService` |
| `internal/server/server.go` (modify) | `Options.Repos`, `Options.GitHubOrg`; register `catalogService` |
| `cmd/forge-api/templates.go`, `main.go` (modify) | `repoHost(app)`; pass `Repos` and `GitHubOrg` |
| `internal/cli/create.go` (create), `internal/cli/prompt.go`, `factory.go`, `root.go` (modify) | `forge create`, a shared stdin reader |
| `internal/cli/cli_test.go`, `internal/cli/create_test.go` | Harness `repos`; CLI tests |
| `README.md`, `.claude/skills/forge/SKILL.md` (modify) | Docs |

---

### Task 0: Branch and spec refinements

**Files:**
- Modify: `docs/specs/2026-10-08-phase-2-templates-design.md`

**Interfaces:** none.

- [ ] **Step 1: Branch**

```bash
git switch main && git pull --ff-only && git switch -c phase-2-m3 && git branch --show-current
```

Expected: `phase-2-m3`. (If the plan commit is already on a `phase-2-m3` branch, stay on it.)

- [ ] **Step 2: Edit the spec**

In section 2, rename every `CreateServiceEvent` to `CreateServiceResponse` (the `rpc` line and the message), and add under the proto block:

```markdown
The streamed message is named `CreateServiceResponse` because buf's STANDARD lint requires `<Rpc>Response`.
```

Under "The create pipeline", replace step 5 and 6 text with:

```markdown
5. **create_repo:** create the private repo `showmino/{id}` with `auto_init` (GitHub's Git Data API cannot write to an empty repository).
6. **push:** write one commit with every rendered file and no parents, and force-update the default branch to it, so history starts with the scaffold. Text files go into the tree inline; other files are uploaded as blobs. The `.github/workflows` it contains start CI.
```

and add after the idempotency paragraph:

```markdown
Once `create_repo` starts, the pipeline runs to its end even if the client disconnects, so the stored outcome is complete when the client retries. `forge create` pins `template_version` to the version `GetTemplate` resolved.
```

- [ ] **Step 3: Commit**

```bash
git add docs/specs && git commit -m "spec: CreateServiceResponse, auto_init and orphan commit, disconnect handling

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 1: GitHub requests with bodies and typed status errors

**Files:**
- Modify: `internal/github/github.go`, `internal/github/app.go` (`call`)
- Test: `internal/github/github_test.go` (append)

**Interfaces:**
- Produces:
  - `type StatusError struct { Method, Path string; Status int; Body string }` with `Error()` = `"<method> <path>: <status> <text>: <body>"` and `Is(ErrNotFound)` true for 404
  - `func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error)` — `in` is JSON-encoded when non-nil
  - `func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error` — `out` nil discards the body

- [ ] **Step 1: Write the failing tests** — append to `internal/github/github_test.go`:

```go
func TestDoJSON(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ A int }
		if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil || in.A != 1 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"b":2}`)
	})
	mux.HandleFunc("POST /taken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"name already exists on this account"}`, http.StatusUnprocessableEntity)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}

	var out struct{ B int }
	if err := c.doJSON(context.Background(), http.MethodPost, "/echo", map[string]int{"a": 1}, &out); err != nil || out.B != 2 {
		t.Errorf("doJSON: %v %+v", err, out)
	}
	err := c.doJSON(context.Background(), http.MethodPost, "/taken", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || !strings.Contains(se.Body, "already exists") {
		t.Errorf("422: %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a 422 is not ErrNotFound")
	}
}
```

Add `"encoding/json"` to the test imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/github -run DoJSON`
Expected: FAIL to compile (`c.doJSON undefined` / `undefined: StatusError`).

- [ ] **Step 3: Implement** — in `internal/github/github.go`, add `"bytes"` to the imports, and replace the `do` function with:

```go
// StatusError is a non-2xx answer from GitHub.
type StatusError struct {
	Method, Path string
	Status       int
	// Body is at most 512 bytes of the response.
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Body)
}

// Is makes errors.Is(err, ErrNotFound) true for 404 answers.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// do sends a request with in as its JSON body (none when nil) and returns
// the response if it answered 2xx; the caller closes its body.
func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, &StatusError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
}

// doJSON is do that decodes a JSON reply into out (discarded when out is nil).
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}
```

In `ListTags` and `Tarball`, change `c.do(ctx, http.MethodGet, <path>)` to `c.do(ctx, http.MethodGet, <path>, nil)`. In `internal/github/app.go`, replace the body of `call` after the JWT with:

```go
	c := &Client{BaseURL: a.BaseURL, HTTP: a.HTTP, Token: StaticToken(jwt)}
	return c.doJSON(ctx, method, path, nil, out)
```

- [ ] **Step 4: Run all GitHub tests**

Run: `go test -count=1 -race ./internal/github`
Expected: PASS (the earlier tests still find "500", "boom", "401" and `ErrNotFound`).

- [ ] **Step 5: Commit**

```bash
git add internal/github && git commit -m "github: JSON request bodies and typed status errors

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Creating, filling and deleting repos

**Files:**
- Create: `internal/github/repos.go`
- Test: `internal/github/repos_test.go`

**Interfaces:**
- Consumes: `doJSON`, `StatusError`, `ErrNotFound` (Task 1).
- Produces:
  - `type Repo struct { FullName string; HTMLURL string; DefaultBranch string }` (JSON `full_name`, `html_url`, `default_branch`)
  - `type File struct { Path string; Executable bool; Content []byte }`
  - `var ErrRepoExists error`
  - `func (c *Client) RepoExists(ctx context.Context, owner, name string) (bool, error)`
  - `func (c *Client) CreateOrgRepo(ctx context.Context, org, name, description string) (*Repo, error)` — private, `auto_init`; 422 "already exists" wraps `ErrRepoExists`
  - `func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error`
  - `func (c *Client) PushFiles(ctx context.Context, owner, repo, branch, message string, files []File) (string, error)` — returns the commit SHA

- [ ] **Step 1: Write the failing tests** — `internal/github/repos_test.go`:

```go
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeOrg is a GitHub org with just enough of the repos and Git Data APIs.
type fakeOrg struct {
	mu       sync.Mutex
	repos    map[string]bool
	blobs    []string         // base64 contents uploaded as blobs
	tree     []map[string]any // entries of the last tree
	commit   map[string]any   // body of the last commit
	ref      map[string]any   // body of the last ref update
	refMiss  atomic.Int32     // answer 409 to this many ref reads first
	refReads atomic.Int32
}

func newFakeOrg(t *testing.T) (*fakeOrg, *Client) {
	t.Helper()
	o := &fakeOrg{repos: map[string]bool{"taken": true}}
	decode := func(r *http.Request, v any) { _ = json.NewDecoder(r.Body).Decode(v) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/{name}", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if !o.repos[r.PathValue("name")] {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("POST /orgs/showmino/repos", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string `json:"name"`
			Private  bool   `json:"private"`
			AutoInit bool   `json:"auto_init"`
		}
		decode(r, &in)
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.repos[in.Name] {
			http.Error(w, `{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`, http.StatusUnprocessableEntity)
			return
		}
		if !in.Private || !in.AutoInit {
			http.Error(w, "want private and auto_init", http.StatusBadRequest)
			return
		}
		o.repos[in.Name] = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"full_name":"showmino/%s","html_url":"https://github.com/showmino/%s","default_branch":"main"}`, in.Name, in.Name)
	})
	mux.HandleFunc("DELETE /repos/showmino/{name}", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.repos, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/showmino/svc/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		o.refReads.Add(1)
		if o.refMiss.Add(-1) >= 0 {
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
			return
		}
		fmt.Fprint(w, `{"object":{"sha":"init"}}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Content, Encoding string }
		decode(r, &in)
		o.mu.Lock()
		o.blobs = append(o.blobs, in.Content)
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"blob1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tree []map[string]any `json:"tree"`
		}
		decode(r, &in)
		o.mu.Lock()
		o.tree = in.Tree
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"tree1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(r, &in)
		o.mu.Lock()
		o.commit = in
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"commit1abcdef"}`)
	})
	mux.HandleFunc("PATCH /repos/showmino/svc/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(r, &in)
		o.mu.Lock()
		o.ref = in
		o.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return o, &Client{BaseURL: srv.URL}
}

func TestCreateAndDeleteRepo(t *testing.T) {
	t.Parallel()
	o, c := newFakeOrg(t)
	ctx := context.Background()
	if ok, err := c.RepoExists(ctx, "showmino", "svc"); err != nil || ok {
		t.Errorf("exists before create: %v %v", ok, err)
	}
	r, err := c.CreateOrgRepo(ctx, "showmino", "svc", "Go service")
	if err != nil || r.DefaultBranch != "main" || r.HTMLURL != "https://github.com/showmino/svc" {
		t.Fatalf("create: %+v %v", r, err)
	}
	if ok, _ := c.RepoExists(ctx, "showmino", "svc"); !ok {
		t.Error("created repo does not exist")
	}
	if _, err := c.CreateOrgRepo(ctx, "showmino", "taken", ""); !errors.Is(err, ErrRepoExists) {
		t.Errorf("taken name: %v", err)
	}
	if err := c.DeleteRepo(ctx, "showmino", "svc"); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.repos["svc"] {
		t.Error("repo still exists after delete")
	}
}

func TestPushFiles(t *testing.T) {
	t.Parallel()
	o, c := newFakeOrg(t)
	o.refMiss.Store(2) // a new repo's branch is not readable at first
	binary := []byte{0xff, 0x00, 0xfe}
	sha, err := c.PushFiles(context.Background(), "showmino", "svc", "main", "Scaffold svc", []File{
		{Path: "README.md", Content: []byte("# svc\n")},
		{Path: "run.sh", Executable: true, Content: []byte("#!/bin/sh\n")},
		{Path: "logo.bin", Content: binary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sha != "commit1abcdef" || o.refReads.Load() != 3 {
		t.Errorf("sha %q after %d ref reads", sha, o.refReads.Load())
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	byPath := map[string]map[string]any{}
	for _, e := range o.tree {
		byPath[e["path"].(string)] = e
	}
	if e := byPath["README.md"]; e["content"] != "# svc\n" || e["mode"] != "100644" {
		t.Errorf("README.md entry: %v", e)
	}
	if e := byPath["run.sh"]; e["mode"] != "100755" {
		t.Errorf("run.sh must be executable: %v", e)
	}
	if e := byPath["logo.bin"]; e["sha"] != "blob1" || e["content"] != nil {
		t.Errorf("logo.bin must be a blob: %v", e)
	}
	if len(o.blobs) != 1 || o.blobs[0] != base64.StdEncoding.EncodeToString(binary) {
		t.Errorf("blobs: %v", o.blobs)
	}
	if parents, _ := o.commit["parents"].([]any); len(parents) != 0 || o.commit["tree"] != "tree1" || o.commit["message"] != "Scaffold svc" {
		t.Errorf("commit: %v", o.commit)
	}
	if o.ref["sha"] != "commit1abcdef" || o.ref["force"] != true {
		t.Errorf("ref update: %v", o.ref)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/github -run 'CreateAndDelete|PushFiles'`
Expected: FAIL to compile (`c.RepoExists undefined`).

- [ ] **Step 3: Implement** — `internal/github/repos.go`:

```go
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Repo is a repository GitHub created.
type Repo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

// File is one file of a commit.
type File struct {
	Path       string
	Executable bool
	Content    []byte
}

// ErrRepoExists means a repository of that name already exists.
var ErrRepoExists = errors.New("the repository already exists")

func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// RepoExists reports whether owner/name exists and is visible to the token.
func (c *Client) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, repoPath(owner, name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CreateOrgRepo creates a private repository in org with GitHub's initial
// commit: the Git Data API cannot write to an empty repository.
func (c *Client) CreateOrgRepo(ctx context.Context, org, name, description string) (*Repo, error) {
	var r Repo
	err := c.doJSON(ctx, http.MethodPost, "/orgs/"+url.PathEscape(org)+"/repos", map[string]any{
		"name":        name,
		"description": description,
		"private":     true,
		"auto_init":   true,
	}, &r)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity && strings.Contains(se.Body, "already exists") {
		return nil, fmt.Errorf("%s/%s: %w", org, name, ErrRepoExists)
	}
	if err != nil {
		return nil, err
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	return &r, nil
}

// DeleteRepo deletes owner/name.
func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error {
	return c.doJSON(ctx, http.MethodDelete, repoPath(owner, name), nil, nil)
}

// refWait is the first wait for a new repository's branch; it grows linearly.
const refWait = 500 * time.Millisecond

// PushFiles replaces the content of branch with files in one commit that has
// no parents, force-updating the branch, and returns the commit's SHA. Text
// files go into the tree inline; other files are uploaded as blobs.
func (c *Client) PushFiles(ctx context.Context, owner, repo, branch, message string, files []File) (string, error) {
	git := repoPath(owner, repo) + "/git"
	if err := c.waitForRef(ctx, git+"/ref/heads/"+branch); err != nil {
		return "", err
	}
	entries := make([]map[string]any, 0, len(files))
	for _, f := range files {
		mode := "100644"
		if f.Executable {
			mode = "100755"
		}
		e := map[string]any{"path": f.Path, "mode": mode, "type": "blob"}
		if utf8.Valid(f.Content) {
			e["content"] = string(f.Content)
		} else {
			var blob struct {
				SHA string `json:"sha"`
			}
			in := map[string]string{"content": base64.StdEncoding.EncodeToString(f.Content), "encoding": "base64"}
			if err := c.doJSON(ctx, http.MethodPost, git+"/blobs", in, &blob); err != nil {
				return "", fmt.Errorf("upload %s: %w", f.Path, err)
			}
			e["sha"] = blob.SHA
		}
		entries = append(entries, e)
	}
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/trees", map[string]any{"tree": entries}, &tree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPatch, git+"/refs/heads/"+branch, map[string]any{"sha": commit.SHA, "force": true}, nil); err != nil {
		return "", fmt.Errorf("update %s: %w", branch, err)
	}
	return commit.SHA, nil
}

// waitForRef reads a branch until it exists: right after creation GitHub
// can answer 404 or 409 ("Git Repository is empty") for a moment.
func (c *Client) waitForRef(ctx context.Context, path string) error {
	var err error
	for attempt := range 6 {
		if err = c.doJSON(ctx, http.MethodGet, path, nil, nil); err == nil {
			return nil
		}
		var se *StatusError
		if !errors.As(err, &se) || (se.Status != http.StatusNotFound && se.Status != http.StatusConflict) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(refWait * time.Duration(attempt+1)):
		}
	}
	return fmt.Errorf("the new repository's branch never appeared: %w", err)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -race ./internal/github -v -run 'CreateAndDelete|PushFiles'`
Expected: PASS (`TestPushFiles` takes about 1.5 s: two 409s, waits of 0.5 s and 1 s).

- [ ] **Step 5: Commit**

```bash
git add internal/github && git commit -m "github: create, fill and delete repos

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `CreateService` contract, wiring and pre-flight checks

**Files:**
- Modify: `proto/forge/v1/catalog.proto`; generated `gen/forge/v1/catalog.pb.go`, `gen/forge/v1/forgev1connect/catalog.connect.go`
- Modify: `internal/server/templates.go` (extract `resolveRelease`, `templateFromRelease`)
- Create: `internal/server/create.go`
- Modify: `internal/server/catalog.go` (`has`; handler assertion), `internal/server/server.go` (`Options`, `New`)
- Test: `internal/server/create_test.go`

**Interfaces:**
- Consumes: `github.Repo`, `github.File`, `github.ErrRepoExists` (Task 2); `TemplateSource`, `loadTemplate` (milestone 2); `require`, `invalid`, `PermServicesWrite`.
- Produces:
  - proto: `rpc CreateService(CreateServiceRequest) returns (stream CreateServiceResponse)`; messages `CreateServiceRequest`, `CreateServiceResponse` (`oneof event { CreateStep step = 1; Service service = 2; CreateFailure failure = 3; CreatePlan plan = 4; }`), `CreateStep` (nested `enum State`), `CreateFailure`, `CreatePlan`
  - `type RepoHost interface { RepoExists(ctx, owner, name string) (bool, error); CreateOrgRepo(ctx, org, name, description string) (*github.Repo, error); DeleteRepo(ctx, owner, name string) error; PushFiles(ctx, owner, repo, branch, message string, files []github.File) (string, error) }`
  - `server.Options.Repos RepoHost`, `server.Options.GitHubOrg string` (default `showmino`)
  - `func resolveRelease(ctx context.Context, src TemplateSource, version string) (string, fs.FS, error)` and `func templateFromRelease(tree fs.FS, name, version string) (*template.Template, error)` — both return Connect errors
  - in `create.go`: `catalogService`, `creator` with `run(ctx, req, stream createStream) error`; Task 4 adds the pipeline body `create`

- [ ] **Step 1: Write the contract** — in `proto/forge/v1/catalog.proto`, add to `service ServiceCatalogService` after `DeleteService`:

```proto
  // CreateService scaffolds a service from a template: it renders the
  // template, creates the GitHub repo, pushes the first commit and registers
  // the service, streaming progress. Errors before any side effect end the
  // stream with an error code; a failure after the repo exists ends it with
  // a CreateFailure.
  rpc CreateService(CreateServiceRequest) returns (stream CreateServiceResponse) {}
```

and at the end of the file:

```proto
message CreateServiceRequest {
  // Becomes services/{service_id} and the repo {github org}/{service_id}.
  string service_id = 1;
  // "templates/{template}".
  string template = 2;
  // Release tag; empty means the newest.
  string template_version = 3;
  // Template parameters as strings; the server converts them by type.
  map<string, string> parameters = 4;
  string owner_team = 5;
  repeated string owners = 6;
  // Idempotency key: a retry with the same key replays the first outcome.
  string request_id = 7;
  // Render and check; create nothing.
  bool validate_only = 8;
}

// CreateServiceResponse is one message of the CreateService stream.
message CreateServiceResponse {
  oneof event {
    CreateStep step = 1;
    // Success: the registered service (last message).
    Service service = 2;
    // A failure after side effects began (last message).
    CreateFailure failure = 3;
    // validate_only: what would be created (last message).
    CreatePlan plan = 4;
  }
}

// CreateStep reports progress of one step: fetch, render, create_repo, push, register.
message CreateStep {
  // State is where the step is.
  enum State {
    STATE_UNSPECIFIED = 0;
    STATE_STARTED = 1;
    STATE_DONE = 2;
  }
  string name = 1;
  State state = 2;
  // For example "go-grpc@v1.4.0", "23 files" or a commit SHA.
  string detail = 3;
}

// CreateFailure ends a create that failed after the repo was created.
message CreateFailure {
  string step = 1;
  string message = 2;
  // True when the repo was deleted again.
  bool rolled_back = 3;
}

// CreatePlan is what a validate_only create would do.
message CreatePlan {
  string template_version = 1;
  string repo_url = 2;
  repeated string files = 3;
}
```

Run: `make generate && buf lint && buf breaking --against '.git#branch=main' && echo ok`
Expected: `ok`. The build now fails (`*Catalog` does not implement `CreateService`); Step 4 fixes it.

- [ ] **Step 2: Write the failing tests** — `internal/server/create_test.go`:

```go
package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
)

// fakeRepos is an in-memory GitHub org.
type fakeRepos struct {
	mu        sync.Mutex
	repos     map[string][]github.File // name -> pushed files (nil until pushed)
	creates   int
	createErr error
	pushErr   error
	deleteErr error
	onCreate  func() // runs inside CreateOrgRepo, for disconnect tests
	pushGate  chan struct{}
}

func newFakeRepos(existing ...string) *fakeRepos {
	f := &fakeRepos{repos: map[string][]github.File{}}
	for _, n := range existing {
		f.repos[n] = nil
	}
	return f
}

func (f *fakeRepos) RepoExists(_ context.Context, owner, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[name]
	return ok && owner == "showmino", nil
}

func (f *fakeRepos) CreateOrgRepo(_ context.Context, org, name, _ string) (*github.Repo, error) {
	if f.onCreate != nil {
		f.onCreate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, ok := f.repos[name]; ok {
		return nil, github.ErrRepoExists
	}
	f.creates++
	f.repos[name] = nil
	return &github.Repo{FullName: org + "/" + name, DefaultBranch: "main"}, nil
}

func (f *fakeRepos) DeleteRepo(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.repos, name)
	return nil
}

func (f *fakeRepos) PushFiles(_ context.Context, _, repo, _, _ string, files []github.File) (string, error) {
	if f.pushGate != nil {
		<-f.pushGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return "", f.pushErr
	}
	f.repos[repo] = files
	return "abc1234def", nil
}

func (f *fakeRepos) files(name string) ([]github.File, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.repos[name]
	return files, ok
}

// createRelease has template "svc" (a required enum, an int default, an
// executable file) and "dep" (its forge.yaml depends on a missing service).
var createRelease = staticSource{
	versions: []string{"v1.0.0"},
	trees: map[string]fs.FS{"v1.0.0": fstest.MapFS{
		"svc/template.yaml": {Data: []byte(templateYAML("svc", "go", "parameters:\n  - name: port\n    type: int\n    default: 8080\n  - name: tier\n    type: enum\n    values: [gold, silver]\n"))},
		"svc/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n" +
			"{{- if .Service.Owners }}\n  owners:\n{{- range .Service.Owners }}\n    - {{ . }}\n{{- end }}\n{{- end }}\n  lifecycle: experimental\n  repo: {{ .Repo.URL }}\n")},
		"svc/skeleton/main.go":  {Data: []byte("package main // {{ .Params.port }} {{ .Params.tier }}\n")},
		"svc/skeleton/run.sh":   {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"dep/template.yaml":     {Data: []byte(templateYAML("dep", "go", ""))},
		"dep/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n  dependencies: [missing]\n")},
	}},
}

type createEnv struct {
	catalog *Catalog
	repos   *fakeRepos
	client  forgev1connect.ServiceCatalogServiceClient
}

func newCreateEnv(t *testing.T, src TemplateSource, repos *fakeRepos) *createEnv {
	t.Helper()
	e := &createEnv{catalog: NewCatalog(nil), repos: repos}
	opts := Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Catalog:    e.catalog,
		Templates:  src,
	}
	if repos != nil {
		opts.Repos = repos
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.client = forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	return e
}

// create runs CreateService as token and collects every message.
func (e *createEnv) create(ctx context.Context, token string, req *forgev1.CreateServiceRequest) ([]*forgev1.CreateServiceResponse, error) {
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", "Bearer "+token)
	stream, err := e.client.CreateService(ctx, r)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var msgs []*forgev1.CreateServiceResponse
	for stream.Receive() {
		msgs = append(msgs, stream.Msg())
	}
	return msgs, stream.Err()
}

func validRequest() *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{
		ServiceId:  "payments",
		Template:   "templates/svc",
		Parameters: map[string]string{"tier": "gold"},
		OwnerTeam:  "billing",
		Owners:     []string{"ana@example.com"},
		RequestId:  "req-1",
	}
}

func TestCreatePreflight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	with := func(mut func(*forgev1.CreateServiceRequest)) *forgev1.CreateServiceRequest {
		r := validRequest()
		mut(r)
		return r
	}
	tests := []struct {
		name  string
		env   *createEnv
		token string
		req   *forgev1.CreateServiceRequest
		want  connect.Code
		msg   string
	}{
		{"anonymous", newCreateEnv(t, createRelease, newFakeRepos()), "nope", validRequest(), connect.CodeUnauthenticated, ""},
		{"reader", newCreateEnv(t, createRelease, newFakeRepos()), "reader", validRequest(), connect.CodePermissionDenied, "services.write"},
		{"bad id", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.ServiceId = "Pay_ments" }), connect.CodeInvalidArgument, "service_id"},
		{"bad template", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.Template = "templates/Bad" }), connect.CodeInvalidArgument, "template"},
		{"no team", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.OwnerTeam = " " }), connect.CodeInvalidArgument, "owner_team"},
		{"empty owner", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.Owners = []string{""} }), connect.CodeInvalidArgument, "owners"},
		{"no templates", newCreateEnv(t, nil, newFakeRepos()), "writer", validRequest(), connect.CodeFailedPrecondition, "templates are not configured"},
		{"no GitHub", newCreateEnv(t, createRelease, nil), "writer", validRequest(), connect.CodeFailedPrecondition, "no GitHub App"},
		{"repo taken on GitHub", newCreateEnv(t, createRelease, newFakeRepos("payments")), "writer", validRequest(), connect.CodeAlreadyExists, "github.com/showmino/payments already exists"},
	}
	for _, tt := range tests {
		_, err := tt.env.create(ctx, tt.token, tt.req)
		if connect.CodeOf(err) != tt.want || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: got %v, want %v containing %q", tt.name, err, tt.want, tt.msg)
		}
	}

	// A name taken in the catalog is refused before GitHub is touched.
	e := newCreateEnv(t, createRelease, newFakeRepos())
	register(t, e.catalog, "payments")
	if _, err := e.create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("catalog conflict: %v", err)
	}
	if e.repos.creates != 0 {
		t.Error("a conflict must not create a repo")
	}
}

func TestCreateWithoutGitHubCanStillValidate(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, nil)
	r := validRequest()
	r.ValidateOnly = true
	msgs, err := e.create(context.Background(), "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; last.GetPlan() == nil {
		t.Errorf("validate_only without GitHub must still plan: %v", last)
	}
}

var errBoom = errors.New("boom")
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/server -run 'CreatePreflight|WithoutGitHub'`
Expected: FAIL to compile (the generated handler interface needs `CreateService`; `Options` has no `Repos`).

- [ ] **Step 4: Implement**

In `internal/server/templates.go`, replace `func (s *templateService) release(...)` with these three functions and use them in `GetTemplate`:

```go
// release resolves version (empty = newest) and fetches its tree, for any
// authenticated caller.
func (s *templateService) release(ctx context.Context, version string) (string, fs.FS, error) {
	if err := authenticated(ctx); err != nil {
		return "", nil, err
	}
	if s.src == nil {
		return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	return resolveRelease(ctx, s.src, version)
}

// resolveRelease resolves version (empty = newest) and fetches its tree.
func resolveRelease(ctx context.Context, src TemplateSource, version string) (string, fs.FS, error) {
	if version == "" {
		vs, err := src.Versions(ctx)
		if err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list template versions: %w", err))
		}
		if len(vs) == 0 {
			return "", nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the template repo has no release tags yet"))
		}
		version = vs[0]
	}
	tree, err := src.Fetch(ctx, version)
	switch {
	case errors.Is(err, ErrVersionNotFound):
		return "", nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("template version %s not found", version))
	case errors.Is(err, ErrInvalidRelease):
		return "", nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", nil, connect.NewError(connect.CodeDeadlineExceeded, err)
	case err != nil:
		return "", nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("template source unavailable: %w", err))
	}
	return version, tree, nil
}

// templateFromRelease loads template name from a release tree, as a Connect error.
func templateFromRelease(tree fs.FS, name, version string) (*template.Template, error) {
	t, err := loadTemplate(tree, name)
	if errors.Is(err, errTemplateNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found in %s", templatePrefix, name, version))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s in %s is invalid: %w", templatePrefix, name, version, err))
	}
	return t, nil
}
```

In `GetTemplate`, replace the `loadTemplate` call and its two `if` blocks with:

```go
	t, err := templateFromRelease(tree, name, version)
	if err != nil {
		return nil, err
	}
```

In `internal/server/catalog.go`, delete the line `var _ forgev1connect.ServiceCatalogServiceHandler = (*Catalog)(nil)` (and the `forgev1connect` import if it becomes unused), and add after `NewCatalog`:

```go
// has reports whether a service is registered under name.
func (c *Catalog) has(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.services[name]
	return ok
}
```

Create `internal/server/create.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

// RepoHost creates, fills and deletes the repos of new services: GitHub, or
// a fake in tests.
type RepoHost interface {
	RepoExists(ctx context.Context, owner, name string) (bool, error)
	CreateOrgRepo(ctx context.Context, org, name, description string) (*github.Repo, error)
	DeleteRepo(ctx context.Context, owner, name string) error
	PushFiles(ctx context.Context, owner, repo, branch, message string, files []github.File) (string, error)
}

var _ RepoHost = (*github.Client)(nil)

// catalogService adds CreateService to the Catalog's handlers.
type catalogService struct {
	*Catalog
	create *creator
}

var _ forgev1connect.ServiceCatalogServiceHandler = (*catalogService)(nil)

func (s *catalogService) CreateService(ctx context.Context, req *connect.Request[forgev1.CreateServiceRequest], stream *connect.ServerStream[forgev1.CreateServiceResponse]) error {
	return s.create.run(ctx, req.Msg, stream)
}

// createStream is the part of the server stream the pipeline uses.
type createStream interface {
	Send(*forgev1.CreateServiceResponse) error
}

// maxParams caps the number of parameters in one request.
const maxParams = 100

// creator runs CreateService: render a template, create the repo, push the
// first commit and register the service.
type creator struct {
	catalog   *Catalog
	templates TemplateSource
	repos     RepoHost // nil: repos cannot be created; validate_only still works
	org       string
	log       *slog.Logger

	mu sync.Mutex
	// results holds the last message of each create by request_id; nil while it runs.
	results map[string]*forgev1.CreateServiceResponse
}

func validateCreate(req *forgev1.CreateServiceRequest) error {
	if !manifest.ValidID(req.GetServiceId()) {
		return invalid(fmt.Sprintf("service_id %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", req.GetServiceId()))
	}
	if !manifest.ValidID(strings.TrimPrefix(req.GetTemplate(), templatePrefix)) {
		return invalid(fmt.Sprintf("template %q must look like templates/{template}", req.GetTemplate()))
	}
	if strings.TrimSpace(req.GetOwnerTeam()) == "" {
		return invalid("owner_team is required")
	}
	for _, o := range req.GetOwners() {
		if strings.TrimSpace(o) == "" {
			return invalid("owners must not be empty strings")
		}
	}
	if len(req.GetParameters()) > maxParams {
		return invalid(fmt.Sprintf("at most %d parameters", maxParams))
	}
	for k, v := range req.GetParameters() {
		if len(v) > template.MaxParamLen {
			return invalid(fmt.Sprintf("parameter %q is longer than %d bytes", k, template.MaxParamLen))
		}
	}
	return nil
}

// run checks the caller and the request, applies request_id idempotency and
// runs the pipeline.
func (c *creator) run(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) error {
	if err := require(ctx, PermServicesWrite); err != nil {
		return err
	}
	if err := validateCreate(req); err != nil {
		return err
	}
	if c.templates == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	if c.repos == nil && !req.GetValidateOnly() {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("creating repos is not configured on this Forge API (no GitHub App)"))
	}
	key := req.GetRequestId()
	if key != "" && !req.GetValidateOnly() {
		c.mu.Lock()
		final, seen := c.results[key]
		if !seen {
			c.results[key] = nil
		}
		c.mu.Unlock()
		if seen {
			if final == nil {
				return connect.NewError(connect.CodeAborted, errors.New("a create with this request_id is still running"))
			}
			return stream.Send(final)
		}
	}
	final, err := c.create(ctx, req, stream)
	if key != "" && !req.GetValidateOnly() {
		c.mu.Lock()
		if final != nil {
			c.results[key] = final
		} else {
			delete(c.results, key) // nothing happened yet: a retry starts over
		}
		c.mu.Unlock()
	}
	return err
}

// create runs the pipeline and returns its last message (nil when it ended
// with an error before any side effect). Task 4 fills in the steps after the
// checks.
func (c *creator) create(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	id := req.GetServiceId()
	if c.catalog.has(servicePrefix + id) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", servicePrefix, id))
	}
	if c.repos != nil {
		exists, err := c.repos.RepoExists(ctx, c.org, id)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("check github.com/%s/%s: %w", c.org, id, err))
		}
		if exists {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s already exists", c.org, id))
		}
	}
	return c.scaffold(ctx, req, stream)
}
```

Add this temporary `scaffold` at the end of `create.go` (Task 4 replaces it):

```go
func (c *creator) scaffold(_ context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	if !req.GetValidateOnly() {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("create is not implemented yet"))
	}
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{}}}
	return final, stream.Send(final)
}
```

In `internal/server/server.go`, add to `Options` (after `Templates`):

```go
	// Repos creates the repos of new services; nil answers FAILED_PRECONDITION
	// to CreateService unless validate_only.
	Repos RepoHost
	// GitHubOrg is the org new repos are created in (default "showmino").
	GitHubOrg string
```

In `New`, after the `o.Catalog == nil` block add:

```go
	if o.GitHubOrg == "" {
		o.GitHubOrg = "showmino"
	}
```

and replace `mux.Handle(forgev1connect.NewServiceCatalogServiceHandler(o.Catalog, opts))` with:

```go
	create := &creator{
		catalog: o.Catalog, templates: o.Templates, repos: o.Repos, org: o.GitHubOrg, log: o.Logger,
		results: map[string]*forgev1.CreateServiceResponse{},
	}
	mux.Handle(forgev1connect.NewServiceCatalogServiceHandler(&catalogService{Catalog: o.Catalog, create: create}, opts))
```

`Options.Repos` is an interface: `newCreateEnv` only sets it when `repos != nil`, so a nil `*fakeRepos` never becomes a non-nil interface. Callers in `cmd/forge-api` (Task 5) must do the same.

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go test -count=1 -race ./internal/server`
Expected: PASS (all milestone 2 tests too).

- [ ] **Step 6: Gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint`
Expected: all ok, `0 issues.`, `buf lint` silent.

```bash
git add proto gen internal/server && git commit -m "api: CreateService contract, wiring and pre-flight checks

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The create pipeline

**Files:**
- Modify: `internal/server/create.go` (replace the temporary `scaffold`)
- Test: `internal/server/create_test.go` (append)

**Interfaces:**
- Consumes: `resolveRelease`, `templateFromRelease` (Task 3); `template.Values`, `Template.ResolveParams`, `Template.Render`, `template.CheckManifest`, `template.ParamError`, `template.SkeletonDir` (milestone 1); `manifest.Parse`; `Catalog.RegisterService`; `github.File`, `github.ErrRepoExists`.
- Produces: `func (c *creator) scaffold(ctx, req, stream) (*forgev1.CreateServiceResponse, error)` and `func serviceFromManifest(m *manifest.Manifest) *forgev1.Service`.

- [ ] **Step 1: Write the failing tests** — append to `internal/server/create_test.go`:

```go
// steps returns "name:state" for each step message and the last message.
func steps(msgs []*forgev1.CreateServiceResponse) ([]string, *forgev1.CreateServiceResponse) {
	var out []string
	for _, m := range msgs {
		if s := m.GetStep(); s != nil {
			out = append(out, s.GetName()+":"+strings.TrimPrefix(s.GetState().String(), "STATE_")+":"+s.GetDetail())
		}
	}
	if len(msgs) == 0 {
		return out, nil
	}
	return out, msgs[len(msgs)-1]
}

func TestCreateService(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	msgs, err := e.create(context.Background(), "writer", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	want := []string{
		"fetch:STARTED:", "fetch:DONE:svc@v1.0.0",
		"render:STARTED:", "render:DONE:3 files",
		"create_repo:STARTED:", "create_repo:DONE:github.com/showmino/payments",
		"push:STARTED:", "push:DONE:abc1234 (CI started)",
		"register:STARTED:", "register:DONE:services/payments",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	svc := last.GetService()
	if svc.GetName() != "services/payments" || svc.GetOwnerTeam() != "billing" || svc.GetRepoUrl() != "https://github.com/showmino/payments" ||
		svc.GetLifecycle() != forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL || strings.Join(svc.GetOwners(), ",") != "ana@example.com" {
		t.Errorf("service: %v", svc)
	}
	files, _ := e.repos.files("payments")
	byPath := map[string]github.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if string(byPath["main.go"].Content) != "package main // 8080 gold\n" || !byPath["run.sh"].Executable || byPath["main.go"].Executable {
		t.Errorf("pushed files: %+v", files)
	}
	if !e.catalog.has("services/payments") {
		t.Error("service not registered")
	}
}

func TestCreateValidateOnly(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	r := validRequest()
	r.ValidateOnly = true
	msgs, err := e.create(context.Background(), "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	if strings.Join(got, ",") != "fetch:STARTED:,fetch:DONE:svc@v1.0.0,render:STARTED:,render:DONE:3 files" {
		t.Errorf("steps: %v", got)
	}
	p := last.GetPlan()
	if p.GetTemplateVersion() != "v1.0.0" || p.GetRepoUrl() != "https://github.com/showmino/payments" || strings.Join(p.GetFiles(), ",") != "forge.yaml,main.go,run.sh" {
		t.Errorf("plan: %v", p)
	}
	if _, ok := e.repos.files("payments"); ok || e.catalog.has("services/payments") {
		t.Error("validate_only must not create anything")
	}
}

func TestCreateBadParameters(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	for name, params := range map[string]map[string]string{
		"missing required": {},
		"wrong type":       {"tier": "gold", "port": "http"},
		"unknown":          {"tier": "gold", "nope": "x"},
	} {
		r := validRequest()
		r.Parameters = params
		_, err := e.create(context.Background(), "writer", r)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if e.repos.creates != 0 {
		t.Error("bad parameters must not create a repo")
	}
}

func TestCreateFailuresRollBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pushFails := newFakeRepos()
	pushFails.pushErr = errBoom
	e := newCreateEnv(t, createRelease, pushFails)
	msgs, err := e.create(ctx, "writer", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, last := steps(msgs)
	if f := last.GetFailure(); f.GetStep() != "push" || !f.GetRolledBack() || !strings.Contains(f.GetMessage(), "boom") {
		t.Errorf("push failure: %v", last)
	}
	if _, ok := pushFails.files("payments"); ok {
		t.Error("the repo must be deleted again")
	}

	cannotDelete := newFakeRepos()
	cannotDelete.pushErr, cannotDelete.deleteErr = errBoom, errBoom
	msgs, _ = newCreateEnv(t, createRelease, cannotDelete).create(ctx, "writer", validRequest())
	if _, last := steps(msgs); last.GetFailure().GetRolledBack() {
		t.Error("rolled_back must be false when the delete failed")
	}

	registerFails := newFakeRepos()
	e = newCreateEnv(t, createRelease, registerFails)
	r := validRequest()
	r.Template, r.Parameters = "templates/dep", nil
	msgs, err = e.create(ctx, "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	if _, last := steps(msgs); last.GetFailure().GetStep() != "register" || !last.GetFailure().GetRolledBack() {
		t.Errorf("register failure: %v", last)
	}

	raced := newFakeRepos()
	raced.createErr = github.ErrRepoExists
	if _, err := newCreateEnv(t, createRelease, raced).create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("repo created by someone else meanwhile: %v", err)
	}
	down := newFakeRepos()
	down.createErr = errBoom
	if _, err := newCreateEnv(t, createRelease, down).create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("GitHub down at create_repo: %v", err)
	}
}

func TestCreateIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	e := newCreateEnv(t, createRelease, repos)
	if _, err := e.create(ctx, "writer", validRequest()); err != nil {
		t.Fatal(err)
	}
	msgs, err := e.create(ctx, "writer", validRequest()) // same request_id
	if err != nil || len(msgs) != 1 || msgs[0].GetService().GetName() != "services/payments" {
		t.Errorf("replay: %v %v", msgs, err)
	}
	if repos.creates != 1 {
		t.Errorf("%d repos created, want 1", repos.creates)
	}

	// A retry while the first attempt still runs is refused.
	slow := newFakeRepos()
	slow.pushGate = make(chan struct{})
	e = newCreateEnv(t, createRelease, slow)
	done := make(chan error, 1)
	go func() {
		_, err := e.create(ctx, "writer", validRequest())
		done <- err
	}()
	for {
		slow.mu.Lock()
		n := slow.creates
		slow.mu.Unlock()
		if n == 1 {
			break
		}
	}
	if _, err := e.create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("retry while running: %v", err)
	}
	close(slow.pushGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A client that disconnects after create_repo does not leave a half-made service.
func TestCreateSurvivesDisconnect(t *testing.T) {
	t.Parallel()
	repos := newFakeRepos()
	e := newCreateEnv(t, createRelease, repos)
	ctx, cancel := context.WithCancel(context.Background())
	repos.onCreate = cancel // the client goes away while the repo is being created
	_, _ = e.create(ctx, "writer", validRequest())
	// The server finishes in the background; wait for the registration.
	for i := 0; !e.catalog.has("services/payments"); i++ {
		if i > 200 {
			t.Fatal("the create did not finish after the client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs, err := e.create(context.Background(), "writer", validRequest()) // the CLI's retry
	if err != nil || len(msgs) != 1 || msgs[0].GetService() == nil {
		t.Errorf("retry after disconnect: %v %v", msgs, err)
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/server -run 'TestCreate'`
Expected: `TestCreateService` and the others FAIL with `unimplemented: create is not implemented yet`; `TestCreatePreflight` still passes.

- [ ] **Step 3: Implement** — in `internal/server/create.go` add `"io/fs"`, `"path"` to the imports and replace the temporary `scaffold` with:

```go
// sender sends stream messages and remembers the first send error: once
// side effects began, the pipeline continues even if the client is gone.
type sender struct {
	s   createStream
	err error
}

func (w *sender) send(m *forgev1.CreateServiceResponse) {
	if w.err == nil {
		w.err = w.s.Send(m)
	}
}

func stepMsg(name string, state forgev1.CreateStep_State, detail string) *forgev1.CreateServiceResponse {
	return &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Step{Step: &forgev1.CreateStep{Name: name, State: state, Detail: detail}}}
}

// scaffold runs fetch, render and (unless validate_only) create_repo, push
// and register.
func (c *creator) scaffold(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	w := &sender{s: stream}
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	tname := strings.TrimPrefix(req.GetTemplate(), templatePrefix)
	repoURL := "https://github.com/" + c.org + "/" + id

	started("fetch")
	version, tree, err := resolveRelease(ctx, c.templates, req.GetTemplateVersion())
	if err != nil {
		return nil, err
	}
	t, err := templateFromRelease(tree, tname, version)
	if err != nil {
		return nil, err
	}
	skeleton, err := fs.Sub(tree, path.Join(tname, template.SkeletonDir))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	done("fetch", tname+"@"+version)

	started("render")
	v := template.Values{
		Service:  template.ServiceValues{Name: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners()},
		Repo:     template.RepoValues{Owner: c.org, Name: id, URL: repoURL},
		Template: template.TemplateValues{Name: tname, Version: version},
	}
	params, err := t.ResolveParams(v, req.GetParameters())
	var pe *template.ParamError
	if errors.As(err, &pe) {
		return nil, invalid(pe.Error())
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	v.Params = params
	files, err := t.Render(skeleton, v)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s does not render: %w", tname, version, err))
	}
	if err := template.CheckManifest(files, id, req.GetOwnerTeam()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s@%s: %w", tname, version, err))
	}
	var m *manifest.Manifest
	for _, f := range files {
		if f.Path == manifest.FileName {
			m, _ = manifest.Parse(f.Data) // CheckManifest parsed it already
		}
	}
	done("render", fmt.Sprintf("%d files", len(files)))

	if req.GetValidateOnly() {
		plan := &forgev1.CreatePlan{TemplateVersion: version, RepoUrl: repoURL}
		for _, f := range files {
			plan.Files = append(plan.Files, f.Path)
		}
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: plan}}
		w.send(final)
		return final, w.err
	}

	// From here on the work outlives the client: the outcome is stored for
	// its retry. The HTTP client's timeout bounds every GitHub call.
	work := context.WithoutCancel(ctx)
	started("create_repo")
	repo, err := c.repos.CreateOrgRepo(work, c.org, id, fmt.Sprintf("%s, created by forge create from %s@%s", t.Title, tname, version))
	if errors.Is(err, github.ErrRepoExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s already exists", c.org, id))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("create github.com/%s/%s: %w", c.org, id, err))
	}
	done("create_repo", "github.com/"+c.org+"/"+id)

	fail := func(step string, cause error) (*forgev1.CreateServiceResponse, error) {
		delErr := c.repos.DeleteRepo(work, c.org, id)
		c.log.WarnContext(ctx, "create failed", "service", id, "step", step, "error", cause, "rolled_back", delErr == nil, "rollback_error", delErr)
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Failure{Failure: &forgev1.CreateFailure{
			Step: step, Message: cause.Error(), RolledBack: delErr == nil,
		}}}
		w.send(final)
		return final, nil
	}

	started("push")
	pushFiles := make([]github.File, 0, len(files))
	for _, f := range files {
		pushFiles = append(pushFiles, github.File{Path: f.Path, Executable: f.Mode&0o111 != 0, Content: f.Data})
	}
	sha, err := c.repos.PushFiles(work, c.org, id, repo.DefaultBranch, fmt.Sprintf("Scaffold %s from %s@%s\n\nCreated with forge create.", id, tname, version), pushFiles)
	if err != nil {
		return fail("push", err)
	}
	done("push", shortSHA(sha)+" (CI started)")

	started("register")
	svc := serviceFromManifest(m)
	svc.RepoUrl = repoURL
	resp, err := c.catalog.RegisterService(work, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}))
	if err != nil {
		return fail("register", err)
	}
	done("register", servicePrefix+id)

	p, _ := PrincipalFrom(ctx)
	c.log.InfoContext(ctx, "service created", "service", id, "template", tname, "version", version, "repo", repoURL, "by", p.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Service{Service: resp.Msg.GetService()}}
	w.send(final)
	return final, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// serviceFromManifest is the catalog entry a rendered forge.yaml describes.
func serviceFromManifest(m *manifest.Manifest) *forgev1.Service {
	s := &forgev1.Service{
		DisplayName: m.Service.DisplayName,
		OwnerTeam:   m.Service.Team,
		Owners:      m.Service.Owners,
		RepoUrl:     m.Service.Repo,
		Labels:      m.Service.Labels,
	}
	switch m.Service.Lifecycle {
	case "experimental":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL
	case "production":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_PRODUCTION
	case "deprecated":
		s.Lifecycle = forgev1.Lifecycle_LIFECYCLE_DEPRECATED
	}
	for _, d := range m.Service.Dependencies {
		s.Dependencies = append(s.Dependencies, servicePrefix+strings.TrimPrefix(d, servicePrefix))
	}
	return s
}
```

`ctx` still carries the principal after `context.WithoutCancel`, so `RegisterService`'s permission check sees the caller.

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -race ./internal/server -run 'TestCreate' -v 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: every `TestCreate*` PASS.

- [ ] **Step 5: Gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint`
Expected: all ok, `0 issues.`

```bash
git add internal/server && git commit -m "api: the create pipeline with rollback and idempotency

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: forge-api creates repos as the App

**Files:**
- Modify: `cmd/forge-api/templates.go`, `cmd/forge-api/templates_test.go`, `cmd/forge-api/main.go`

**Interfaces:**
- Consumes: `githubApp` (milestone 2), `server.RepoHost`, `server.Options.Repos`, `server.Options.GitHubOrg` (Task 3).
- Produces: `func repoHost(app *github.App) server.RepoHost` — nil (a nil interface) without an App.

- [ ] **Step 1: Write the failing test** — append to `cmd/forge-api/templates_test.go`:

```go
func TestRepoHost(t *testing.T) {
	t.Parallel()
	if h := repoHost(nil); h != nil {
		t.Errorf("no App: want a nil RepoHost, got %#v", h)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := repoHost(&github.App{ID: 1, Key: key, Org: "showmino"}).(*github.Client)
	if !ok || c.Token == nil {
		t.Errorf("App: want a *github.Client with the App's token, got %#v", c)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/forge-api -run RepoHost`
Expected: FAIL to compile (`undefined: repoHost`).

- [ ] **Step 3: Implement** — append to `cmd/forge-api/templates.go`:

```go
// repoHost returns where new services' repos are created: GitHub as the App,
// or nil (an untyped nil interface) when no App is configured.
func repoHost(app *github.App) server.RepoHost {
	if app == nil {
		return nil
	}
	return &github.Client{Token: app.Token}
}
```

In `cmd/forge-api/main.go`, add to the `server.Options{...}` literal:

```go
		Repos:            repoHost(app),
		GitHubOrg:        *githubOrg,
```

and change the "templates are not configured" log block to also log when creating repos is off:

```go
	if app == nil {
		log.Info("creating services is off; set --github-app-id and --github-app-key-file")
	}
```

(placed right after the `templates == nil` block). Run `gofmt -w cmd/forge-api`.

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 ./cmd/forge-api && go build -o bin/ ./cmd/...`
Expected: PASS and a clean build.

- [ ] **Step 5: Commit**

```bash
git add cmd/forge-api && git commit -m "forge-api: create repos as the GitHub App

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: `forge create`

**Files:**
- Create: `internal/cli/create.go`, `internal/cli/create_test.go`
- Modify: `internal/cli/factory.go` (`stdin` reader), `internal/cli/prompt.go` (`ask`), `internal/cli/root.go` (register), `internal/cli/cli_test.go` (harness `repos`)
- Modify: `README.md`, `.claude/skills/forge/SKILL.md`

**Interfaces:**
- Consumes: `client.Client.Catalog.CreateService`, `client.Client.Templates.GetTemplate`, `client.NewRequestID`; `template.Parameter`, `Parameter.Convert`; `paramType` (milestone 2 CLI); `Factory.printMutation`; `server.RepoHost`, `github.Repo`, `github.File`.
- Produces: `forge create <name> --template <t>[@version] --team <team> [--owner …] [--set k=v …] [--dry-run]`.

- [ ] **Step 1: Let the harness create repos** — in `internal/cli/cli_test.go` add the field `repos server.RepoHost` to `harness`, and in `startAPI` add `Repos: h.repos,` to `server.Options`. (When `h.repos` is unset it is a nil interface, so the API reports creating as not configured.)

- [ ] **Step 2: Write the failing tests** — `internal/cli/create_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/showmino/forge/internal/github"
)

// memRepos is an in-memory GitHub org for CLI tests.
type memRepos struct {
	mu        sync.Mutex
	repos     map[string]int // name -> files pushed
	pushErr   error
	deleteErr error
}

func (m *memRepos) RepoExists(_ context.Context, _, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.repos[name]
	return ok, nil
}

func (m *memRepos) CreateOrgRepo(_ context.Context, org, name, _ string) (*github.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos[name] = 0
	return &github.Repo{FullName: org + "/" + name, DefaultBranch: "main"}, nil
}

func (m *memRepos) DeleteRepo(_ context.Context, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.repos, name)
	return nil
}

func (m *memRepos) PushFiles(_ context.Context, _, repo, _, _ string, files []github.File) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pushErr != nil {
		return "", m.pushErr
	}
	m.repos[repo] = len(files)
	return "abc1234def", nil
}

// createHarness serves templateRepo (milestone 2's fixture: "svc" needs
// --set tier=gold|silver) and an in-memory GitHub org.
func createHarness(t *testing.T) (*harness, *memRepos) {
	t.Helper()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	repos := &memRepos{repos: map[string]int{}}
	h.repos = repos
	h.serveTemplates(filepath.Join(h.dir, "repo"))
	return h, repos
}

func TestCreate(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	r := h.ok("create", "payments", "--template", "svc", "--team", "platform", "--owner", "ana@example.com", "--set", "tier=gold")
	if r.out != "services/payments created\n" {
		t.Errorf("stdout: %q", r.out)
	}
	for _, want := range []string{"✓ fetched template svc@dev", "✓ rendered 1 files", "✓ created github.com/showmino/payments", "✓ pushed initial commit abc1234 (CI started)", "✓ registered services/payments"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
	if !strings.Contains(h.ok("service", "get", "payments", "-o", "json").out, `"repoUrl": "https://github.com/showmino/payments"`) {
		t.Error("the catalog entry has no repo URL")
	}
	repos.mu.Lock()
	pushed := repos.repos["payments"]
	repos.mu.Unlock()
	if pushed != 1 {
		t.Errorf("files pushed: %d, want 1", pushed)
	}
	h.fails(6, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold") // exists now

	r = h.ok("create", "ledger", "--template", "svc", "--team", "platform", "--set", "tier=silver", "-o", "json", "-q")
	if !strings.Contains(r.out, `"name": "services/ledger"`) || r.err != "" {
		t.Errorf("-o json -q: out %q err %q", r.out, r.err)
	}
}

func TestCreateDryRun(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	r := h.ok("create", "web-app", "--template", "web", "--team", "platform", "--dry-run")
	if !strings.Contains(r.out, "Would create https://github.com/showmino/web-app from web@dev with 1 file:") || !strings.Contains(r.out, "  forge.yaml") {
		t.Errorf("dry run output:\n%s", r.out)
	}
	if len(repos.repos) != 0 {
		t.Error("--dry-run must not create a repo")
	}
	h.fails(3, "service", "get", "web-app")
}

func TestCreateUsage(t *testing.T) {
	t.Parallel()
	h, _ := createHarness(t)
	r := h.fails(2, "create", "payments", "--template", "svc", "--team", "platform") // tier is required, no terminal
	if !strings.Contains(r.err, `parameter "tier" is required`) || !strings.Contains(r.err, "--set tier=") {
		t.Errorf("missing parameter:\n%s", r.err)
	}
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=bronze")
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--set", "nope=1")
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier")
	h.fails(2, "create", "payments", "--team", "platform")
	h.fails(2, "create", "payments", "--template", "svc", "--set", "tier=gold") // --team, no terminal
	h.fails(2, "create", "Pay_ments", "--template", "svc", "--team", "platform")
	h.fails(2, "create")
	h.fails(3, "create", "payments", "--template", "nope", "--team", "platform")
}

func TestCreatePrompts(t *testing.T) {
	t.Parallel()
	h, _ := createHarness(t)
	h.tty = true
	h.stdin = "platform\ngold\n"
	r := h.ok("create", "payments", "--template", "svc")
	if !strings.Contains(r.err, "Owning team: ") || !strings.Contains(r.err, "tier (enum (gold, silver)): ") {
		t.Errorf("prompts:\n%s", r.err)
	}
	if !strings.Contains(h.ok("service", "get", "payments", "-o", "json").out, `"ownerTeam": "platform"`) {
		t.Error("the prompted team was not used")
	}
}

func TestCreateFailures(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	repos.pushErr = errors.New("boom")
	r := h.fails(7, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold")
	if !strings.Contains(r.err, "push failed: boom") || !strings.Contains(r.err, "deleted again") {
		t.Errorf("rolled back:\n%s", r.err)
	}
	repos.deleteErr = errors.New("denied")
	r = h.fails(7, "create", "ledger", "--template", "svc", "--team", "platform", "--set", "tier=gold")
	if !strings.Contains(r.err, "could not be deleted") {
		t.Errorf("not rolled back:\n%s", r.err)
	}

	noGitHub := newHarness(t)
	writeTree(t, filepath.Join(noGitHub.dir, "repo"), templateRepo)
	noGitHub.serveTemplates(filepath.Join(noGitHub.dir, "repo"))
	r = noGitHub.fails(6, "create", "payments", "--template", "web", "--team", "platform")
	if !strings.Contains(r.err, "not configured") {
		t.Errorf("no GitHub App:\n%s", r.err)
	}
	noGitHub.ok("create", "payments", "--template", "web", "--team", "platform", "--dry-run")
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/cli -run 'TestCreate'`
Expected: FAIL: `unknown command "create" for "forge"`.

- [ ] **Step 4: Implement**

In `internal/cli/factory.go`, add the field `stdin *bufio.Reader // created on first use, shared by every prompt` to `Factory` (after `flags`), add `"bufio"` to the imports, and add:

```go
// readLine reads one line from stdin. One buffered reader serves every
// prompt, so consecutive prompts do not lose each other's input.
func (f *Factory) readLine() (string, error) {
	if f.stdin == nil {
		f.stdin = bufio.NewReader(f.IO.In)
	}
	line, err := f.stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
```

(add `"strings"` to the imports if missing). In `internal/cli/prompt.go` add:

```go
// ask prints label on stderr and reads the answer from stdin.
func (f *Factory) ask(label string) (string, error) {
	fmt.Fprintf(f.IO.ErrOut, "%s: ", label)
	answer, err := f.readLine()
	if err != nil {
		return "", exitcode.Usagef("no answer for %s", label)
	}
	return answer, nil
}
```

In `internal/cli/root.go`, change the core loop to `[]*cobra.Command{newServiceCmd(f), newTemplateCmd(f), newCreateCmd(f)}`.

Create `internal/cli/create.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/template"
	"github.com/showmino/forge/pkg/client"
)

func newCreateCmd(f *Factory) *cobra.Command {
	var tmpl, team string
	var owners, sets []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "create <name> --template <template>[@version] --team <team>",
		Short: "Create a service from a template",
		Long: `Create a service from a template: the Forge API renders the template,
creates the private repo github.com/<org>/<name>, pushes the first commit
(which starts its CI) and registers the service in the catalog. If a step
after the repo exists fails, the repo is deleted again.

Template parameters are set with --set name=value; on a terminal forge asks
for missing required ones. See 'forge template get <template>'.`,
		Example: `  forge create payments --template go-grpc --team billing
  forge create ledger --template go-grpc@v1.0.0 --team billing --owner ana@example.com --set port=9090
  forge create payments --template go-grpc --team billing --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !manifest.ValidID(id) {
				return exitcode.Usagef("%q is not a valid service name (lowercase letters, digits and '-')", id)
			}
			if tmpl == "" {
				return exitcode.Usagef("--template is required").WithHint("see 'forge template list'")
			}
			raw, err := parseSets(sets)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if team == "" {
				if !f.interactive() {
					return exitcode.Usagef("--team is required")
				}
				if team, err = f.ask("Owning team"); err != nil {
					return err
				}
				if team == "" {
					return exitcode.Usagef("--team is required")
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			name, version, _ := strings.Cut(tmpl, "@")
			tr, err := c.Templates.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{
				Name: "templates/" + strings.TrimPrefix(name, "templates/"), Version: version,
			}))
			if err != nil {
				return err
			}
			t := tr.Msg.GetTemplate()
			if err := f.completeParams(t, raw); err != nil {
				return err
			}
			final, err := f.streamCreate(ctx, c, &forgev1.CreateServiceRequest{
				ServiceId:       id,
				Template:        t.GetName(),
				TemplateVersion: t.GetVersion(), // pinned: what was validated is what is rendered
				Parameters:      raw,
				OwnerTeam:       team,
				Owners:          owners,
				RequestId:       client.NewRequestID(),
				ValidateOnly:    dryRun,
			})
			if err != nil {
				return err
			}
			switch {
			case final.GetService() != nil:
				return f.printMutation(final.GetService(), "created", false)
			case final.GetPlan() != nil:
				return f.printPlan(format, strings.TrimPrefix(t.GetName(), "templates/"), final.GetPlan())
			case final.GetFailure() != nil:
				fl := final.GetFailure()
				hint := "the repo was deleted again; fix the cause and run the same command"
				if !fl.GetRolledBack() {
					hint = "the repo could not be deleted; delete it on GitHub before trying again"
				}
				return exitcode.New(exitcode.OperationFailed, "%s failed: %s", fl.GetStep(), fl.GetMessage()).WithHint(hint)
			}
			return errors.New("the Forge API ended the create without a result")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&tmpl, "template", "", "template to use, optionally with @version (see 'forge template list')")
	fl.StringVar(&team, "team", "", "owning team")
	fl.StringSliceVar(&owners, "owner", nil, "owner email or handle (repeatable)")
	fl.StringArrayVar(&sets, "set", nil, "template parameter as name=value (repeatable)")
	fl.BoolVar(&dryRun, "dry-run", false, "render and check without creating anything; print the files")
	return cmd
}

// parseSets turns repeated --set name=value into a map.
func parseSets(sets []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, exitcode.Usagef("--set %q must be name=value", s)
		}
		out[k] = v
	}
	return out, nil
}

// templateParam turns the API's parameter into the template package's, to
// reuse its type, pattern and enum checks before calling the API.
func templateParam(p *forgev1.TemplateParameter) template.Parameter {
	return template.Parameter{
		Name:    p.GetName(),
		Type:    template.ParamType(strings.ToLower(strings.TrimPrefix(p.GetType().String(), "TYPE_"))),
		Pattern: p.GetPattern(),
		Values:  p.GetValues(),
	}
}

// completeParams rejects unknown or invalid values and fills in missing
// required ones by asking, or fails naming the --set flag without a terminal.
func (f *Factory) completeParams(t *forgev1.Template, raw map[string]string) error {
	byName := map[string]*forgev1.TemplateParameter{}
	for _, p := range t.GetParameters() {
		byName[p.GetName()] = p
	}
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if _, ok := byName[k]; !ok {
			return exitcode.Usagef("%s has no parameter %q", t.GetName(), k).
				WithHint(fmt.Sprintf("see 'forge template get %s'", strings.TrimPrefix(t.GetName(), "templates/")))
		}
	}
	for _, p := range t.GetParameters() {
		v, ok := raw[p.GetName()]
		if !ok {
			if !p.GetRequired() {
				continue
			}
			if !f.interactive() {
				return exitcode.Usagef("parameter %q is required", p.GetName()).
					WithHint(fmt.Sprintf("pass --set %s=<%s>", p.GetName(), paramType(p)))
			}
			var err error
			if v, err = f.ask(fmt.Sprintf("%s (%s)", p.GetName(), paramType(p))); err != nil {
				return err
			}
			raw[p.GetName()] = v
		}
		if _, err := templateParam(p).Convert(v); err != nil {
			return exitcode.Usagef("%v", err)
		}
	}
	return nil
}

// stepVerbs turn step names into progress lines.
var stepVerbs = map[string]string{
	"fetch": "fetched template", "render": "rendered", "create_repo": "created",
	"push": "pushed initial commit", "register": "registered",
}

// streamCreate runs CreateService, printing finished steps on stderr, and
// returns its last message. If the stream drops before that message, it
// retries once with the same request_id, which replays the outcome.
func (f *Factory) streamCreate(ctx context.Context, c *client.Client, req *forgev1.CreateServiceRequest) (*forgev1.CreateServiceResponse, error) {
	for attempt := 0; ; attempt++ {
		stream, err := c.Catalog.CreateService(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		var final *forgev1.CreateServiceResponse
		for stream.Receive() {
			msg := stream.Msg()
			if s := msg.GetStep(); s != nil {
				if s.GetState() == forgev1.CreateStep_STATE_DONE {
					verb := stepVerbs[s.GetName()]
					if verb == "" {
						verb = s.GetName()
					}
					f.infof("✓ %s %s", verb, s.GetDetail())
				}
				continue
			}
			final = msg
		}
		err = stream.Err()
		_ = stream.Close()
		if final != nil {
			return final, nil
		}
		dropped := err == nil || connect.CodeOf(err) == connect.CodeUnavailable || connect.CodeOf(err) == connect.CodeUnknown
		if attempt == 0 && dropped && ctx.Err() == nil {
			f.infof("The connection dropped; asking the Forge API for the outcome...")
			continue
		}
		if err == nil {
			err = errors.New("the Forge API ended the create without a result")
		}
		return nil, err
	}
}

// printPlan prints what a --dry-run would create.
func (f *Factory) printPlan(format output.Format, tmpl string, p *forgev1.CreatePlan) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: p.GetFiles(), Data: p})
	}
	files := "files"
	if len(p.GetFiles()) == 1 {
		files = "file"
	}
	if _, err := fmt.Fprintf(f.IO.Out, "Would create %s from %s@%s with %d %s:\n", p.GetRepoUrl(), tmpl, p.GetTemplateVersion(), len(p.GetFiles()), files); err != nil {
		return err
	}
	for _, path := range p.GetFiles() {
		if _, err := fmt.Fprintf(f.IO.Out, "  %s\n", path); err != nil {
			return err
		}
	}
	return nil
}
```

`printMutation(s, "created", false)` prints `services/payments created` in table mode and the resource with `-o json`; check its signature in `internal/cli/service.go` before relying on the exact wording. If its table line differs from `services/payments created`, keep `printMutation` unchanged and adjust `TestCreate`'s expected stdout to what `forge service register` prints for the same verb.

- [ ] **Step 5: Run the tests**

Run: `go test -count=1 -race ./internal/cli -run 'TestCreate' -v 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: every `TestCreate*` PASS.

- [ ] **Step 6: Document** — in `README.md`'s CLI table add the row `| `create` | scaffold a service from a template: repo, first commit, catalog entry |` after the `template` row, change the `template` row's note to `` `list`, `get`, `lint` ``, and add to the "Templates" section:

```markdown
`forge create <name> --template <template> --team <team>` asks forge-api to
render the template, create the private repo `github.com/<org>/<name>` as the
Forge GitHub App, push the first commit (which starts its CI) and register the
service. `--set name=value` sets parameters, `--dry-run` lists the files
without creating anything, and a failure after the repo exists deletes it again.
```

In `.claude/skills/forge/SKILL.md`, in the Phase 2 status line replace "then `forge create`;" with "milestone 3 (`CreateService`, `forge create`) per `docs/plans/2026-10-08-phase-2-m3-create.md`; then milestone 4 (k3s and the exit gate);".

- [ ] **Step 7: Gate and commit**

Run: `GOTOOLCHAIN=go1.24.13 make test lint && make update-golden && git status --short`
Expected: gate passes; no golden file changes.

```bash
git add internal/cli README.md .claude/skills/forge/SKILL.md
git commit -m "cli: forge create

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: One real create on GitHub (owner confirms first)

**Files:** none unless a defect is found (then a failing test first, in the owning task's test file).

**Interfaces:**
- Consumes: everything above; the `mino-forge` App (ID 5239131) and its key; `showmino/forge-templates` `v1.0.0`; the owner's `gh` login to watch CI.

This creates a real private repo `showmino/forge-e2e-m3`, runs its CI (whose `image` job pushes `ghcr.io/showmino/forge-e2e-m3:<sha>`), then deletes the repo. **Stop and ask the owner before Step 2**; the ghcr package is not deleted by deleting the repo (the owner removes it in the org's Packages if wanted).

- [ ] **Step 1: Dry run against the real template**

```bash
make build
bin/forge-api --insecure-dev-token --addr :18081 --github-app-id 5239131 \
  --github-app-key-file mino-forge.2026-10-08.private-key.pem --github-org showmino 2> /tmp/forge-api-m3.log &
sleep 1
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 create forge-e2e-m3 --template go-grpc --team platform --dry-run
```

Expected: `Would create https://github.com/showmino/forge-e2e-m3 from go-grpc@v1.0.0 with 15 files:` and the file list (`cmd/forge-e2e-m3/main.go`, `proto/forgee2em3/v1/greeter.proto`, …).

- [ ] **Step 2: Create for real (after the owner says yes)**

```bash
FORGE_TOKEN=dev bin/forge --api-url http://localhost:18081 create forge-e2e-m3 --template go-grpc --team platform
gh repo view showmino/forge-e2e-m3 --json visibility,defaultBranchRef -q '.visibility + " " + .defaultBranchRef.name'
gh api repos/showmino/forge-e2e-m3/commits --jq 'length'
sleep 10; gh run list -R showmino/forge-e2e-m3 -L 2
gh run watch -R showmino/forge-e2e-m3 --exit-status "$(gh run list -R showmino/forge-e2e-m3 -L1 --json databaseId -q '.[0].databaseId')"
```

Expected: five `✓` lines and `services/forge-e2e-m3 created`; `PRIVATE main`; `1` commit (the scaffold, history starts there); a `ci` run that finishes green (tests, golangci-lint, buf lint, then the image job). This is the Phase 2 exit gate's "passes its own CI", run early on a laptop API; milestone 4 repeats it on k3s.

- [ ] **Step 3: Clean up**

Delete the repo with an App installation token (the owner's `gh` token lacks `delete_repo`): mint a JWT from the key with `openssl` (as in the milestone 2 App check), exchange it for an installation token for installation `169296924`, `DELETE /repos/showmino/forge-e2e-m3` (expect `204`), then revoke the token (`DELETE /installation/token`, expect `204`). Never print the JWT or token. Then:

```bash
kill %1
grep -c -e 'ghs_' -e 'PRIVATE KEY' -e 'eyJhbGciOiJSUzI1NiI' /tmp/forge-api-m3.log; rm -f /tmp/forge-api-m3.log
gh repo view showmino/forge-e2e-m3 2>&1 | head -1
```

Expected: `0` secret matches; the repo is gone ("Could not resolve to a Repository").

---

## After Milestone 3

Milestone 4 (its own plan): the App key as a k8s Secret in `forge`, forge-api's App flags and `--templates-repo` in `deploy/k3s`, `cli-smoke.sh` template and create cases, and the Phase 2 exit gate on k3s: `forge create forge-e2e-<date>` against the cluster's API, its CI green, the service in the catalog, then cleanup.
