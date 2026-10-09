# Phase 4 Milestone 1: Deploy Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `forge deploy start / status / history / rollback / promote` deploy a catalog service to `staging` or `production`. Each deploy is a commit to a GitOps repo, which an Argo CD ApplicationSet turns into Applications; forge-api watches the Application until it is Synced and Healthy.

**Architecture:**
- **API:** forge-api gains `DeployService` (`internal/server/deploys.go`, `deploy_work.go`).
- **Writing deploys:** a `DeployRepo` interface, implemented by `github.RepoFiles` (git data API, as the App), writes `services/<svc>/<env>/values.yaml` to `showmino/forge-deploys`. Each commit message carries `Forge-*` trailers, which are the deploy history.
- **Watching Argo:** forge-api reads Argo's `applications.argoproj.io` objects through the existing `Cluster` (dynamic client), and writes the image pull secret into the namespace Argo creates.
- **Shared with previews:** service resolution and the image wait move out of `env_create.go` into `source.go`.
- **The chart:** it lives in `deploy/argocd/chart`, with goldens rendered by `make chart-test`.
- **Tests:** they use `internal/envtest` fakes. `envtest.Repo` is an in-memory git repo, and `envtest.Argo` plays Argo CD.
- **Not yet:** CI authentication (milestone 2) and the cluster objects (milestone 3).

**Tech Stack:** Go 1.27.2, ConnectRPC, buf v1.47.2, client-go v0.35.9 (no version change), gopkg.in/yaml.v3, Cobra, Helm v3.21.4 run via `go run` (`make chart-test` only; not a module dependency).

**Spec:** `docs/specs/2026-10-09-phase-4-deployments-design.md` (sections 1, 2, 4, 6; milestone 1 in section 7). Read `internal/server/env_create.go`, `environments.go`, `env_cluster.go`, `operations.go` and `internal/envtest/envtest.go` first; this milestone reuses all of them.

## Global Constraints

- **Environments:** exactly `staging` and `production`, in promotion order.
- **Names:**
  - namespace and Argo Application: `<service>-<env>`;
  - resource name: `deployments/<service>/<env>`;
  - host: `<service>.<env>.<Domain>`, where `Domain` is, for example, `10.150.1.222.nip.io`.
- **Deploy repo:** `showmino/forge-deploys`, branch `main`. The file is `services/<service>/<env>/values.yaml`, with the keys `name`, `env`, `image`, `port`, `health`, `replicas` (1), `host` and a `forge:` record (`service`, `env`, `ref`, `commit`, `actor`, `operation`, `kind` ∈ deploy|rollback|promote, `time` RFC 3339).
- **Commit message:** `<kind> <service> to <env>: <image> (<actor>)`, a blank line, then trailers `Forge-Kind`, `Forge-Service`, `Forge-Env`, `Forge-Image`, `Forge-Ref`, `Forge-Commit`, `Forge-Actor`, `Forge-Operation`, one per line as `Key: value`.
- **Commits:** made on the head of `main`. When `main` moved (the ref update is not a fast-forward), rebuild on the new head and retry, at most 3 attempts in total, then fail with `git_conflict`.
- **Synced** means the Application's `status.sync.revisions` (multi-source) or `status.sync.revision` contains the new deploy commit, `status.sync.status == "Synced"` and `status.health.status == "Healthy"`. A missing Application reads as health `Missing`.
- **Timeouts:**
  - sync: 10m (`not_healthy`, with the last Argo message);
  - namespace: 2m (`namespace_timeout`);
  - image: 10m with a 10s poll (shared with previews);
  - the cluster is polled every 2s.
- **No automatic rollback.** A cancel after the commit ends `cancelled`, with a message saying the commit stands.
- **Concurrency:** at most one running mutation (start, rollback or promote) per `<service>/<env>`. A second gets `FailedPrecondition` naming the running operation. A `request_id` repeated on start returns the same operation.
- **Permissions:** reads need `services.read`; mutations need `services.write`. Milestone 2 adds CI scopes.
- **Pull secret:** forge-api writes `forge-pull` (a copy of its pull secret) into `<service>-<env>` only. The namespace must carry the labels `app.kubernetes.io/managed-by=forge`, `forge.dev/service=<service>` and `forge.dev/deploy-env=<env>`; anything else is refused.
- **CLI exit codes:** 2 bad input, 3 not found, 5 permission, 6 conflict or precondition, 7 the operation failed or was cancelled.
- **Gate:** before each commit that touches Go, run `make build test lint` with `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`. Commit messages end with your session's Co-Authored-By line.

## Review Focus

1. **Two deploys to the same target at once, and a commit racing another writer:** exactly one runs; the other gets exit 6; a lost ref race retries on the new head and never overwrites the other commit. Covered by `TestDeployConcurrencyAndConflict` (Task 6).
2. **Argo reports Synced, but to an older revision** (the ApplicationSet hasn't picked up the commit yet): not done until the revision matches. Covered by `TestStartDeploySyncWaitsForRevision` (Task 6).
3. **Rollback with no earlier deploy, and promote with staging empty:** exit 6 before any commit. Covered by `TestRollbackAndPromote` (Task 7).
4. **A namespace `<svc>-<env>` that exists without Forge's labels,** for example one someone made by hand: the pull secret is not written and the deploy fails. Covered by `TestWritePullSecretGuard` (Task 5).
5. **The previews refactor must not change `forge env create`.** The existing environment tests must pass unchanged. Covered by Task 1's run of the full `./internal/server` suite.

---

### Task 0: Branch

- [x] **Step 1:** `cd /home/mino/code/forge && git branch --show-current`. Expected: `phase-4` (the spec is committed there). Then run `git checkout -b phase-4-m1`.

---

### Task 1: Share service resolution and the image wait (refactor)

**Files:**
- Create: `internal/server/source.go`
- Modify: `internal/server/env_create.go`

**Interfaces:**
- Produces:
  - `type source struct { serviceID, owner, repo, ref, commit, image string; port int32; health string }`;
  - `func resolveSource(ctx context.Context, cat *Catalog, gh EnvGitHub, org, service, ref string) (source, error)`, where `service` is `services/<id>` and `ref` defaults to `main`. It returns errors with the same Connect codes as today's `plan()`.
  - `func waitForImage(ctx context.Context, gh EnvGitHub, images ImageChecker, s source, timeout, poll time.Duration, emit func(step, message string)) error`, with the same behaviour and OpError codes (`ci_failed`, `image_timeout`) as today's method.
- `envPlan` becomes `struct { source; id, creator string; ttl time.Duration }`, embedding `source` so `p.serviceID`, `p.image` and the rest keep working.

- [x] **Step 1: Baseline.** `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && go test -race -count=1 ./internal/server > /tmp/claude-1000/-home-mino-code-forge/base.txt 2>&1; tail -1 /tmp/claude-1000/-home-mino-code-forge/base.txt`. Expected: `ok`. This refactor's test is that the environment suite stays green, unchanged.

- [x] **Step 2: Write `internal/server/source.go`** by moving code, not rewriting it:
  - Move the service, ref, `forge.yaml` and image part of `(*Environments).plan` into `resolveSource`. Keep the `envIDRE` and TTL checks and the creator in `plan`.
  - Move `(*Environments).waitForImage` into the free function `waitForImage` above, replacing `e.o.*` with the parameters.
  - Move `failedRun` too.

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// source is a service's code at one commit: where it lives and how it runs.
type source struct {
	serviceID, owner, repo string
	ref, commit, image     string
	port                   int32
	health                 string
}

// resolveSource checks service ("services/<id>") and ref (default main),
// resolves the ref to a commit and reads forge.yaml's run settings there.
func resolveSource(ctx context.Context, cat *Catalog, gh EnvGitHub, org, service, ref string) (source, error) {
	s := source{ref: ref}
	if s.ref == "" {
		s.ref = "main"
	}
	if !refRE.MatchString(s.ref) || strings.Contains(s.ref, "..") {
		return s, invalidArg("%q is not a valid Git ref", s.ref)
	}
	sid, ok := strings.CutPrefix(service, servicePrefix)
	if !ok || sid == "" {
		return s, invalidArg("service %q must be services/<name>", service)
	}
	s.serviceID = sid
	svc, ok := cat.get(servicePrefix + sid)
	if !ok {
		return s, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", servicePrefix, sid))
	}
	if s.owner, s.repo, ok = repoOf(svc.GetRepoUrl(), org); !ok {
		return s, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no GitHub repo in %s (repo_url %q)", servicePrefix, sid, org, svc.GetRepoUrl()))
	}
	var err error
	s.commit, err = gh.ResolveRef(ctx, s.owner, s.repo, s.ref)
	if errors.Is(err, github.ErrNotFound) {
		return s, connect.NewError(connect.CodeNotFound, fmt.Errorf("ref %q not found in github.com/%s/%s", s.ref, s.owner, s.repo))
	}
	if err != nil {
		return s, connect.NewError(connect.CodeUnavailable, fmt.Errorf("resolve %s in github.com/%s/%s: %w", s.ref, s.owner, s.repo, err))
	}
	s.port, s.health = manifest.DefaultRunPort, manifest.DefaultRunHealth
	data, err := gh.FileAt(ctx, s.owner, s.repo, manifest.FileName, s.commit)
	switch {
	case errors.Is(err, github.ErrNotFound): // no forge.yaml: defaults
	case err != nil:
		return s, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read forge.yaml at %s: %w", shortSHA(s.commit), err))
	default:
		mf, err := manifest.Parse(data)
		if err != nil {
			return s, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("forge.yaml at %s is invalid: %w", shortSHA(s.commit), err))
		}
		s.port, s.health = int32(mf.RunPort()), mf.RunHealth() //nolint:gosec // Parse checks run.port is 1-65535
	}
	s.image = "ghcr.io/" + strings.ToLower(s.owner) + "/" + s.serviceID + ":" + shortSHA(s.commit)
	return s, nil
}
```

  Then paste today's `waitForImage` body, changed to the free function, and `failedRun` unchanged:
  - `e.o.ImageTimeout` → `timeout`, `e.o.ImagePoll` → `poll`, `e.o.Images` → `images`, `e.o.GitHub` → `gh`;
  - `p.image`, `p.owner`, `p.repo`, `p.commit` → `s.`.

- [x] **Step 3: Rewire `env_create.go`.**
  - `envPlan` becomes `type envPlan struct { source; id, creator string; ttl time.Duration }`.
  - `plan` keeps the id and TTL checks, then does `src, err := resolveSource(ctx, e.o.Catalog, e.o.GitHub, e.o.Org, m.GetService(), m.GetRef()); if err != nil { return p, err }; p.source = src`, and then sets `p.creator` from the principal.
  - In `createWork`, `e.waitForImage(ctx, p, emit)` becomes `waitForImage(ctx, e.o.GitHub, e.o.Images, p.source, e.o.ImageTimeout, e.o.ImagePoll, emit)`.
  - Delete the moved method and functions, and fix the imports.

- [x] **Step 4:** `go test -race -count=1 ./internal/server ./internal/cli`. Expected: `ok` for both, with no test changed. Then `git diff --stat -- '*_test.go'` must be empty.

- [x] **Step 5:** `make build test lint`, then `git add internal/server && git commit -m "server: share service resolution and the image wait"`.

---

### Task 2: Git data writes to the deploy repo

**Files:**
- Create: `internal/github/repofiles.go`
- Test: `internal/github/repofiles_test.go`

**Interfaces:**
- Consumes: `(*Client).doJSON`, `repoPath`, `escapeRef` (commits.go), `FileAt`, `ErrNotFound`, `*StatusError`.
- Produces:
  - `var ErrNotFastForward = errors.New("the branch moved")`;
  - `type CommitInfo struct { SHA, Message string; Time time.Time }`;
  - `type RepoFiles struct { Client *Client; Owner, Name, Branch string }`, with these methods:
    - `Read(ctx, path, ref string) ([]byte, error)`: ref `""` means Branch; a missing file is `ErrNotFound`;
    - `Head(ctx) (commit, tree string, err error)`;
    - `CommitFile(ctx, parent, tree, path string, content []byte, message string) (string, error)`: one commit with parent `parent`, on `tree` plus `path`=`content`, then a non-force ref update; a 422 on the update is `ErrNotFastForward`;
    - `History(ctx, path string, limit int) ([]CommitInfo, error)`: commits touching path, newest first.

- [x] **Step 1: Write the failing test** `internal/github/repofiles_test.go`:

```go
package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// gitServer is the slice of GitHub's git data API RepoFiles uses.
type gitServer struct {
	mu      sync.Mutex
	head    string
	trees   map[string]string // tree sha -> base tree sha (for asserting base_tree)
	commits map[string]map[string]any
	updates []map[string]any
	moved   bool // answer the next ref update with 422
}

func (g *gitServer) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /repos/showmino/forge-deploys/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		write(w, map[string]any{"object": map[string]string{"sha": g.head}})
	})
	mux.HandleFunc("GET /repos/showmino/forge-deploys/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"sha": r.PathValue("sha"), "tree": map[string]string{"sha": "tree-of-" + r.PathValue("sha")}})
	})
	mux.HandleFunc("POST /repos/showmino/forge-deploys/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BaseTree string           `json:"base_tree"`
			Tree     []map[string]any `json:"tree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Tree) != 1 || in.Tree[0]["path"] != "services/payments/staging/values.yaml" || in.Tree[0]["content"] != "image: x\n" {
			http.Error(w, "bad tree", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.trees["t2"] = in.BaseTree
		g.mu.Unlock()
		write(w, map[string]string{"sha": "t2"})
	})
	mux.HandleFunc("POST /repos/showmino/forge-deploys/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		g.commits["c2"] = in
		g.mu.Unlock()
		write(w, map[string]string{"sha": "c2"})
	})
	mux.HandleFunc("PATCH /repos/showmino/forge-deploys/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.updates = append(g.updates, in)
		if g.moved {
			g.moved = false
			http.Error(w, `{"message":"Update is not a fast forward"}`, http.StatusUnprocessableEntity)
			return
		}
		g.head = in["sha"].(string)
		write(w, map[string]any{})
	})
	mux.HandleFunc("GET /repos/showmino/forge-deploys/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "services/payments/staging/values.yaml" || r.URL.Query().Get("per_page") != "2" {
			write(w, []any{})
			return
		}
		write(w, []map[string]any{
			{"sha": "c2", "commit": map[string]any{"message": "deploy b", "committer": map[string]string{"date": "2026-10-09T12:00:00Z"}}},
			{"sha": "c1", "commit": map[string]any{"message": "deploy a", "committer": map[string]string{"date": "2026-10-08T12:00:00Z"}}},
		})
	})
	return mux
}

func TestRepoFiles(t *testing.T) {
	t.Parallel()
	g := &gitServer{head: "c1", trees: map[string]string{}, commits: map[string]map[string]any{}}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	r := &RepoFiles{Client: &Client{BaseURL: srv.URL}, Owner: "showmino", Name: "forge-deploys", Branch: "main"}
	ctx := context.Background()

	commit, tree, err := r.Head(ctx)
	if err != nil || commit != "c1" || tree != "tree-of-c1" {
		t.Fatalf("Head = %q %q %v", commit, tree, err)
	}
	sha, err := r.CommitFile(ctx, commit, tree, "services/payments/staging/values.yaml", []byte("image: x\n"), "deploy payments")
	if err != nil || sha != "c2" {
		t.Fatalf("CommitFile = %q %v", sha, err)
	}
	if g.trees["t2"] != "tree-of-c1" {
		t.Errorf("base_tree %q", g.trees["t2"])
	}
	if c := g.commits["c2"]; c["tree"] != "t2" || c["message"] != "deploy payments" || c["parents"].([]any)[0] != "c1" {
		t.Errorf("commit %v", c)
	}
	if u := g.updates[0]; u["sha"] != "c2" || u["force"] != false {
		t.Errorf("ref update %v (must not force)", u)
	}

	g.mu.Lock()
	g.moved = true
	g.mu.Unlock()
	if _, err := r.CommitFile(ctx, "c2", "tree-of-c2", "services/payments/staging/values.yaml", []byte("image: x\n"), "again"); !errors.Is(err, ErrNotFastForward) {
		t.Errorf("moved branch: %v, want ErrNotFastForward", err)
	}

	hist, err := r.History(ctx, "services/payments/staging/values.yaml", 2)
	if err != nil || len(hist) != 2 || hist[0].SHA != "c2" || hist[0].Message != "deploy b" || hist[0].Time.Day() != 9 {
		t.Errorf("History = %+v %v", hist, err)
	}
}
```

- [x] **Step 2:** `go test ./internal/github -run TestRepoFiles`. Expected: build failure, `undefined: RepoFiles`.

- [x] **Step 3: Implement** `internal/github/repofiles.go`:

```go
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrNotFastForward is returned when a branch moved since the commit's parent
// was read; the caller rebuilds on the new head.
var ErrNotFastForward = errors.New("the branch moved")

// CommitInfo is one commit of a file's history.
type CommitInfo struct {
	SHA, Message string
	Time         time.Time
}

// RepoFiles reads and commits single files on one branch of one repo through
// the git data API. Forge's deploy repo is written this way, one file per commit.
type RepoFiles struct {
	Client             *Client
	Owner, Name, Branch string
}

func (r *RepoFiles) git() string { return repoPath(r.Owner, r.Name) + "/git" }

// Read returns path at ref (default: the branch). A missing file is ErrNotFound.
func (r *RepoFiles) Read(ctx context.Context, path, ref string) ([]byte, error) {
	if ref == "" {
		ref = r.Branch
	}
	return r.Client.FileAt(ctx, r.Owner, r.Name, path, ref)
}

// Head returns the branch's commit and that commit's tree.
func (r *RepoFiles) Head(ctx context.Context) (string, string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := r.Client.doJSON(ctx, http.MethodGet, r.git()+"/ref/heads/"+escapeRef(r.Branch), nil, &ref); err != nil {
		return "", "", err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := r.Client.doJSON(ctx, http.MethodGet, r.git()+"/commits/"+ref.Object.SHA, nil, &commit); err != nil {
		return "", "", err
	}
	return ref.Object.SHA, commit.Tree.SHA, nil
}

// CommitFile commits path=content on top of parent (whose tree is tree) and
// moves the branch to it without forcing. If the branch is no longer at
// parent, it returns ErrNotFastForward and the branch is unchanged.
func (r *RepoFiles) CommitFile(ctx context.Context, parent, tree, path string, content []byte, message string) (string, error) {
	var newTree, commit struct {
		SHA string `json:"sha"`
	}
	in := map[string]any{
		"base_tree": tree,
		"tree":      []map[string]any{{"path": path, "mode": "100644", "type": "blob", "content": string(content)}},
	}
	if err := r.Client.doJSON(ctx, http.MethodPost, r.git()+"/trees", in, &newTree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := r.Client.doJSON(ctx, http.MethodPost, r.git()+"/commits",
		map[string]any{"message": message, "tree": newTree.SHA, "parents": []string{parent}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	err := r.Client.doJSON(ctx, http.MethodPatch, r.git()+"/refs/heads/"+escapeRef(r.Branch), map[string]any{"sha": commit.SHA, "force": false}, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
		return "", fmt.Errorf("%w: %w", ErrNotFastForward, err)
	}
	if err != nil {
		return "", fmt.Errorf("update %s: %w", r.Branch, err)
	}
	return commit.SHA, nil
}

// History returns up to limit commits that changed path, newest first.
func (r *RepoFiles) History(ctx context.Context, path string, limit int) ([]CommitInfo, error) {
	var out []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	q := url.Values{"path": {path}, "sha": {r.Branch}, "per_page": {strconv.Itoa(limit)}}
	if err := r.Client.doJSON(ctx, http.MethodGet, repoPath(r.Owner, r.Name)+"/commits?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	hist := make([]CommitInfo, 0, len(out))
	for _, c := range out {
		hist = append(hist, CommitInfo{SHA: c.SHA, Message: c.Commit.Message, Time: c.Commit.Committer.Date})
	}
	return hist, nil
}
```

- [x] **Step 4:** `go test -race ./internal/github`. Expected: PASS.

- [x] **Step 5:** `make build test lint`, then `git add internal/github && git commit -m "github: single-file commits and history on a repo branch"`.

---

### Task 3: `deploy.proto` and the client

**Files:**
- Create: `proto/forge/v1/deploy.proto`
- Generated: `gen/forge/v1/deploy.pb.go`, `gen/forge/v1/forgev1connect/deploy.connect.go`
- Modify: `pkg/client/client.go` (add `Deploys`)

**Interfaces:**
- Produces:
  - `forgev1.Deployment`, with enums `Deployment_SyncStatus` (`Deployment_SYNC_STATUS_SYNCED`, `_OUT_OF_SYNC`, `_UNKNOWN`) and `Deployment_Health` (`Deployment_HEALTH_HEALTHY`, `_PROGRESSING`, `_DEGRADED`, `_MISSING`, `_SUSPENDED`, `_UNKNOWN`);
  - `forgev1.DeployRecord`;
  - the request and response types below;
  - `forgev1connect.DeployServiceHandler` and `client.Client.Deploys`.

- [x] **Step 1: Write the proto**

```protobuf
syntax = "proto3";

package forge.v1;

import "forge/v1/operation.proto";
import "google/protobuf/timestamp.proto";

// DeployService ships catalog services to the staging and production
// environments. Every change is a commit to Forge's GitOps repo, which Argo CD
// applies; mutations return an Operation that ends when Argo CD reports the
// new version Synced and Healthy. Names: "deployments/{service}/{env}".
service DeployService {
  // StartDeploy deploys ref (default main) of service to env.
  rpc StartDeploy(StartDeployRequest) returns (StartDeployResponse) {}

  // GetDeployment returns what runs in one environment.
  rpc GetDeployment(GetDeploymentRequest) returns (GetDeploymentResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ListDeployments returns every deployment, by service then environment.
  rpc ListDeployments(ListDeploymentsRequest) returns (ListDeploymentsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ListDeployHistory returns one environment's changes, newest first.
  rpc ListDeployHistory(ListDeployHistoryRequest) returns (ListDeployHistoryResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // RollbackDeploy restores an earlier deploy of one environment.
  rpc RollbackDeploy(RollbackDeployRequest) returns (RollbackDeployResponse) {}

  // PromoteDeploy copies what runs in one environment to the next.
  rpc PromoteDeploy(PromoteDeployRequest) returns (PromoteDeployResponse) {}
}

// Deployment is what runs for one service in one environment.
message Deployment {
  // SyncStatus is Argo CD's sync status.
  enum SyncStatus {
    SYNC_STATUS_UNSPECIFIED = 0;
    SYNC_STATUS_SYNCED = 1;
    SYNC_STATUS_OUT_OF_SYNC = 2;
    SYNC_STATUS_UNKNOWN = 3;
  }
  // Health is Argo CD's health status.
  enum Health {
    HEALTH_UNSPECIFIED = 0;
    HEALTH_HEALTHY = 1;
    HEALTH_PROGRESSING = 2;
    HEALTH_DEGRADED = 3;
    HEALTH_MISSING = 4;
    HEALTH_SUSPENDED = 5;
    HEALTH_UNKNOWN = 6;
  }

  // "deployments/{service}/{env}".
  string name = 1;
  // "services/{service}".
  string service = 2;
  // "staging" or "production".
  string env = 3;
  string image = 4;
  string ref = 5;
  // The service commit that runs.
  string commit = 6;
  string url = 7;
  SyncStatus sync = 8;
  Health health = 9;
  // Argo CD's condition or health message, when there is one.
  string message = 10;
  // The deploy repo commit that set this version.
  string deploy_commit = 11;
  string deployed_by = 12;
  google.protobuf.Timestamp deploy_time = 13;
  // The newest operation on it, while forge-api still has it in memory.
  string operation = 14;
}

// DeployRecord is one change of one environment.
message DeployRecord {
  string deploy_commit = 1;
  // "deploy", "rollback" or "promote".
  string kind = 2;
  string image = 3;
  string ref = 4;
  string commit = 5;
  string actor = 6;
  google.protobuf.Timestamp time = 7;
  string operation = 8;
}

message StartDeployRequest {
  // "services/{service}".
  string service = 1;
  // "staging" or "production".
  string env = 2;
  // Branch, tag or commit; default main.
  string ref = 3;
  // Retries with the same request_id return the same operation.
  string request_id = 4;
  // Check and resolve only; change nothing.
  bool validate_only = 5;
}

message StartDeployResponse {
  // Unset when validate_only.
  Operation operation = 1;
  // What will run (or, with validate_only, would run).
  Deployment deployment = 2;
}

message GetDeploymentRequest {
  // "deployments/{service}/{env}".
  string name = 1;
}

message GetDeploymentResponse {
  Deployment deployment = 1;
}

message ListDeploymentsRequest {
  // Only this service, "services/{service}".
  string service = 1;
  // Only this environment.
  string env = 2;
}

message ListDeploymentsResponse {
  repeated Deployment deployments = 1;
}

message ListDeployHistoryRequest {
  // "deployments/{service}/{env}".
  string name = 1;
  // At most this many records (default 20, at most 50).
  int32 limit = 2;
}

message ListDeployHistoryResponse {
  repeated DeployRecord records = 1;
}

message RollbackDeployRequest {
  // "deployments/{service}/{env}".
  string name = 1;
  // The deploy commit to restore; default the one before the current.
  string to = 2;
  bool validate_only = 3;
}

message RollbackDeployResponse {
  Operation operation = 1;
  // What will run after the rollback.
  Deployment deployment = 2;
}

message PromoteDeployRequest {
  // "services/{service}".
  string service = 1;
  // Default "staging".
  string from = 2;
  // Default "production".
  string to = 3;
  bool validate_only = 4;
}

message PromoteDeployResponse {
  Operation operation = 1;
  // What will run in the target environment.
  Deployment deployment = 2;
}
```

- [x] **Step 2:** `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && make generate && buf lint && go build ./...`. Expected: lint is clean.

- [x] **Step 3: Client.** Add `Deploys forgev1connect.DeployServiceClient` after `Environments` in `pkg/client/client.go`, wire it in `New`, then run `gofmt -w`.

- [x] **Step 4:** `make build test lint`, then `git add proto gen pkg/client && git commit -m "proto: DeployService"`.

---

### Task 4: The chart and its goldens

**Files:**
- Create: `deploy/argocd/chart/Chart.yaml`, `values.yaml`, `templates/deployment.yaml`, `templates/service.yaml`, `templates/httproute.yaml`, `templates/limits.yaml`
- Create: `deploy/argocd/chart/testdata/staging.yaml`, `testdata/staging.golden`
- Modify: `Makefile` (target `chart-test`), `.github/workflows/ci.yml` (run it in the test job)

Values contract: `name`, `env`, `image`, `port`, `health`, `replicas` and `host` are used. The `forge:` block is ignored. The spec lists `name` and `env` implicitly through `forge:`; this task makes them top-level values for the chart.

- [x] **Step 1: The test input and the Makefile target first.** Create `deploy/argocd/chart/testdata/staging.yaml`:

```yaml
name: payments
env: staging
image: ghcr.io/showmino/payments:1a2b3c4
port: 9090
health: /ready
replicas: 1
host: payments.staging.10.150.1.222.nip.io
forge:
  service: services/payments
  env: staging
  ref: main
  commit: 1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d
  actor: ana@example.com
  operation: operations/op_000000000001
  kind: deploy
  time: "2026-10-09T12:00:00Z"
```

Add to the `Makefile`, and add `chart-test` to `.PHONY` if the Makefile has one:

```make
HELM ?= go run helm.sh/helm/v3/cmd/helm@v3.21.4

chart-test: ## Render deploy/argocd/chart and compare it with its golden
	$(HELM) template payments deploy/argocd/chart --namespace payments-staging \
	  -f deploy/argocd/chart/testdata/staging.yaml | diff -u deploy/argocd/chart/testdata/staging.golden -
```

- [x] **Step 2:** `make chart-test`. Expected: fails, because the chart does not exist (`Error: path "deploy/argocd/chart" not found`).

- [x] **Step 3: The chart.**

`Chart.yaml`:

```yaml
apiVersion: v2
name: forge-service
description: A Forge service in one deploy environment (staging or production).
type: application
version: 0.1.0
```

`values.yaml` (defaults; every deploy sets them all):

```yaml
name: ""
env: ""
image: ""
port: 8080
health: /healthz
replicas: 1
host: ""
```

`templates/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ required "name" .Values.name }}
  labels: {app: {{ .Values.name }}}
spec:
  replicas: {{ .Values.replicas }}
  selector:
    matchLabels: {app: {{ .Values.name }}}
  template:
    metadata:
      labels: {app: {{ .Values.name }}}
    spec:
      automountServiceAccountToken: false
      imagePullSecrets:
        - name: forge-pull
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: {{ .Values.name }}
          image: {{ required "image" .Values.image }}
          ports:
            - {name: http, containerPort: {{ .Values.port }}}
          env:
            - {name: PORT, value: "{{ .Values.port }}"}
          readinessProbe:
            httpGet: {path: {{ .Values.health }}, port: {{ .Values.port }}}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: {{ .Values.health }}, port: {{ .Values.port }}}
            initialDelaySeconds: 10
            periodSeconds: 5
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
```

`templates/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.name }}
spec:
  selector: {app: {{ .Values.name }}}
  ports:
    - {name: http, port: 80, targetPort: {{ .Values.port }}}
```

`templates/httproute.yaml`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: {{ .Values.name }}
spec:
  parentRefs:
    - {name: forge-previews, namespace: forge, sectionName: {{ required "env" .Values.env }}}
  hostnames: [{{ required "host" .Values.host | quote }}]
  rules:
    - backendRefs:
        - {name: {{ .Values.name }}, port: 80}
```

`templates/limits.yaml`:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: forge
spec:
  hard:
    requests.cpu: "1"
    requests.memory: 1Gi
    limits.cpu: "2"
    limits.memory: 2Gi
    pods: "10"
---
apiVersion: v1
kind: LimitRange
metadata:
  name: forge
spec:
  limits:
    - type: Container
      defaultRequest: {cpu: 50m, memory: 64Mi}
      default: {cpu: 500m, memory: 512Mi}
```

- [x] **Step 4: Generate the golden, then read it.** Run `go run helm.sh/helm/v3/cmd/helm@v3.21.4 template payments deploy/argocd/chart --namespace payments-staging -f deploy/argocd/chart/testdata/staging.yaml > deploy/argocd/chart/testdata/staging.golden`. Then read the file and check:
  - image `ghcr.io/showmino/payments:1a2b3c4`, `containerPort: 9090`, both probes on `/ready` port 9090, `PORT` `"9090"`;
  - the Service maps 80→9090;
  - the HTTPRoute's parentRef has `sectionName: staging` and the hostname is `"payments.staging.10.150.1.222.nip.io"`;
  - the quota and LimitRange values;
  - no `forge:` content leaked into any object.
  Fix the templates until all of that holds, then regenerate.

- [x] **Step 5:** `make chart-test`. Expected: no diff, exit 0. Then add a step to `.github/workflows/ci.yml`'s `test` job, after the Go tests:

```yaml
      - name: Deploy chart golden
        run: make chart-test
```

- [x] **Step 6:** `make build test lint chart-test`, then `git add Makefile .github deploy/argocd && git commit -m "deploy/argocd: the service chart and its golden"`.

---

### Task 5: Fakes and the cluster side: Argo Applications and the pull secret

**Files:**
- Modify: `internal/envtest/envtest.go` (applications GVR in `Dynamic()`, `Repo`, `Argo`)
- Modify: `internal/server/env_cluster.go` (`Application`, `WaitServiceNamespace`, `WriteServicePullSecret`, constant `labelDeployEnv`)
- Test: `internal/server/deploy_cluster_test.go`, `internal/envtest/envtest_test.go`

**Interfaces:**
- Produces, in `envtest`:
  - `var Applications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}`; `Dynamic()` also knows `ApplicationList`.
  - `type Repo struct{...}`, an in-memory git repo, safe for concurrent use. It has `Read`, `Head`, `CommitFile` and `History`, with the same signatures and errors as `github.RepoFiles`. It also has:
    - `Push(path string, content []byte, message string) string`: an outside writer's commit;
    - `BeforeCommit func()`: called once, before the next `CommitFile`, for racing writers.
  - `type Argo struct { Kube kubernetes.Interface; Dynamic dynamic.Interface; Repo *Repo; Namespace string; Unhealthy map[string]string; Lag time.Duration }` with `Run(ctx)`. Until ctx ends, it plays Argo CD. For every `services/<svc>/<env>/values.yaml` at the repo head, it:
    1. ensures namespace `<svc>-<env>` with Forge's labels;
    2. after `Lag`, sets Application `<svc>-<env>` in `Namespace` to Synced at revisions `["chart", <head>]`, with Healthy health, or Degraded with message `Unhealthy[<svc>-<env>]` when that key is set.
- Produces, in `server`:
  - `labelDeployEnv = "forge.dev/deploy-env"`;
  - `type AppStatus struct { Sync, Health, Message string; Revisions []string }`;
  - `func (c *Cluster) Application(ctx, argoNamespace, name string) (AppStatus, error)`: a missing app is `AppStatus{Health: "Missing", Sync: "Unknown"}` with a nil error;
  - `func (c *Cluster) WaitServiceNamespace(ctx, service, env string, poll time.Duration) error`;
  - `func (c *Cluster) WriteServicePullSecret(ctx, service, env string) error`: guarded, and idempotent (AlreadyExists → nil).

- [x] **Step 1: Write the failing tests.**

`internal/server/deploy_cluster_test.go`:

```go
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/envtest"
)

func serviceNS(name, svc, env string, forge bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if forge {
		ns.Labels = map[string]string{labelManagedBy: "forge", labelService: svc, labelDeployEnv: env}
	}
	return ns
}

func TestWritePullSecretGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster(
		serviceNS("payments-staging", "payments", "staging", true),
		serviceNS("ledger-staging", "ledger", "staging", false), // made by hand
		serviceNS("billing-production", "other", "production", true),
	)
	if err := c.WriteServicePullSecret(ctx, "payments", "staging"); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteServicePullSecret(ctx, "payments", "staging"); err != nil {
		t.Errorf("second write must be a no-op: %v", err)
	}
	s, err := c.Kube.CoreV1().Secrets("payments-staging").Get(ctx, "forge-pull", metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeDockerConfigJson || string(s.Data[corev1.DockerConfigJsonKey]) != string(envtest.PullSecretJSON) {
		t.Errorf("secret: %v %v", s, err)
	}
	for _, tc := range []struct{ svc, env string }{{"ledger", "staging"}, {"billing", "production"}, {"nope", "staging"}, {"payments", "kube"}} {
		if err := c.WriteServicePullSecret(ctx, tc.svc, tc.env); !errors.Is(err, errNoEnv) {
			t.Errorf("%s-%s: %v, want errNoEnv", tc.svc, tc.env, err)
		}
	}
	if _, err := c.Kube.CoreV1().Secrets("ledger-staging").Get(ctx, "forge-pull", metav1.GetOptions{}); err == nil {
		t.Error("wrote into a namespace Forge did not make")
	}

	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.WaitServiceNamespace(wctx, "ledger", "staging", 5*time.Millisecond); err == nil {
		t.Error("waited successfully for an unlabelled namespace")
	}
	if err := c.WaitServiceNamespace(ctx, "payments", "staging", 5*time.Millisecond); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestApplicationStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster()
	st, err := c.Application(ctx, "argocd", "payments-staging")
	if err != nil || st.Health != "Missing" || st.Sync != "Unknown" {
		t.Errorf("missing: %+v %v", st, err)
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "payments-staging", "namespace": "argocd"},
		"status": map[string]any{
			"sync":       map[string]any{"status": "Synced", "revisions": []any{"chartrev", "deploy1"}},
			"health":     map[string]any{"status": "Degraded", "message": "back-off pulling image"},
			"conditions": []any{map[string]any{"type": "SyncError", "message": "one or more objects failed"}},
		},
	}}
	if _, err := c.Dynamic.Resource(envtest.Applications).Namespace("argocd").Create(ctx, app, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err = c.Application(ctx, "argocd", "payments-staging")
	if err != nil || st.Sync != "Synced" || st.Health != "Degraded" || len(st.Revisions) != 2 || st.Revisions[1] != "deploy1" ||
		st.Message != "one or more objects failed; back-off pulling image" {
		t.Errorf("status: %+v %v", st, err)
	}
}
```

`internal/envtest/envtest_test.go`:

```go
package envtest

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/github"
)

func TestRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &Repo{}
	if _, err := r.Read(ctx, "a", ""); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("empty read: %v", err)
	}
	head, tree, _ := r.Head(ctx)
	c1, err := r.CommitFile(ctx, head, tree, "a", []byte("1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	r.Push("b", []byte("x"), "outside")
	if _, err := r.CommitFile(ctx, c1, tree, "a", []byte("2"), "two"); !errors.Is(err, github.ErrNotFastForward) {
		t.Errorf("stale parent: %v", err)
	}
	head, tree, _ = r.Head(ctx)
	c3, _ := r.CommitFile(ctx, head, tree, "a", []byte("3"), "three")
	if got, _ := r.Read(ctx, "a", ""); string(got) != "3" {
		t.Errorf("head read %q", got)
	}
	if got, _ := r.Read(ctx, "a", c1); string(got) != "1" {
		t.Errorf("read at c1 %q", got)
	}
	if got, _ := r.Read(ctx, "b", c3); string(got) != "x" {
		t.Errorf("b kept: %q", got)
	}
	hist, _ := r.History(ctx, "a", 10)
	if len(hist) != 2 || hist[0].SHA != c3 || hist[1].SHA != c1 || hist[0].Message != "three" {
		t.Errorf("history %+v", hist)
	}
}

func TestArgo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kube, dyn, repo := Kube(), Dynamic(), &Repo{}
	a := &Argo{Kube: kube, Dynamic: dyn, Repo: repo, Namespace: "argocd", Unhealthy: map[string]string{"bad-staging": "ImagePullBackOff"}}
	go a.Run(ctx)
	repo.Push("services/payments/staging/values.yaml", []byte("image: x\n"), "deploy")
	repo.Push("services/bad/staging/values.yaml", []byte("image: y\n"), "deploy")
	deadline := time.Now().Add(5 * time.Second)
	for {
		app, err := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "payments-staging", metav1.GetOptions{})
		bad, berr := dyn.Resource(Applications).Namespace("argocd").Get(ctx, "bad-staging", metav1.GetOptions{})
		if err == nil && berr == nil {
			revs, _, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions")
			health, _, _ := unstructured.NestedString(bad.Object, "status", "health", "status")
			if len(revs) == 2 && health == "Degraded" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("argo never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ns, err := kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{})
	if err != nil || ns.Labels["app.kubernetes.io/managed-by"] != "forge" || ns.Labels["forge.dev/deploy-env"] != "staging" || ns.Labels["forge.dev/service"] != "payments" {
		t.Errorf("namespace: %v %v", ns, err)
	}
}
```

- [x] **Step 2:** `go test ./internal/envtest ./internal/server -run 'TestRepo|TestArgo|TestWritePullSecretGuard|TestApplicationStatus'`. Expected: build failure (`undefined: Repo`, `undefined: labelDeployEnv`).

- [x] **Step 3: Implement the fakes** in `internal/envtest/envtest.go`:
  - **`Applications`:** add the var, and add it to `Dynamic()`'s list-kind map as `ApplicationList`.
  - **`Repo`:** an in-memory git repo. Commits are a slice of `{sha string; files map[string][]byte; message string; time time.Time}`. The SHA is `fmt.Sprintf("%040x", n)` for the n-th commit, starting at 1; a zero commit with no files is the initial state, so `Head` on a new repo returns the zero SHA. The tree id equals the commit SHA.
    - `Head` returns the last commit's SHA twice.
    - `CommitFile` runs `BeforeCommit` (and clears it) first. It returns `github.ErrNotFastForward` if `parent` isn't the head; otherwise it appends a commit whose files are the head's plus `path`.
    - `Read(ctx, path, ref)`: ref `""` or `"main"` means the head; otherwise the commit with that SHA. A missing path or commit is `github.ErrNotFound`.
    - `History` walks newest first and keeps commits whose `path` content differs from the previous commit's.
    - `Push` commits on the head unconditionally.
    - Everything is under one mutex.
  - **`Argo.Run`:** a 5ms ticker. Each tick reads the head with `Repo.Head`, lists every `services/*/*/values.yaml` in the head commit (add an unexported `files()` snapshot method on `Repo`), and for each `<svc>-<env>`:
    1. creates the namespace if missing, with labels `app.kubernetes.io/managed-by: forge`, `forge.dev/service: <svc>`, `forge.dev/deploy-env: <env>`;
    2. if `Lag` has passed since that head was first seen, creates or updates the Application with `status.sync = {status: Synced, revisions: [chart, <head>]}` and `status.health`, which is `{status: Healthy}` or `{status: Degraded, message: Unhealthy[key]}`. Use `Create`, and on AlreadyExists, `Get` then `Update`.

- [x] **Step 4: Implement the cluster side** in `internal/server/env_cluster.go`:

```go
const labelDeployEnv = "forge.dev/deploy-env"

var applications = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// AppStatus is what Argo CD reports for one Application.
type AppStatus struct {
	Sync, Health, Message string
	// Revisions are the synced revisions, one per source.
	Revisions []string
}

// Application reads Argo CD's status for name in argoNamespace. A missing
// Application (the ApplicationSet has not generated it yet) is Missing.
func (c *Cluster) Application(ctx context.Context, argoNamespace, name string) (AppStatus, error) {
	u, err := c.Dynamic.Resource(applications).Namespace(argoNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return AppStatus{Sync: "Unknown", Health: "Missing"}, nil
	}
	if err != nil {
		return AppStatus{}, err
	}
	st := AppStatus{}
	st.Sync, _, _ = unstructured.NestedString(u.Object, "status", "sync", "status")
	st.Health, _, _ = unstructured.NestedString(u.Object, "status", "health", "status")
	st.Revisions, _, _ = unstructured.NestedStringSlice(u.Object, "status", "sync", "revisions")
	if rev, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision"); rev != "" {
		st.Revisions = append(st.Revisions, rev)
	}
	var msgs []string
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any)["message"].(string); ok && m != "" {
			msgs = append(msgs, m)
		}
	}
	if m, _, _ := unstructured.NestedString(u.Object, "status", "health", "message"); m != "" {
		msgs = append(msgs, m)
	}
	st.Message = clip(strings.Join(msgs, "; "))
	if st.Sync == "" {
		st.Sync = "Unknown"
	}
	if st.Health == "" {
		st.Health = "Unknown"
	}
	return st, nil
}

// serviceNamespace returns <service>-<env> if Argo CD made it for Forge.
func (c *Cluster) serviceNamespace(ctx context.Context, service, env string) (*corev1.Namespace, error) {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, service+"-"+env, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errNoEnv
	}
	if err != nil {
		return nil, err
	}
	l := ns.Labels
	if l[labelManagedBy] != "forge" || l[labelService] != service || l[labelDeployEnv] != env {
		return nil, errNoEnv
	}
	return ns, nil
}

// WaitServiceNamespace waits until Argo CD has created <service>-<env>.
func (c *Cluster) WaitServiceNamespace(ctx context.Context, service, env string, poll time.Duration) error {
	for {
		_, err := c.serviceNamespace(ctx, service, env)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s-%s: %w", service, env, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// WriteServicePullSecret copies the pull secret into <service>-<env>.
func (c *Cluster) WriteServicePullSecret(ctx context.Context, service, env string) error {
	if _, err := c.serviceNamespace(ctx, service, env); err != nil {
		return err
	}
	pull, err := c.PullSecretData(ctx)
	if err != nil {
		return err
	}
	_, err = c.Kube.CoreV1().Secrets(service+"-"+env).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: pull},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
```

  `WaitServiceNamespace` takes `poll` as a parameter, because `Cluster` holds no timings. Add the `time` import.

- [x] **Step 5:** `go test -race ./internal/envtest ./internal/server`. Expected: PASS, the whole server suite included.

- [x] **Step 6:** `make build test lint`, then `git add internal/envtest internal/server && git commit -m "server: read Argo CD Applications and write service pull secrets; envtest Repo and Argo"`.

---

### Task 6: `DeployService`, part 1: start, get and list

**Files:**
- Create: `internal/server/deploys.go`, `internal/server/deploy_work.go`
- Modify: `internal/server/server.go` (`Options.Deploys`, handler), `internal/server/catalog.go` (`ids()`), `internal/server/operations.go` (`opNameKey` in the work's context)
- Test: `internal/server/deploys_test.go`

**Interfaces:**
- Consumes: Task 1's `resolveSource` and `waitForImage`; Task 5's Cluster methods and `envtest.Repo`/`Argo`; `Operations`; `require`; `PrincipalFrom`; `shortSHA`; `clip`; `servicePrefix`; `OpError`.
- Produces:
  - `type DeployRepo interface { Read(ctx, path, ref string) ([]byte, error); Head(ctx) (string, string, error); CommitFile(ctx, parent, tree, path string, content []byte, message string) (string, error); History(ctx, path string, limit int) ([]github.CommitInfo, error) }`, satisfied by `*github.RepoFiles` and `*envtest.Repo`;
  - `type DeployOptions struct { Cluster *Cluster; GitHub EnvGitHub; Images ImageChecker; Repo DeployRepo; Catalog *Catalog; Operations *Operations; Org, Domain, ArgoNamespace string; ImageTimeout, SyncTimeout, NamespaceTimeout, ImagePoll, Poll time.Duration; Now func() time.Time; Log *slog.Logger }`, with defaults: ArgoNamespace `argocd`; 10m, 10m, 2m, 10s, 2s;
  - `func NewDeploys(o DeployOptions) *Deploys`; a nil `*Deploys` answers `FailedPrecondition` "deploys are not configured on this Forge API";
  - `Options.Deploys *Deploys`;
  - `deployEnvs = []string{"staging", "production"}`;
  - `valuesPath(svc, env string) string`;
  - `type deployValues struct{...}` and `type deployRecord struct{...}`, with `(deployValues) marshal() []byte`, `parseValues([]byte) (deployValues, error)`, `commitMessage(r deployRecord) string` and `parseRecord(sha, message string, t time.Time) (deployRecord, bool)`;
  - `(d *Deploys) commit(ctx, path string, build func() ([]byte, error), message string, emit) (string, error)` (retry);
  - `(d *Deploys) awaitSync(ctx, svc, env, deployCommit string, emit) error`;
  - `(d *Deploys) lock(target, opName string) error` and `unlock(target)`;
  - `(d *Deploys) deploymentFrom(svc, env string, v deployValues, deployCommit string, st AppStatus) *forgev1.Deployment`.

- [x] **Step 1: Write the failing tests** `internal/server/deploys_test.go`:

```go
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
)

// deployFixture is forge-api with deploys on fakes: service "payments"
// (repo showmino/payments, run port 9090 health /ready), main at envSHA,
// its image present, and envtest.Argo playing Argo CD unless noArgo.
type deployFixture struct {
	cluster *Cluster
	repo    *envtest.Repo
	images  *envtest.Images
	ops     *Operations
	deploys *Deploys
	client  forgev1connect.DeployServiceClient
	argo    *envtest.Argo
}

func newDeployFixture(t *testing.T, noArgo bool, tweak func(*DeployOptions, *envtest.Argo)) *deployFixture {
	t.Helper()
	f := &deployFixture{
		cluster: &Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"},
		repo:    &envtest.Repo{},
		images:  &envtest.Images{},
		ops:     NewOperations(nil),
	}
	f.images.Add("ghcr.io/showmino/payments:1a2b3c4")
	gh := &envtest.GitHub{
		Refs:  map[string]string{"main": envSHA},
		Files: map[string][]byte{envSHA + ":forge.yaml": []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  health: /ready\n")},
	}
	catalog := NewCatalog(nil)
	if _, err := catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	})); err != nil {
		t.Fatal(err)
	}
	o := DeployOptions{
		Cluster: f.cluster, GitHub: gh, Images: f.images, Repo: f.repo, Catalog: catalog, Operations: f.ops,
		Org: "showmino", Domain: "10.150.1.222.nip.io",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, SyncTimeout: 5 * time.Second, NamespaceTimeout: 5 * time.Second,
	}
	f.argo = &envtest.Argo{Kube: f.cluster.Kube, Dynamic: f.cluster.Dynamic, Repo: f.repo, Namespace: "argocd", Unhealthy: map[string]string{}}
	if tweak != nil {
		tweak(&o, f.argo)
	}
	f.deploys = NewDeploys(o)
	if !noArgo {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { f.argo.Run(ctx) })
		t.Cleanup(func() { cancel(); wg.Wait() })
	}
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Catalog:    catalog, Operations: f.ops, Deploys: f.deploys,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewDeployServiceClient(srv.Client(), srv.URL)
	return f
}

func (f *deployFixture) start(t *testing.T, env string) (*forgev1.StartDeployResponse, error) {
	t.Helper()
	resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: env}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestStartDeploy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if d := resp.GetDeployment(); d.GetName() != "deployments/payments/staging" || d.GetImage() != "ghcr.io/showmino/payments:1a2b3c4" || d.GetUrl() != "http://payments.staging.10.150.1.222.nip.io" {
		t.Errorf("preview: %v", d)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil {
		t.Fatalf("op: %v", op.GetError())
	}
	if got := eventSteps(op); got != "resolve,image,commit,namespace,sync" {
		t.Errorf("steps %s", got)
	}
	data, err := f.repo.Read(context.Background(), "services/payments/staging/values.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := parseValues(data)
	if err != nil || v.Name != "payments" || v.Env != "staging" || v.Port != 9090 || v.Health != "/ready" || v.Replicas != 1 ||
		v.Host != "payments.staging.10.150.1.222.nip.io" || v.Forge.Kind != "deploy" || v.Forge.Commit != envSHA ||
		v.Forge.Actor != "ana@example.com" || v.Forge.Operation != op.GetName() {
		t.Errorf("values: %+v %v", v, err)
	}
	hist, _ := f.repo.History(context.Background(), "services/payments/staging/values.yaml", 1)
	rec, ok := parseRecord(hist[0].SHA, hist[0].Message, hist[0].Time)
	if !ok || !strings.HasPrefix(hist[0].Message, "deploy payments to staging: ghcr.io/showmino/payments:1a2b3c4 (ana@example.com)\n\n") || rec.Image != v.Image || rec.Operation != op.GetName() {
		t.Errorf("commit message %q -> %+v", hist[0].Message, rec)
	}
	if _, err := f.cluster.Kube.CoreV1().Secrets("payments-staging").Get(context.Background(), "forge-pull", metav1.GetOptions{}); err != nil {
		t.Errorf("pull secret: %v", err)
	}

	got, err := f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	d := got.Msg.GetDeployment()
	if d.GetSync() != forgev1.Deployment_SYNC_STATUS_SYNCED || d.GetHealth() != forgev1.Deployment_HEALTH_HEALTHY || d.GetCommit() != envSHA ||
		d.GetDeployCommit() != hist[0].SHA || d.GetDeployedBy() != "ana@example.com" || d.GetOperation() != op.GetName() {
		t.Errorf("get: %v", d)
	}
	list, err := f.client.ListDeployments(context.Background(), as("reader", &forgev1.ListDeploymentsRequest{}))
	if err != nil || len(list.Msg.GetDeployments()) != 1 {
		t.Errorf("list: %v %v", list, err)
	}
	for name, code := range map[string]connect.Code{
		"deployments/payments/production": connect.CodeNotFound,
		"deployments/payments/qa":         connect.CodeInvalidArgument,
		"deployments/payments":            connect.CodeInvalidArgument,
	} {
		if _, err := f.client.GetDeployment(context.Background(), as("reader", &forgev1.GetDeploymentRequest{Name: name})); connect.CodeOf(err) != code {
			t.Errorf("get %s: %v, want %v", name, err, code)
		}
	}
}

func TestStartDeployValidation(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, true, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		req  *forgev1.StartDeployRequest
		tok  string
		code connect.Code
	}{
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "qa"}, "writer", connect.CodeInvalidArgument},
		{&forgev1.StartDeployRequest{Service: "services/nope", Env: "staging"}, "writer", connect.CodeNotFound},
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "nope"}, "writer", connect.CodeNotFound},
		{&forgev1.StartDeployRequest{Service: "services/payments", Env: "staging"}, "reader", connect.CodePermissionDenied},
	} {
		if _, err := f.client.StartDeploy(ctx, as(tc.tok, tc.req)); connect.CodeOf(err) != tc.code {
			t.Errorf("%v as %s: %v, want %v", tc.req, tc.tok, err, tc.code)
		}
	}
	dry, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetDeployment().GetImage() == "" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if head, _, _ := f.repo.Head(ctx); head != strings.Repeat("0", 40) {
		t.Error("validate_only committed")
	}

	off, err := New(Options{Authenticator: StaticAuthenticator{"writer": {Subject: "ana"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(off)
	t.Cleanup(srv.Close)
	c := forgev1connect.NewDeployServiceClient(srv.Client(), srv.URL)
	if _, err := c.ListDeployments(ctx, as("writer", &forgev1.ListDeploymentsRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("deploys off: %v", err)
	}
}

func TestStartDeploySyncWaitsForRevision(t *testing.T) {
	t.Parallel()
	// Argo lags 300ms behind every new head: until then it reports the old
	// revision (or nothing), and the deploy must keep waiting.
	f := newDeployFixture(t, false, func(_ *DeployOptions, a *envtest.Argo) { a.Lag = 300 * time.Millisecond })
	start := time.Now()
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil || time.Since(start) < 300*time.Millisecond {
		t.Errorf("finished after %v: %v", time.Since(start), op.GetError())
	}
}

func TestStartDeployNotHealthy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, a *envtest.Argo) {
		o.SyncTimeout = 200 * time.Millisecond
		a.Unhealthy["payments-staging"] = "ImagePullBackOff"
	})
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError().GetCode() != "not_healthy" || !strings.Contains(op.GetError().GetMessage(), "ImagePullBackOff") {
		t.Errorf("error: %v", op.GetError())
	}
	if _, err := f.repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err != nil {
		t.Error("the commit must stay (no automatic rollback)")
	}
}

func TestDeployConcurrencyAndConflict(t *testing.T) {
	t.Parallel()
	// No Argo: the first deploy waits in "namespace" and keeps the target busy.
	f := newDeployFixture(t, true, nil)
	first, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), first.GetOperation().GetName()) {
		t.Errorf("second deploy: %v", err)
	}
	f.ops.Cancel(first.GetOperation().GetName())
	if op := wait(t, f.ops, first.GetOperation().GetName()); op.GetError().GetCode() != OpCodeCancelled || !strings.Contains(op.GetError().GetMessage(), "stands") {
		t.Errorf("cancel after commit: %v", op.GetError())
	}

	// Another writer pushes between our Head and our CommitFile.
	g := newDeployFixture(t, false, nil)
	g.repo.BeforeCommit = func() { g.repo.Push("services/other/staging/values.yaml", []byte("image: o\n"), "outside") }
	resp, err := g.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, g.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("retry on the new head: %v", op.GetError())
	}
	if _, err := g.repo.Read(context.Background(), "services/other/staging/values.yaml", ""); err != nil {
		t.Error("the other writer's commit was overwritten")
	}

	h := newDeployFixture(t, true, nil)
	n := 0
	var bump func()
	bump = func() { n++; h.repo.Push("x", []byte{byte(n)}, "outside"); h.repo.BeforeCommit = bump }
	h.repo.BeforeCommit = bump
	resp, err = h.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, h.ops, resp.GetOperation().GetName()); op.GetError().GetCode() != "git_conflict" || n != 3 {
		t.Errorf("after %d conflicts: %v", n, op.GetError())
	}
}
```

  `BeforeCommit` is cleared by the fake before it runs. The `bump` closure re-arms it, so every attempt races: three attempts, then `git_conflict`.

- [x] **Step 2:** `go test ./internal/server -run 'TestStartDeploy|TestDeployConcurrency'`. Expected: build failure, `undefined: Deploys`.

- [x] **Step 3: `catalog.go`:** add

```go
// ids returns the registered service ids, sorted.
func (c *Catalog) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.services))
	for name := range c.services {
		out = append(out, strings.TrimPrefix(name, servicePrefix))
	}
	slices.Sort(out)
	return out
}
```

  Add the `slices` import if it's missing.

- [x] **Step 4: Write `internal/server/deploys.go`**: the service, its values and records, and the reads.

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

const (
	deployNamePrefix = "deployments/"
	kindDeploy       = "deploy"
	kindRollback     = "rollback"
	kindPromote      = "promote"
	opDeploy         = "deploy"
)

// deployEnvs are the deploy environments, in promotion order.
var deployEnvs = []string{"staging", "production"}

// DeployRepo is Forge's GitOps repo (*github.RepoFiles on GitHub).
type DeployRepo interface {
	Read(ctx context.Context, path, ref string) ([]byte, error)
	Head(ctx context.Context) (string, string, error)
	CommitFile(ctx context.Context, parent, tree, path string, content []byte, message string) (string, error)
	History(ctx context.Context, path string, limit int) ([]github.CommitInfo, error)
}

// DeployOptions configure deploys.
type DeployOptions struct {
	Cluster    *Cluster
	GitHub     EnvGitHub
	Images     ImageChecker
	Repo       DeployRepo
	Catalog    *Catalog
	Operations *Operations
	// Org is the GitHub org services' repos must be in.
	Org string
	// Domain: hosts are <service>.<env>.<Domain>.
	Domain string
	// ArgoNamespace holds the Applications (default argocd).
	ArgoNamespace string
	// Defaults: 10m, 10m, 2m.
	ImageTimeout, SyncTimeout, NamespaceTimeout time.Duration
	// Defaults: 10s, 2s.
	ImagePoll, Poll time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Deploys serves DeployService. A nil *Deploys answers FAILED_PRECONDITION.
type Deploys struct {
	o DeployOptions

	mu sync.Mutex
	// running maps "<service>/<env>" to its running mutation.
	running map[string]string
	// requests maps StartDeploy request_ids to their operation.
	requests map[string]string
}

// NewDeploys returns the deploy service.
func NewDeploys(o DeployOptions) *Deploys {
	if o.ArgoNamespace == "" {
		o.ArgoNamespace = "argocd"
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.ImageTimeout, 10 * time.Minute}, {&o.SyncTimeout, 10 * time.Minute}, {&o.NamespaceTimeout, 2 * time.Minute}, {&o.ImagePoll, 10 * time.Second}, {&o.Poll, 2 * time.Second}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Deploys{o: o, running: map[string]string{}, requests: map[string]string{}}
}

func (d *Deploys) ready() error {
	if d == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("deploys are not configured on this Forge API"))
	}
	return nil
}

func valuesPath(svc, env string) string { return "services/" + svc + "/" + env + "/values.yaml" }

func (d *Deploys) host(svc, env string) string { return svc + "." + env + "." + d.o.Domain }

func validEnv(env string) error {
	if !slices.Contains(deployEnvs, env) {
		return invalidArg("environment %q must be staging or production", env)
	}
	return nil
}

// parseDeployName turns "deployments/<svc>/<env>" into its parts.
func parseDeployName(name string) (string, string, error) {
	rest, ok := strings.CutPrefix(name, deployNamePrefix)
	svc, env, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || svc == "" {
		return "", "", invalidArg("%q is not a deployment name (deployments/<service>/<env>)", name)
	}
	return svc, env, validEnv(env)
}

// deployRecord is who changed an environment to what, and why.
type deployRecord struct {
	Service   string `yaml:"service"`
	Env       string `yaml:"env"`
	Ref       string `yaml:"ref"`
	Commit    string `yaml:"commit"`
	Actor     string `yaml:"actor"`
	Operation string `yaml:"operation"`
	Kind      string `yaml:"kind"`
	Time      string `yaml:"time"`
	// Image and DeployCommit come from the commit, not the values file.
	Image        string `yaml:"-"`
	DeployCommit string `yaml:"-"`
}

// deployValues is services/<svc>/<env>/values.yaml.
type deployValues struct {
	Name     string       `yaml:"name"`
	Env      string       `yaml:"env"`
	Image    string       `yaml:"image"`
	Port     int32        `yaml:"port"`
	Health   string       `yaml:"health"`
	Replicas int          `yaml:"replicas"`
	Host     string       `yaml:"host"`
	Forge    deployRecord `yaml:"forge"`
}

func (v deployValues) marshal() []byte {
	data, err := yaml.Marshal(v)
	if err != nil {
		panic(err) // a plain struct always marshals
	}
	return append([]byte("# Written by Forge; change it with 'forge deploy'.\n"), data...)
}

func parseValues(data []byte) (deployValues, error) {
	var v deployValues
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("deploy values: %w", err)
	}
	return v, nil
}

var trailerKeys = []string{"Kind", "Service", "Env", "Image", "Ref", "Commit", "Actor", "Operation"}

// commitMessage is the deploy repo commit message for r.
func commitMessage(r deployRecord) string {
	svc := strings.TrimPrefix(r.Service, servicePrefix)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s to %s: %s (%s)\n\n", r.Kind, svc, r.Env, r.Image, r.Actor)
	vals := []string{r.Kind, r.Service, r.Env, r.Image, r.Ref, r.Commit, r.Actor, r.Operation}
	for i, k := range trailerKeys {
		fmt.Fprintf(&b, "Forge-%s: %s\n", k, vals[i])
	}
	return b.String()
}

// parseRecord reads a deploy record back from a commit; ok is false for
// commits Forge did not write.
func parseRecord(sha, message string, t time.Time) (deployRecord, bool) {
	r := deployRecord{DeployCommit: sha, Time: t.UTC().Format(time.RFC3339)}
	found := 0
	for line := range strings.SplitSeq(message, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok || !strings.HasPrefix(k, "Forge-") {
			continue
		}
		found++
		switch strings.TrimPrefix(k, "Forge-") {
		case "Kind":
			r.Kind = v
		case "Service":
			r.Service = v
		case "Env":
			r.Env = v
		case "Image":
			r.Image = v
		case "Ref":
			r.Ref = v
		case "Commit":
			r.Commit = v
		case "Actor":
			r.Actor = v
		case "Operation":
			r.Operation = v
		}
	}
	return r, found > 0 && r.Kind != ""
}

var syncStates = map[string]forgev1.Deployment_SyncStatus{
	"Synced": forgev1.Deployment_SYNC_STATUS_SYNCED, "OutOfSync": forgev1.Deployment_SYNC_STATUS_OUT_OF_SYNC,
}

var healthStates = map[string]forgev1.Deployment_Health{
	"Healthy": forgev1.Deployment_HEALTH_HEALTHY, "Progressing": forgev1.Deployment_HEALTH_PROGRESSING,
	"Degraded": forgev1.Deployment_HEALTH_DEGRADED, "Missing": forgev1.Deployment_HEALTH_MISSING,
	"Suspended": forgev1.Deployment_HEALTH_SUSPENDED,
}

// deploymentFrom builds the API view of one environment.
func (d *Deploys) deploymentFrom(svc, env string, v deployValues, deployCommit string, st AppStatus) *forgev1.Deployment {
	dep := &forgev1.Deployment{
		Name: deployNamePrefix + svc + "/" + env, Service: servicePrefix + svc, Env: env,
		Image: v.Image, Ref: v.Forge.Ref, Commit: v.Forge.Commit, Url: "http://" + d.host(svc, env),
		Sync: forgev1.Deployment_SYNC_STATUS_UNKNOWN, Health: forgev1.Deployment_HEALTH_UNKNOWN,
		Message: st.Message, DeployCommit: deployCommit, DeployedBy: v.Forge.Actor,
	}
	if s, ok := syncStates[st.Sync]; ok {
		dep.Sync = s
	}
	if h, ok := healthStates[st.Health]; ok {
		dep.Health = h
	}
	if t, err := time.Parse(time.RFC3339, v.Forge.Time); err == nil {
		dep.DeployTime = timestamppb.New(t)
	}
	if ops := d.o.Operations.List(dep.Name, false); len(ops) > 0 {
		dep.Operation = ops[0].GetName()
	}
	return dep
}

// current reads an environment's values at the head and the commit that set them.
func (d *Deploys) current(ctx context.Context, svc, env string) (deployValues, string, error) {
	data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), "")
	if errors.Is(err, github.ErrNotFound) {
		return deployValues{}, "", connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s/%s has never been deployed", deployNamePrefix, svc, env))
	}
	if err != nil {
		return deployValues{}, "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo: %w", err))
	}
	v, err := parseValues(data)
	if err != nil {
		return v, "", connect.NewError(connect.CodeInternal, err)
	}
	hist, err := d.o.Repo.History(ctx, valuesPath(svc, env), 1)
	if err != nil || len(hist) == 0 {
		return v, "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("deploy repo history: %w", err))
	}
	return v, hist[0].SHA, nil
}

func (d *Deploys) GetDeployment(ctx context.Context, req *connect.Request[forgev1.GetDeploymentRequest]) (*connect.Response[forgev1.GetDeploymentResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	v, commit, err := d.current(ctx, svc, env)
	if err != nil {
		return nil, err
	}
	st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("argo cd: %w", err))
	}
	return connect.NewResponse(&forgev1.GetDeploymentResponse{Deployment: d.deploymentFrom(svc, env, v, commit, st)}), nil
}

func (d *Deploys) ListDeployments(ctx context.Context, req *connect.Request[forgev1.ListDeploymentsRequest]) (*connect.Response[forgev1.ListDeploymentsResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	if e := req.Msg.GetEnv(); e != "" {
		if err := validEnv(e); err != nil {
			return nil, err
		}
	}
	var out []*forgev1.Deployment
	for _, svc := range d.o.Catalog.ids() {
		if s := req.Msg.GetService(); s != "" && s != servicePrefix+svc {
			continue
		}
		for _, env := range deployEnvs {
			if e := req.Msg.GetEnv(); e != "" && e != env {
				continue
			}
			v, commit, err := d.current(ctx, svc, env)
			if connect.CodeOf(err) == connect.CodeNotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("argo cd: %w", err))
			}
			out = append(out, d.deploymentFrom(svc, env, v, commit, st))
		}
	}
	return connect.NewResponse(&forgev1.ListDeploymentsResponse{Deployments: out}), nil
}

// ListDeployHistory, RollbackDeploy and PromoteDeploy: Task 7.
func (d *Deploys) ListDeployHistory(context.Context, *connect.Request[forgev1.ListDeployHistoryRequest]) (*connect.Response[forgev1.ListDeployHistoryResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListDeployHistory is not implemented yet"))
}

func (d *Deploys) RollbackDeploy(context.Context, *connect.Request[forgev1.RollbackDeployRequest]) (*connect.Response[forgev1.RollbackDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("RollbackDeploy is not implemented yet"))
}

func (d *Deploys) PromoteDeploy(context.Context, *connect.Request[forgev1.PromoteDeployRequest]) (*connect.Response[forgev1.PromoteDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("PromoteDeploy is not implemented yet"))
}
```

  `strings.SplitSeq` is in Go 1.24+.

- [x] **Step 5: Write `internal/server/deploy_work.go`**: start, the lock, commit-with-retry, namespace and sync.

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

// lock claims target ("<svc>/<env>") for opName; d.mu must be held.
func (d *Deploys) lock(target, opName string) error {
	if running, ok := d.running[target]; ok {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is already running for %s%s", running, deployNamePrefix, target))
	}
	d.running[target] = opName
	return nil
}

func (d *Deploys) unlock(target string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, target)
}

func actorOf(ctx context.Context) string {
	p, ok := PrincipalFrom(ctx)
	switch {
	case !ok:
		return "unknown"
	case p.Email != "":
		return p.Email
	default:
		return p.Subject
	}
}

func (d *Deploys) StartDeploy(ctx context.Context, req *connect.Request[forgev1.StartDeployRequest]) (*connect.Response[forgev1.StartDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validEnv(m.GetEnv()); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, m.GetService(), m.GetRef())
	if err != nil {
		return nil, err
	}
	env, actor := m.GetEnv(), actorOf(ctx)
	v := deployValues{
		Name: src.serviceID, Env: env, Image: src.image, Port: src.port, Health: src.health, Replicas: 1,
		Host: d.host(src.serviceID, env),
		Forge: deployRecord{Service: servicePrefix + src.serviceID, Env: env, Ref: src.ref, Commit: src.commit, Actor: actor, Kind: kindDeploy},
	}
	preview := d.deploymentFrom(src.serviceID, env, v, "", AppStatus{})
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.StartDeployResponse{Deployment: preview}), nil
	}
	target := src.serviceID + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if key := m.GetRequestId(); key != "" {
		if name, ok := d.requests[key]; ok {
			if op, ok := d.o.Operations.Get(name); ok {
				return connect.NewResponse(&forgev1.StartDeployResponse{Operation: op, Deployment: preview}), nil
			}
		}
	}
	if err := d.lock(target, "an operation"); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, opDeploy, deployNamePrefix+target, actor, func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		emit("resolve", fmt.Sprintf("%s is %s", src.ref, shortSHA(src.commit)))
		if err := waitForImage(ctx, d.o.GitHub, d.o.Images, src, d.o.ImageTimeout, d.o.ImagePoll, emit); err != nil {
			return err
		}
		return d.apply(ctx, src.serviceID, env, v, emit)
	})
	d.running[target] = op.GetName()
	if key := m.GetRequestId(); key != "" {
		d.requests[key] = op.GetName()
	}
	return connect.NewResponse(&forgev1.StartDeployResponse{Operation: op, Deployment: preview}), nil
}

// apply commits v for svc/env, then waits for its namespace and for Argo CD.
// v.Forge's operation and time are filled in here.
func (d *Deploys) apply(ctx context.Context, svc, env string, v deployValues, emit func(string, string)) error {
	if p, ok := ctx.Value(opNameKey{}).(string); ok {
		v.Forge.Operation = p
	}
	v.Forge.Time = d.o.Now().UTC().Format(time.RFC3339)
	rec := v.Forge
	rec.Image = v.Image
	commit, err := d.commit(ctx, valuesPath(svc, env), v.marshal(), commitMessage(rec))
	if err != nil {
		return err
	}
	emit("commit", fmt.Sprintf("forge-deploys %s: %s", shortSHA(commit), v.Image))
	stands := func(err error) error {
		if ctx.Err() != nil {
			return &OpError{Code: OpCodeCancelled, Message: fmt.Sprintf("cancelled; the deploy commit %s stands and Argo CD keeps applying it", shortSHA(commit))}
		}
		return err
	}
	nctx, cancel := context.WithTimeout(ctx, d.o.NamespaceTimeout)
	err = d.o.Cluster.WaitServiceNamespace(nctx, svc, env, d.o.Poll)
	cancel()
	if err != nil {
		return stands(&OpError{Code: "namespace_timeout", Message: fmt.Sprintf("namespace %s-%s did not appear within %s: %v", svc, env, d.o.NamespaceTimeout, err)})
	}
	if err := d.o.Cluster.WriteServicePullSecret(ctx, svc, env); err != nil {
		return stands(fmt.Errorf("pull secret for %s-%s: %w", svc, env, err))
	}
	emit("namespace", svc+"-"+env)
	return stands(d.awaitSync(ctx, svc, env, commit, emit))
}

// commit writes content to path on the deploy repo's head, retrying on a
// moved branch (3 attempts).
func (d *Deploys) commit(ctx context.Context, path string, content []byte, message string) (string, error) {
	for range 3 {
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return "", fmt.Errorf("deploy repo head: %w", err)
		}
		sha, err := d.o.Repo.CommitFile(ctx, head, tree, path, content, message)
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		return sha, err
	}
	return "", &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; try again"}
}

// awaitSync waits until Argo CD has synced deployCommit and the app is healthy.
func (d *Deploys) awaitSync(ctx context.Context, svc, env, deployCommit string, emit func(string, string)) error {
	timeout := time.NewTimer(d.o.SyncTimeout)
	defer timeout.Stop()
	last := ""
	for {
		st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
		if err == nil {
			atRev := slices.Contains(st.Revisions, deployCommit)
			if atRev && st.Sync == "Synced" && st.Health == "Healthy" {
				emit("sync", "http://"+d.host(svc, env))
				return nil
			}
			msg := fmt.Sprintf("%s, %s", st.Sync, st.Health)
			if !atRev {
				msg += ", waiting for " + shortSHA(deployCommit)
			}
			if st.Message != "" {
				msg += ": " + st.Message
			}
			if msg != last {
				emit("sync", msg)
				last = msg
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "not_healthy", Message: fmt.Sprintf("%s-%s was not Synced and Healthy within %s: %s", svc, env, d.o.SyncTimeout, last)}
		case <-time.After(d.o.Poll):
		}
	}
}
```

  **The work needs its own operation name** for `v.Forge.Operation`, but `Start` returns the name only after the work starts. Give the work its name through its context:
  - in `operations.go`, add `type opNameKey struct{}`;
  - in `(*Operations).Start`, after the name is chosen, wrap the work's context with `context.WithValue(ctx, opNameKey{}, o.pb.Name)` before `go s.run(...)`.

  This is a one-line change to milestone 1's store. `apply` reads it.

  **`StartDeploy`'s `d.lock(target, "an operation")`** reserves the target before `Start`, and the next line replaces the placeholder with the real name while `d.mu` is still held. A concurrent caller therefore always sees a running entry.

- [x] **Step 6: Wire it.** In `server.go`, add to `Options`, after `Environments`:

```go
	// Deploys serves DeployService; nil answers FAILED_PRECONDITION. It must
	// share Catalog and Operations with these Options.
	Deploys *Deploys
```

  and register `mux.Handle(forgev1connect.NewDeployServiceHandler(o.Deploys, opts))`.

- [x] **Step 7:** `go test -race -count=3 ./internal/server`. Expected: PASS, including every earlier test and `TestStartDeploy*` / `TestDeployConcurrencyAndConflict`.

- [x] **Step 8:** `make build test lint`, then `git add internal/server && git commit -m "server: DeployService start, get and list"`.

---

### Task 7: `DeployService`, part 2: history, rollback and promote

**Files:**
- Modify: `internal/server/deploys.go` (replace the three `Unimplemented` methods)
- Test: `internal/server/deploys_more_test.go`

**Interfaces:**
- Consumes: Task 6's `current`, `deploymentFrom`, `lock`/`unlock`, `apply`, `parseRecord`, `parseValues`, `valuesPath` and `deployFixture`.

- [x] **Step 1: Write the failing tests** `internal/server/deploys_more_test.go`:

```go
package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func (f *deployFixture) deployOK(t *testing.T, env string) {
	t.Helper()
	resp, err := f.start(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
}

func TestRollbackAndPromote(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()

	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("rollback with no deploys: %v", err)
	}
	if _, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("promote from empty staging: %v", err)
	}

	f.deployOK(t, "staging") // record 1
	f.deployOK(t, "staging") // record 2, same image (a redeploy)
	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging", To: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("rollback to unknown commit: %v", err)
	}
	hist, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging"}))
	if err != nil || len(hist.Msg.GetRecords()) != 2 || hist.Msg.GetRecords()[0].GetKind() != "deploy" || hist.Msg.GetRecords()[0].GetActor() != "ana@example.com" {
		t.Fatalf("history: %v %v", hist, err)
	}
	first := hist.Msg.GetRecords()[1].GetDeployCommit()

	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || eventSteps(op) != "commit,namespace,sync" {
		t.Fatalf("rollback: %v %s", op.GetError(), eventSteps(op))
	}
	hist, _ = f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging", Limit: 1}))
	if r := hist.Msg.GetRecords(); len(r) != 1 || r[0].GetKind() != "rollback" {
		t.Errorf("after rollback: %v", r)
	}
	data, _ := f.repo.Read(ctx, valuesPath("payments", "staging"), "")
	v, _ := parseValues(data)
	old, _ := f.repo.Read(ctx, valuesPath("payments", "staging"), first)
	ov, _ := parseValues(old)
	if v.Image != ov.Image || v.Forge.Kind != "rollback" || v.Forge.Commit != ov.Forge.Commit {
		t.Errorf("rollback content %+v vs %+v", v, ov)
	}

	pr, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, pr.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("promote: %v", op.GetError())
	}
	data, _ = f.repo.Read(ctx, valuesPath("payments", "production"), "")
	pv, _ := parseValues(data)
	if pv.Image != v.Image || pv.Env != "production" || pv.Host != "payments.production.10.150.1.222.nip.io" || pv.Forge.Kind != "promote" || pv.Port != 9090 {
		t.Errorf("promoted values %+v", pv)
	}
	for _, req := range []*forgev1.PromoteDeployRequest{
		{Service: "services/payments", From: "production", To: "staging"},
		{Service: "services/payments", From: "staging", To: "staging"},
		{Service: "services/payments", To: "qa"},
	} {
		if _, err := f.client.PromoteDeploy(ctx, as("writer", req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("promote %v: %v", req, err)
		}
	}
	if _, err := f.client.RollbackDeploy(ctx, as("reader", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader rollback: %v", err)
	}
	if _, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/production", Limit: 51})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("limit 51: %v", err)
	}
}
```

- [x] **Step 2:** `go test ./internal/server -run TestRollbackAndPromote`. Expected: FAIL (`Unimplemented`).

- [x] **Step 3: Implement**, replacing the three methods in `deploys.go`:

```go
func (d *Deploys) ListDeployHistory(ctx context.Context, req *connect.Request[forgev1.ListDeployHistoryRequest]) (*connect.Response[forgev1.ListDeployHistoryResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	switch {
	case limit == 0:
		limit = 20
	case limit < 0 || limit > 50:
		return nil, invalidArg("limit %d must be 1 to 50", limit)
	}
	recs, err := d.history(ctx, svc, env, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.DeployRecord, 0, len(recs))
	for _, r := range recs {
		pr := &forgev1.DeployRecord{DeployCommit: r.DeployCommit, Kind: r.Kind, Image: r.Image, Ref: r.Ref, Commit: r.Commit, Actor: r.Actor, Operation: r.Operation}
		if t, err := time.Parse(time.RFC3339, r.Time); err == nil {
			pr.Time = timestamppb.New(t)
		}
		out = append(out, pr)
	}
	return connect.NewResponse(&forgev1.ListDeployHistoryResponse{Records: out}), nil
}

// history returns Forge's records of svc/env, newest first.
func (d *Deploys) history(ctx context.Context, svc, env string, limit int) ([]deployRecord, error) {
	commits, err := d.o.Repo.History(ctx, valuesPath(svc, env), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("deploy repo history: %w", err))
	}
	var out []deployRecord
	for _, c := range commits {
		if r, ok := parseRecord(c.SHA, c.Message, c.Time); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *Deploys) RollbackDeploy(ctx context.Context, req *connect.Request[forgev1.RollbackDeployRequest]) (*connect.Response[forgev1.RollbackDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	recs, err := d.history(ctx, svc, env, 50)
	if err != nil {
		return nil, err
	}
	var target *deployRecord
	switch to := req.Msg.GetTo(); {
	case to == "" && len(recs) < 2:
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s/%s has no earlier deploy to roll back to", deployNamePrefix, svc, env))
	case to == "":
		target = &recs[1]
	default:
		for i := range recs {
			if recs[i].DeployCommit == to || (len(to) >= 7 && strings.HasPrefix(recs[i].DeployCommit, to)) {
				target = &recs[i]
				break
			}
		}
		if target == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no deploy %s in the last 50 of %s%s/%s", to, deployNamePrefix, svc, env))
		}
	}
	data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), target.DeployCommit)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read %s: %w", shortSHA(target.DeployCommit), err))
	}
	v, err := parseValues(data)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	v.Forge.Kind, v.Forge.Actor = kindRollback, actorOf(ctx)
	preview := d.deploymentFrom(svc, env, v, "", AppStatus{})
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.RollbackDeployResponse{Deployment: preview}), nil
	}
	op, err := d.startMutation(ctx, kindRollback, svc, env, v)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.RollbackDeployResponse{Operation: op, Deployment: preview}), nil
}

func (d *Deploys) PromoteDeploy(ctx context.Context, req *connect.Request[forgev1.PromoteDeployRequest]) (*connect.Response[forgev1.PromoteDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	from, to := req.Msg.GetFrom(), req.Msg.GetTo()
	if from == "" {
		from = "staging"
	}
	if to == "" {
		to = "production"
	}
	if err := validEnv(from); err != nil {
		return nil, err
	}
	if err := validEnv(to); err != nil {
		return nil, err
	}
	if slices.Index(deployEnvs, to) != slices.Index(deployEnvs, from)+1 {
		return nil, invalidArg("promote goes one step forward: staging to production")
	}
	svc, ok := strings.CutPrefix(req.Msg.GetService(), servicePrefix)
	if !ok || svc == "" {
		return nil, invalidArg("service %q must be services/<name>", req.Msg.GetService())
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	src, _, err := d.current(ctx, svc, from)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has nothing in %s to promote", req.Msg.GetService(), from))
	}
	if err != nil {
		return nil, err
	}
	v := src
	v.Env, v.Host = to, d.host(svc, to)
	v.Forge.Env, v.Forge.Kind, v.Forge.Actor = to, kindPromote, actorOf(ctx)
	preview := d.deploymentFrom(svc, to, v, "", AppStatus{})
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.PromoteDeployResponse{Deployment: preview}), nil
	}
	op, err := d.startMutation(ctx, kindPromote, svc, to, v)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.PromoteDeployResponse{Operation: op, Deployment: preview}), nil
}

// startMutation locks svc/env and starts an operation that applies v.
func (d *Deploys) startMutation(ctx context.Context, kind, svc, env string, v deployValues) (*forgev1.Operation, error) {
	target := svc + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.lock(target, "an operation"); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, kind, deployNamePrefix+target, v.Forge.Actor, func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		return d.apply(ctx, svc, env, v, emit)
	})
	d.running[target] = op.GetName()
	return op, nil
}
```

  `StartDeploy` keeps its own code path because of the resolve/image steps and `request_id`; don't merge them.

- [x] **Step 4:** `go test -race -count=3 ./internal/server`. Expected: PASS.

- [x] **Step 5:** `make build test lint`, then `git add internal/server && git commit -m "server: deploy history, rollback and promote"`.

---

### Task 8: forge-api `--deploys`

**Files:**
- Create: `cmd/forge-api/deploys.go`
- Modify: `cmd/forge-api/main.go`
- Test: `cmd/forge-api/deploys_test.go`

**Interfaces:**
- Consumes: `previews(...)` returning `*server.Environments` (Phase 3), `server.NewDeploys`, `DeployOptions`, `github.RepoFiles`, `github.Client`, the `Cluster` built for previews, and `registry.Client` (rebuilt; see Step 3).
- Produces:
  - `type deployFlags struct { on bool; repo, domain, syncTimeout string }`;
  - `func deploys(f deployFlags, app *github.App, previewDomain string, cluster *server.Cluster, images server.ImageChecker, catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger) (*server.Deploys, error)`.

- [x] **Step 1: Write the failing test** `cmd/forge-api/deploys_test.go`:

```go
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

func TestDeploys(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &github.App{ID: 1, Key: key, Org: "showmino"}
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	log := slog.New(slog.DiscardHandler)
	cat, ops := server.NewCatalog(nil), server.NewOperations(nil)
	ok := deployFlags{on: true, repo: "showmino/forge-deploys", syncTimeout: "10m"}

	if d, err := deploys(deployFlags{}, nil, "", nil, nil, cat, ops, "showmino", log); d != nil || err != nil {
		t.Errorf("off: %v %v", d, err)
	}
	if d, err := deploys(ok, app, "preview.10.150.1.222.nip.io", cluster, &envtest.Images{}, cat, ops, "showmino", log); d == nil || err != nil {
		t.Errorf("on: %v %v", d, err)
	}
	for name, tc := range map[string]struct {
		f       deployFlags
		app     *github.App
		cluster *server.Cluster
		domain  string
		msg     string
	}{
		"no app":      {ok, nil, cluster, "preview.x.nip.io", "GitHub App"},
		"no previews": {ok, app, nil, "", "--previews"},
		"bad repo":    {deployFlags{on: true, repo: "nope", syncTimeout: "10m"}, app, cluster, "preview.x.nip.io", "owner/name"},
		"bad timeout": {deployFlags{on: true, repo: "a/b", syncTimeout: "soon"}, app, cluster, "preview.x.nip.io", "FORGE_DEPLOY_SYNC_TIMEOUT"},
		"bad domain":  {ok, app, cluster, "x.nip.io", "--deploy-domain"},
	} {
		if _, err := deploys(tc.f, tc.app, tc.domain, tc.cluster, &envtest.Images{}, cat, ops, "showmino", log); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want %q", name, err, tc.msg)
		}
	}
	if d, err := deploys(deployFlags{on: true, repo: "a/b", domain: "lan.example", syncTimeout: "1m"}, app, "x.nip.io", cluster, &envtest.Images{}, cat, ops, "showmino", log); d == nil || err != nil {
		t.Errorf("explicit domain: %v %v", d, err)
	}
}
```

- [x] **Step 2:** `go test ./cmd/forge-api -run TestDeploys`. Expected: build failure, `undefined: deployFlags`.

- [x] **Step 3: Implement** `cmd/forge-api/deploys.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

// deployFlags are forge-api's deploy settings.
type deployFlags struct {
	on                          bool
	repo, domain, syncTimeout   string
}

// deploys returns the deploy service, or nil when deploys are off. It reuses
// the previews' cluster and image checker. The default domain drops the
// "preview." label from previewDomain: preview.10.150.1.222.nip.io gives
// <service>.<env>.10.150.1.222.nip.io.
func deploys(f deployFlags, app *github.App, previewDomain string, cluster *server.Cluster, images server.ImageChecker,
	catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger) (*server.Deploys, error) {
	if !f.on {
		return nil, nil
	}
	if app == nil {
		return nil, errors.New("--deploys needs the GitHub App (--github-app-id and --github-app-key-file)")
	}
	if cluster == nil {
		return nil, errors.New("--deploys needs --previews (the cluster connection and image checks)")
	}
	owner, name, ok := strings.Cut(f.repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("--deploy-repo %q must be owner/name", f.repo)
	}
	sync, err := positiveDuration("FORGE_DEPLOY_SYNC_TIMEOUT", f.syncTimeout)
	if err != nil {
		return nil, err
	}
	domain := f.domain
	if domain == "" {
		var found bool
		if domain, found = strings.CutPrefix(previewDomain, "preview."); !found {
			return nil, fmt.Errorf("set --deploy-domain: cannot derive it from preview domain %q", previewDomain)
		}
	}
	gh := &github.Client{Token: app.Token}
	return server.NewDeploys(server.DeployOptions{
		Cluster: cluster, GitHub: gh, Images: images,
		Repo:    &github.RepoFiles{Client: gh, Owner: owner, Name: name, Branch: "main"},
		Catalog: catalog, Operations: ops, Org: org, Domain: domain, SyncTimeout: sync, Log: log,
	}), nil
}
```

  `previews()` builds the `Cluster` and the registry client internally. Change it to also return them:
  - its signature becomes `previews(...) (*server.Environments, *server.Cluster, server.ImageChecker, error)`;
  - update `TestPreviews` to take the extra values (`envs, _, _, err := previews(...)`);
  - in the off case all four are `nil, nil, nil, nil`.

  In `main.go`:
  - **Flags:**

```go
		deploysOn    = flag.Bool("deploys", env("FORGE_DEPLOYS", "") == "1" || env("FORGE_DEPLOYS", "") == "true", "serve deploys to staging and production through Argo CD (needs --previews)")
		deployRepo   = flag.String("deploy-repo", env("FORGE_DEPLOY_REPO", "showmino/forge-deploys"), "GitOps repo (owner/name) Forge commits deploys to")
		deployDomain = flag.String("deploy-domain", env("FORGE_DEPLOY_DOMAIN", ""), "deploy hosts are <service>.<env>.<domain> (default: the preview domain without 'preview.')")
```

  - **Wiring:** after `previews(...)`, call `deploys(deployFlags{on: *deploysOn, repo: *deployRepo, domain: *deployDomain, syncTimeout: env("FORGE_DEPLOY_SYNC_TIMEOUT", "10m")}, app, *previewDomain, cluster, images, catalog, ops, *githubOrg, log)`, log "deploys are off; set --deploys" when nil, and pass `Deploys: deps` in `server.Options`.

- [x] **Step 4:** `go test ./cmd/forge-api && go build ./...`. Expected: PASS.

- [x] **Step 5:** `make build test lint`, then `git add cmd/forge-api && git commit -m "forge-api: --deploys settings"`.

---

### Task 9: `forge deploy`

**Files:**
- Create: `internal/cli/deploy.go`
- Modify: `internal/cli/root.go` (register `deploy` after `env`), `internal/cli/cli_test.go` (harness field `deploys`)
- Test: `internal/cli/deploy_test.go`, plus the golden `internal/cli/testdata/deploy_status_table.golden`

**Interfaces:**
- Consumes: `client.Client.Deploys`; `f.watchOperation`, `opFailure`, `opID`, `printOperation`, `printStarted` (env.go), `sha7` (env.go), `age`, `until`; `f.confirmDestructive`; `f.Manifest()`; `server.NewDeploys`, `DeployOptions`; `envtest.Repo`, `Argo`.
- Produces: `func deployName(svc, env string) string`.

**CLI behaviour (exact strings):**
- The service argument is optional everywhere and defaults to `forge.yaml`'s `service.name`; outside a repo the error is `a service name is required outside a service repo` (exit 2).
- **`forge deploy start [svc] --env E [--ref R] [--dry-run] [--wait=true]`:**
  - `--env` is required (exit 2).
  - Dry run prints `Would deploy <image> (<ref> at <sha7>) to deployments/<svc>/<env> at <url>`.
  - By default it watches, then prints the deployment as `status` does. A failed operation exits 7 with the hint `see 'forge deploy status <svc> --env <env>'; roll back with 'forge deploy rollback <svc> --env <env>'`.
- **`forge deploy status [svc] [--env E]`:** unlike the other subcommands, the service is optional and is not required outside a repo. Without an argument it uses `forge.yaml`'s service when there is one, and otherwise lists every service.
  - Table columns `SERVICE ENV IMAGE SYNC HEALTH DEPLOYED BY URL`. `IMAGE` shows the tag after `:`; `SYNC` and `HEALTH` are lower-case words (`synced`, `healthy`, `out_of_sync`, …); `DEPLOYED` is the relative age.
  - With both a service and `--env`, it prints a field/value table like `env get`, with the fields `name service env image ref commit url sync health message deploy_commit deployed_by deployed operation`; empty fields are omitted.
  - With none, it prints the info `No deployments.`.
- **`forge deploy history [svc] --env E [--limit N]`:** columns `DEPLOY KIND IMAGE COMMIT ACTOR AGE`, where `DEPLOY` is the deploy commit's sha7.
- **`forge deploy rollback [svc] --env E [--to C] [--yes] [--dry-run] [--wait=true]`:**
  - Confirmation through `confirmDestructive("roll back", "deployments/<svc>/<env>", yes)`.
  - Dry run: `Would roll back deployments/<svc>/<env> to <image>`.
  - Success prints the deployment.
- **`forge deploy promote [svc] [--from staging] [--to production] [--yes] [--dry-run] [--wait=true]`:**
  - Confirmation when `--to production`.
  - Dry run: `Would promote <image> from <from> to <to>`.
  - Success prints the deployment.

- [x] **Step 1: Harness.** In `internal/cli/cli_test.go`, add the field `deploys *server.Deploys` and pass `Deploys: h.deploys` in `startAPI`'s Options.

- [x] **Step 2: Write the failing tests** `internal/cli/deploy_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
)

// serveDeploys restarts the harness API with deploys on fakes and Argo
// playing along; unhealthy names "<svc>-<env>" Argo reports Degraded.
func (h *harness) serveDeploys(unhealthy map[string]string) *envtest.Repo {
	h.t.Helper()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	images := &envtest.Images{}
	images.Add("ghcr.io/showmino/payments:1a2b3c4")
	repo := &envtest.Repo{}
	h.deploys = server.NewDeploys(server.DeployOptions{
		Cluster: cluster, Images: images, Repo: repo, Catalog: h.catalog, Operations: h.ops,
		GitHub: &envtest.GitHub{Refs: map[string]string{"main": cliSHA}},
		Org:    "showmino", Domain: "10.150.1.222.nip.io",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, SyncTimeout: 300 * time.Millisecond, NamespaceTimeout: 5 * time.Second,
	})
	h.startAPI(h.authn)
	argo := &envtest.Argo{Kube: cluster.Kube, Dynamic: cluster.Dynamic, Repo: repo, Namespace: "argocd", Unhealthy: unhealthy}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { argo.Run(ctx) })
	h.t.Cleanup(func() { cancel(); wg.Wait() })
	return repo
}

func TestDeployLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	h.fails(2, "deploy", "start", "payments")             // --env required
	h.fails(2, "deploy", "start", "payments", "--env", "qa")
	h.fails(2, "deploy", "start", "--env", "staging")      // no service outside a repo
	h.fails(3, "deploy", "status", "payments", "--env", "staging")

	if r := h.ok("deploy", "start", "payments", "--env", "staging", "--dry-run"); r.out != "Would deploy ghcr.io/showmino/payments:1a2b3c4 (main at 1a2b3c4) to deployments/payments/staging at http://payments.staging.10.150.1.222.nip.io\n" {
		t.Errorf("dry run: %q", r.out)
	}
	r := h.ok("deploy", "start", "payments", "--env", "staging")
	for _, step := range []string{"resolve", "image", "commit", "namespace", "sync"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	if !strings.Contains(r.out, "health          healthy") {
		t.Errorf("printed deployment:\n%s", r.out)
	}
	h.ok("deploy", "start", "payments", "--env", "staging")

	status := h.ok("deploy", "status").out
	golden(t, "deploy_status_table", status[:strings.Index(status, "ana@example.com")]) // the age varies after it
	hist := h.ok("deploy", "history", "payments", "--env", "staging").out
	if lines := strings.Split(strings.TrimSpace(hist), "\n"); len(lines) != 3 || !strings.Contains(lines[1], "deploy") || !strings.Contains(lines[1], "1a2b3c4") {
		t.Errorf("history:\n%s", hist)
	}

	h.fails(2, "deploy", "rollback", "payments", "--env", "staging") // no --yes, no terminal
	if r := h.ok("deploy", "rollback", "payments", "--env", "staging", "--dry-run"); r.out != "Would roll back deployments/payments/staging to ghcr.io/showmino/payments:1a2b3c4\n" {
		t.Errorf("rollback dry run: %q", r.out)
	}
	h.ok("deploy", "rollback", "payments", "--env", "staging", "--yes")
	if r := h.ok("deploy", "history", "payments", "--env", "staging", "--limit", "1"); !strings.Contains(r.out, "rollback") {
		t.Errorf("after rollback:\n%s", r.out)
	}

	h.fails(2, "deploy", "promote", "payments")                            // to production: needs --yes
	if r := h.ok("deploy", "promote", "payments", "--dry-run"); r.out != "Would promote ghcr.io/showmino/payments:1a2b3c4 from staging to production\n" {
		t.Errorf("promote dry run: %q", r.out)
	}
	h.ok("deploy", "promote", "payments", "--yes")
	if r := h.ok("deploy", "status", "payments", "--env", "production", "-o", "json"); !strings.Contains(r.out, `"env": "production"`) || !strings.Contains(r.out, `"health": "HEALTH_HEALTHY"`) {
		t.Errorf("production json:\n%s", r.out)
	}
}

func TestDeployFailureAndOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(6, "deploy", "status"); !strings.Contains(r.err, "deploys are not configured") {
		t.Errorf("off: %s", r.err)
	}
	h.serveDeploys(map[string]string{"payments-staging": "ImagePullBackOff"})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	r := h.fails(7, "deploy", "start", "payments", "--env", "staging")
	if !strings.Contains(r.err, "(not_healthy)") || !strings.Contains(r.err, "forge deploy rollback payments --env staging") {
		t.Errorf("not healthy:\n%s", r.err)
	}
	h.ok("init", "--team", "billing", "--name", "payments")
	if r := h.ok("deploy", "start", "--env", "staging", "--dry-run"); !strings.Contains(r.out, "deployments/payments/staging") {
		t.Errorf("service from forge.yaml: %q", r.out)
	}
}
```

  Write `internal/cli/testdata/deploy_status_table.golden` by running `go test ./internal/cli -run TestDeployLifecycle -update`, then read it. It must hold the header `SERVICE   ENV   IMAGE   SYNC   HEALTH   DEPLOYED   BY   URL` (spacing as the table writer makes it) and the first row up to the actor, for example `payments   staging   1a2b3c4   synced   healthy   <1m   `. Check that before keeping it. The history test checks substrings, because its ages vary.

- [x] **Step 3:** `go test ./internal/cli -run 'TestDeploy'`. Expected: FAIL (`unknown command "deploy"`).

- [x] **Step 4: Write `internal/cli/deploy.go`**:

```go
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func deployName(svc, env string) string { return "deployments/" + svc + "/" + env }

// deployService is the optional service argument, or forge.yaml's service.
func (f *Factory) deployService(args []string) (string, error) {
	if len(args) == 1 {
		return strings.TrimPrefix(args[0], "services/"), nil
	}
	m, _, err := f.Manifest()
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", exitcode.Usagef("a service name is required outside a service repo")
	}
	return m.Service.Name, nil
}

func lowerEnum(s fmt.Stringer, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), prefix))
}

func tag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return image
}

func newDeployCmd(f *Factory) *cobra.Command {
	cmd := group("deploy", "Deploy services to staging and production", "deploys")
	cmd.Long = `Deploy a service's CI image to staging or production. Every change is a
commit to Forge's GitOps repo that Argo CD applies; forge watches until Argo
CD reports the new version Synced and Healthy. Inside a service repo the
service name defaults to forge.yaml's.`
	cmd.AddCommand(newDeployStartCmd(f), newDeployStatusCmd(f), newDeployHistoryCmd(f), newDeployRollbackCmd(f), newDeployPromoteCmd(f))
	return cmd
}

func newDeployStartCmd(f *Factory) *cobra.Command {
	var env, ref string
	var dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "start [service] --env staging|production",
		Short: "Deploy a ref of a service to an environment",
		Example: `  forge deploy start payments --env staging
  forge deploy start --env staging --ref v1.4.2    # in a service repo`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return exitcode.Usagef("--env is required (staging or production)")
			}
			svc, err := f.deployService(args)
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Deploys.StartDeploy(ctx, connect.NewRequest(&forgev1.StartDeployRequest{
				Service: "services/" + svc, Env: env, Ref: ref, RequestId: client.NewRequestID(), ValidateOnly: dryRun,
			}))
			if err != nil {
				return err
			}
			d := resp.Msg.GetDeployment()
			if dryRun {
				if format != output.Table {
					return f.printDeployment(format, d)
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would deploy %s (%s at %s) to %s at %s\n", d.GetImage(), d.GetRef(), sha7(d.GetCommit()), d.GetName(), d.GetUrl())
				return err
			}
			return f.finishDeploy(ctx, c, format, resp.Msg.GetOperation(), svc, env, wait)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "staging or production")
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit (default main)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check and resolve without deploying")
	cmd.Flags().BoolVar(&wait, "wait", true, "watch the deploy until it ends")
	return cmd
}

// finishDeploy watches op (unless !wait) and prints the deployment.
func (f *Factory) finishDeploy(ctx context.Context, c *client.Client, format output.Format, op *forgev1.Operation, svc, env string, wait bool) error {
	if !wait {
		return f.printStarted(format, op)
	}
	op, err := f.watchOperation(ctx, c, op.GetName())
	if err != nil {
		return err
	}
	if err := opFailure(op); err != nil {
		return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge deploy status %s --env %s'; roll back with 'forge deploy rollback %s --env %s'", svc, env, svc, env))
	}
	got, err := c.Deploys.GetDeployment(ctx, connect.NewRequest(&forgev1.GetDeploymentRequest{Name: deployName(svc, env)}))
	if err != nil {
		return err
	}
	return f.printDeployment(format, got.Msg.GetDeployment())
}

// printDeployment prints one deployment as a field/value table.
func (f *Factory) printDeployment(format output.Format, d *forgev1.Deployment) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{d.GetName()}, Data: d})
	}
	var rows [][]string
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, []string{k, v})
		}
	}
	add("name", d.GetName())
	add("service", d.GetService())
	add("env", d.GetEnv())
	add("image", d.GetImage())
	add("ref", d.GetRef())
	add("commit", d.GetCommit())
	add("url", d.GetUrl())
	add("sync", lowerEnum(d.GetSync(), "SYNC_STATUS_"))
	add("health", lowerEnum(d.GetHealth(), "HEALTH_"))
	add("message", d.GetMessage())
	add("deploy_commit", d.GetDeployCommit())
	add("deployed_by", d.GetDeployedBy())
	if d.GetDeployTime() != nil {
		add("deployed", d.GetDeployTime().AsTime().UTC().Format(time.RFC3339)+" ("+age(f.Now().Sub(d.GetDeployTime().AsTime()))+" ago)")
	}
	add("operation", d.GetOperation())
	return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
}
```

  Continue in the same file:
  - **`newDeployStatusCmd`:**
    - resolve the service: the argument, else `forge.yaml`'s service if there is a manifest, else empty (all services);
    - with a service and `--env`, call `GetDeployment` and `printDeployment`;
    - otherwise call `ListDeployments` (the service when given, the env when given) and print the table.
    - Table columns `service env image sync health deployed by url`, with values `strings.TrimPrefix(svc,"services/")`, `env`, `tag(image)`, `lowerEnum(sync,"SYNC_STATUS_")`, `lowerEnum(health,"HEALTH_")`, `age(now - deploy_time)`, `deployed_by` and `url`. Names are the deployment names.
    - Empty result in table format: info `No deployments.`; JSON `{"deployments": []}`.
  - **`newDeployHistoryCmd`:** `--env` required, `--limit` int default 20; call `ListDeployHistory{Name: deployName(svc, env), Limit}`. Table columns `deploy kind image commit actor age`, with values `sha7(deploy_commit)`, `kind`, `tag(image)`, `sha7(commit)`, `actor` and `age(now - time)`.
  - **`newDeployRollbackCmd`:** `--env` required, `--to`, `--yes`, `--dry-run`, `--wait`.
    - Confirm with `confirmDestructive("roll back", deployName(svc, env), yes)` unless `--dry-run`.
    - Dry run prints `Would roll back <name> to <image>`.
    - Otherwise `finishDeploy`.
  - **`newDeployPromoteCmd`:** `--from` (default `staging`), `--to` (default `production`), `--yes`, `--dry-run`, `--wait`.
    - When `to == "production"` and not `--dry-run`, confirm with `confirmDestructive("promote to production", "services/"+svc, yes)`.
    - Dry run prints `Would promote <image> from <from> to <to>`.
    - Otherwise `finishDeploy(..., svc, to, wait)`.

  `printStarted`, `sha7`, `age` and `opID` already exist in `internal/cli` (env.go and operation.go).

- [x] **Step 5: Register** `newDeployCmd(f)` in `root.go`'s core list after `newEnvCmd(f)`.

- [x] **Step 6:** `go test -race ./internal/cli`. Expected: PASS.

- [x] **Step 7:** `make build test lint`, then `git add internal/cli && git commit -m "cli: forge deploy start, status, history, rollback and promote"`.

---

### Task 10: Docs

**Files:**
- Modify: `.claude/skills/forge/SKILL.md`, `README.md`, `docs/plans/2026-10-09-phase-4-m1-deploy-engine.md` (tick boxes)

- [x] **Step 1:**
  - **`SKILL.md`:** add the bullet `- **Phase 4 (in progress):** spec docs/specs/2026-10-09-phase-4-deployments-design.md. Milestone 1: deploy engine (DeployService, internal/server/deploy*.go, github.RepoFiles, deploy/argocd/chart with make chart-test, forge deploy) per docs/plans/2026-10-09-phase-4-m1-deploy-engine.md. Then milestone 2 (CI workload identity) and 3 (k3s and the exit gate).` after the Phase 3 backlog.
  - **README:** after the `forge env` paragraph, add a paragraph saying that `forge deploy start <svc> --env staging|production` commits the version to the deploy repo for Argo CD, with `status`, `history`, `rollback` and `promote`, served by forge-api with `--deploys`.
- [x] **Step 2:** `make build test lint chart-test`, then `git add -A .claude README.md docs && git commit -m "docs: Phase 4 milestone 1 (deploy engine)"`. Don't push; the push, PR and merge come after the final review.
