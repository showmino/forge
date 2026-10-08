# Phase 3 Milestone 2: Preview Environments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `forge env create / list / get / extend / destroy` run a catalog service's CI image in its own Kubernetes namespace (`preview-<name>`), reachable at a URL and destroyed when its TTL expires.

**Architecture:**
- **API:** forge-api gains `EnvironmentService` (`internal/server/environments.go`), backed by a small Kubernetes layer (`internal/server/env_cluster.go`, client-go).
- **State:** an environment's state lives on its namespace (labels and annotations), so every read works after a restart.
- **Operations:** create and destroy run as operations (milestone 1's store); a reaper starts destroys for expired namespaces.
- **Image:** the image tag comes from resolving a Git ref with the GitHub App. An OCI registry client (`internal/registry`) checks that CI pushed it.
- **Tests:** everything runs against client-go fakes and in-memory GitHub and registry fakes in `internal/envtest`.

**Tech Stack:** Go 1.27; ConnectRPC; buf v1.47.2; `k8s.io/client-go`, `k8s.io/api` and `k8s.io/apimachinery` **v0.35.9**; Cobra.

**Spec:** `docs/specs/2026-10-08-phase-3-environments-design.md`, sections 2, 3, 4 and 7. Milestone 1 (operations) is merged; read `internal/server/operations.go` and `internal/cli/operation.go` for the store and the shared watch renderer.

## Global Constraints

- **Names:**
  - An env name is a DNS label of at most 40 characters (`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`), globally unique.
  - Resource names are `environments/<name>`; the namespace is `preview-<name>`.
  - The URL is `http://<name>.<FORGE_PREVIEW_DOMAIN>`.
- **TTL:** default 24h, at least 1h, at most 168h, for both create and extend. Extend sets `expire_time = now + ttl`.
- **Ref and limit:** the default ref is `main`. At most `FORGE_PREVIEW_MAX` environments (default 10) exist at once, counting all states and creates still waiting for their image.
- **Image:**
  - It is `ghcr.io/<lowercase owner>/<service>:<sha7>`.
  - The registry is polled every 10s, and the CI runs for the commit are checked on each poll.
  - The wait gives up after 10m (`image_timeout`); a finished CI run that didn't succeed fails at once (`ci_failed`).
- **Run settings:** `run.port` defaults to 8080 and `run.health` to `/healthz`, read from `forge.yaml` at the resolved commit.
- **Namespace labels:** `app.kubernetes.io/managed-by=forge`, `forge.dev/environment=<name>`, `forge.dev/service=<service>`.
- **Namespace annotations:** `forge.dev/ref`, `forge.dev/commit`, `forge.dev/image`, `forge.dev/creator`, `forge.dev/expire-time` (RFC 3339), `forge.dev/state` (`creating|ready|failed|destroying`), `forge.dev/state-message`.
- **Limits per environment:**
  - `ResourceQuota` requests 1 CPU / 1Gi, limits 2 CPU / 2Gi, 10 pods.
  - `LimitRange` default requests 50m / 64Mi, default limits 500m / 512Mi.
- **Deployment and routing:**
  - The Deployment has one replica, runs as non-root with a read-only root filesystem, no privilege escalation and all capabilities dropped.
  - Its probes are HTTP GETs on `run.health:run.port`, and the env var `PORT` is set to `run.port`.
  - The Service maps port 80 to `run.port`. The HTTPRoute's parent is `FORGE_PREVIEW_GATEWAY` (default `forge/forge-previews`).
- **Timeouts:** ready waits at most 5m (`not_ready`; the namespace stays, marked failed); destroy waits at most 3m for the namespace to be gone.
- **Guard:** forge-api reads and changes only namespaces named `preview-<x>` that carry `managed-by=forge` and `forge.dev/environment=<x>`. Any other namespace is "not found".
- **Reaper:** every minute it starts a destroy (creator `forge-reaper`) for each expired environment. A missing or unparsable expiry counts as creation time + 168h. It never starts a second destroy for an environment that already has one running.
- **Errors:**

  | Condition | Code | CLI exit |
  | --- | --- | --- |
  | Bad name, TTL or ref syntax | InvalidArgument | 2 |
  | Unknown service, environment or ref | NotFound | 3 |
  | Environment exists, limit reached, service without a repo in the org, invalid `forge.yaml`, previews off | AlreadyExists / FailedPrecondition | 6 |
  | Operation failed or cancelled | — | 7 |
  | Kubernetes, GitHub or registry unreachable | Unavailable | 1 |

- **Permissions:** reads need `services.read`; create, extend and destroy need `services.write`.
- **Gate:** before each commit that touches Go, run `make build test lint` with `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`. Commit messages end with your session's Co-Authored-By line.

## Review Focus

1. **Cancel while the image is awaited, and cancel after the namespace exists:** the first leaves nothing behind; the second removes the namespace, and the operation ends `cancelled`. Covered by `TestCancelCreate` (Task 6).
2. **Destroy while a create is still running:** the create is cancelled first, and the environment ends up gone, never half-made. Covered by `TestDestroyCancelsRunningCreate` (Task 7).
3. **A namespace named `preview-x` that Forge did not make,** or an unrelated one: get, extend and destroy say "not found" and never touch it. Covered by `TestGuardIgnoresForeignNamespaces` (Task 5) and `TestGuardOnRPCs` (Task 7).
4. **Two creates for the same name at once, or the limit reached by creates still waiting for an image:** the second is refused, and the limit counts pending creates. Covered by `TestCreateRefusesDuplicatesAndLimit` (Task 6).
5. **Reaper after a restart, with an environment whose destroy already runs, or with a malformed expiry:** exactly one destroy, and no expiry lost. Covered by `TestReaper` (Task 7).

---

### Task 0: Branch

- [ ] **Step 1:** `cd /home/mino/code/forge && git branch --show-current`. Expected: `phase-3-m2` (created from `main` after milestone 1 merged). If it says `main`, run `git checkout -b phase-3-m2`.

---

### Task 1: `environment.proto` and the client

**Files:**
- Create: `proto/forge/v1/environment.proto`
- Generated: `gen/forge/v1/environment.pb.go`, `gen/forge/v1/forgev1connect/environment.connect.go`
- Modify: `pkg/client/client.go`

**Interfaces:**
- Produces:
  - `forgev1.Environment`, with nested enum `Environment_State`: `Environment_STATE_UNSPECIFIED`, `Environment_STATE_CREATING`, `Environment_STATE_READY`, `Environment_STATE_FAILED`, `Environment_STATE_DESTROYING`.
  - Request and response types for the five RPCs; `CreateEnvironmentRequest` has `EnvironmentId`, `Service`, `Ref`, `Ttl` (`*durationpb.Duration`), `RequestId` and `ValidateOnly`.
  - `forgev1connect.EnvironmentServiceHandler` and `NewEnvironmentServiceHandler`.
  - `client.Client.Environments forgev1connect.EnvironmentServiceClient`.

- [ ] **Step 1: Write `proto/forge/v1/environment.proto`**

```protobuf
syntax = "proto3";

package forge.v1;

import "forge/v1/operation.proto";
import "google/protobuf/duration.proto";
import "google/protobuf/timestamp.proto";

// EnvironmentService runs a service's CI image in a short-lived preview
// environment: one Kubernetes namespace with a URL and a TTL. Resource names
// follow AIP style: "environments/{environment}".
service EnvironmentService {
  // CreateEnvironment checks the request, resolves the ref and starts an
  // operation that waits for the image, creates the namespace and waits
  // until the service is ready. validate_only returns the environment it
  // would create, without an operation.
  rpc CreateEnvironment(CreateEnvironmentRequest) returns (CreateEnvironmentResponse) {}

  // GetEnvironment returns one environment with its live readiness.
  rpc GetEnvironment(GetEnvironmentRequest) returns (GetEnvironmentResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ListEnvironments returns every environment, by name.
  rpc ListEnvironments(ListEnvironmentsRequest) returns (ListEnvironmentsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ExtendEnvironment sets the expiry to now plus ttl.
  rpc ExtendEnvironment(ExtendEnvironmentRequest) returns (ExtendEnvironmentResponse) {
    option idempotency_level = IDEMPOTENT;
  }

  // DestroyEnvironment starts an operation that removes the environment.
  rpc DestroyEnvironment(DestroyEnvironmentRequest) returns (DestroyEnvironmentResponse) {}
}

// Environment is one preview environment.
message Environment {
  // State is where the environment is in its life.
  enum State {
    STATE_UNSPECIFIED = 0;
    STATE_CREATING = 1;
    STATE_READY = 2;
    STATE_FAILED = 3;
    STATE_DESTROYING = 4;
  }

  // "environments/{environment}".
  string name = 1;
  // "services/{service}".
  string service = 2;
  // The ref as given: a branch, tag or commit.
  string ref = 3;
  // The full commit SHA the ref resolved to.
  string commit = 4;
  // The image it runs, for example "ghcr.io/showmino/payments:1a2b3c4".
  string image = 5;
  // Where it is reached, for example "http://pr-142.preview.10.150.1.222.nip.io".
  string url = 6;
  State state = 7;
  // Why it is not ready or failed, for example "ImagePullBackOff".
  string state_message = 8;
  // The Kubernetes namespace, "preview-{environment}".
  string namespace = 9;
  // Subject of the caller that created it.
  string creator = 10;
  google.protobuf.Timestamp create_time = 11;
  google.protobuf.Timestamp expire_time = 12;
  // The newest operation on it, while forge-api still has it in memory.
  string operation = 13;
}

message CreateEnvironmentRequest {
  // The new environment's name without "environments/": a DNS label of at
  // most 40 characters.
  string environment_id = 1;
  // "services/{service}": a catalog service with a GitHub repo.
  string service = 2;
  // Branch, tag or commit to run; default "main".
  string ref = 3;
  // Lifetime, 1h to 168h; default 24h.
  google.protobuf.Duration ttl = 4;
  // Retries with the same request_id return the same operation.
  string request_id = 5;
  // Check and resolve only; create nothing.
  bool validate_only = 6;
}

message CreateEnvironmentResponse {
  // Unset when validate_only.
  Operation operation = 1;
  // What will be (or, with validate_only, would be) created.
  Environment environment = 2;
}

message GetEnvironmentRequest {
  // "environments/{environment}".
  string name = 1;
}

message GetEnvironmentResponse {
  Environment environment = 1;
}

message ListEnvironmentsRequest {
  // Only environments of this service, "services/{service}".
  string service = 1;
}

message ListEnvironmentsResponse {
  repeated Environment environments = 1;
}

message ExtendEnvironmentRequest {
  // "environments/{environment}".
  string name = 1;
  // New lifetime from now, 1h to 168h.
  google.protobuf.Duration ttl = 2;
}

message ExtendEnvironmentResponse {
  Environment environment = 1;
}

message DestroyEnvironmentRequest {
  // "environments/{environment}".
  string name = 1;
  // Check only; destroy nothing.
  bool validate_only = 2;
}

message DestroyEnvironmentResponse {
  // Unset when validate_only.
  Operation operation = 1;
  Environment environment = 2;
}
```

- [ ] **Step 2: Generate and lint**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && make generate && buf lint && go build ./...`
Expected: no `buf lint` output; `gen/forge/v1/environment.pb.go` and `gen/forge/v1/forgev1connect/environment.connect.go` exist.

- [ ] **Step 3: Client.** In `pkg/client/client.go`, add the field `Environments forgev1connect.EnvironmentServiceClient` to `Client` after `Operations`. In `New`, add `Environments: forgev1connect.NewEnvironmentServiceClient(hc, baseURL, opts...),`, then `gofmt -w pkg/client/client.go`.

- [ ] **Step 4: Gate and commit**

```bash
make build test lint
git add proto/forge/v1/environment.proto gen pkg/client/client.go
git commit -m "proto: EnvironmentService"
```

---

### Task 2: `run:` in `forge.yaml`

**Files:**
- Modify: `internal/manifest/manifest.go`
- Test: `internal/manifest/manifest_test.go` (append)

**Interfaces:**
- Produces:
  - `type Run struct { Port int; Health string }` (YAML `port`, `health`) and `Manifest.Run *Run` (YAML `run`, omitempty).
  - `func (m *Manifest) RunPort() int` (default 8080) and `func (m *Manifest) RunHealth() string` (default `/healthz`).
  - Constants `DefaultRunPort = 8080` and `DefaultRunHealth = "/healthz"`.

- [ ] **Step 1: Write the failing test** (append to `internal/manifest/manifest_test.go`; it already imports `strings` and `testing`, otherwise add them):

```go
func TestRun(t *testing.T) {
	t.Parallel()
	base := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"
	m, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if m.RunPort() != 8080 || m.RunHealth() != "/healthz" {
		t.Errorf("defaults: %d %q", m.RunPort(), m.RunHealth())
	}
	m, err = Parse([]byte(base + "run:\n  port: 9090\n  health: /ready\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.RunPort() != 9090 || m.RunHealth() != "/ready" {
		t.Errorf("set: %d %q", m.RunPort(), m.RunHealth())
	}
	for _, bad := range []string{"run:\n  port: 70000\n", "run:\n  port: -1\n", "run:\n  health: ready\n", "run:\n  nope: 1\n"} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	out, err := (&Manifest{APIVersion: APIVersion, Service: Service{Name: "a", Team: "b"}, Run: &Run{Port: 9090, Health: "/ready"}}).Marshal()
	if err != nil || !strings.Contains(string(out), "run:\n  port: 9090\n  health: /ready\n") {
		t.Errorf("marshal: %s %v", out, err)
	}
}
```

- [ ] **Step 2:** Run `go test ./internal/manifest -run TestRun`. Expected: build failure, `m.RunPort undefined`.

- [ ] **Step 3: Implement.** In `internal/manifest/manifest.go`:
  - Change the package comment's second sentence to: `Phase 1 covers the catalog fields; Phase 3 adds run, how a preview environment runs the service.`
  - Add `Run *Run \`yaml:"run,omitempty"\`` to `Manifest` after `Namespace`, with the comment `// Run says how preview environments run the service.`
  - Add:

```go
// Defaults for a manifest without run settings.
const (
	DefaultRunPort   = 8080
	DefaultRunHealth = "/healthz"
)

// Run says how preview environments run the service.
type Run struct {
	// Port the service listens on for HTTP.
	Port int `yaml:"port,omitempty"`
	// Health is the HTTP path probed for readiness and liveness.
	Health string `yaml:"health,omitempty"`
}

// RunPort returns run.port or DefaultRunPort.
func (m *Manifest) RunPort() int {
	if m.Run != nil && m.Run.Port != 0 {
		return m.Run.Port
	}
	return DefaultRunPort
}

// RunHealth returns run.health or DefaultRunHealth.
func (m *Manifest) RunHealth() string {
	if m.Run != nil && m.Run.Health != "" {
		return m.Run.Health
	}
	return DefaultRunHealth
}
```

  - At the end of `Validate`, before `return nil`:

```go
	if r := m.Run; r != nil {
		if r.Port < 0 || r.Port > 65535 {
			return fmt.Errorf("run.port %d must be 1-65535", r.Port)
		}
		if r.Health != "" && !strings.HasPrefix(r.Health, "/") {
			return fmt.Errorf("run.health %q must be an absolute path such as /healthz", r.Health)
		}
	}
```

  Add `"strings"` to the imports. A negative port is invalid, and 0 means "default". The test's `port: -1` case covers the negative branch.

- [ ] **Step 4:** Run `go test ./internal/manifest ./internal/template ./internal/cli`. Expected: PASS. Template lint and `forge init` use the manifest; nothing else changes.

- [ ] **Step 5:** `make build test lint`, then `git add internal/manifest && git commit -m "manifest: run settings for preview environments"`.

---

### Task 3: GitHub lookups: ref, file and CI runs

**Files:**
- Create: `internal/github/commits.go`
- Test: `internal/github/commits_test.go`

**Interfaces:**
- Consumes: `(*Client).do` and `doJSON`, `repoPath(owner, name string) string`, `ErrNotFound`, `*StatusError` (in `github.go` and `repos.go`).
- Produces:
  - `func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)`: the full SHA. An unknown ref returns an error that satisfies `errors.Is(err, ErrNotFound)`.
  - `func (c *Client) FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)`: `ErrNotFound` when the file is missing.
  - `type WorkflowRun struct { Name, Status, Conclusion, HTMLURL string }` (JSON `name`, `status`, `conclusion`, `html_url`).
  - `func (c *Client) RunsFor(ctx context.Context, owner, repo, sha string) ([]WorkflowRun, error)`: newest first.

- [ ] **Step 1: Write the failing test** `internal/github/commits_test.go`:

```go
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveRefFileAtRunsFor(t *testing.T) {
	t.Parallel()
	const sha = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/payments/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("ref") {
		case "main", "feature/x", sha:
			_, _ = w.Write([]byte(`{"sha":"` + sha + `"}`))
		case "deadbeef":
			http.Error(w, `{"message":"No commit found for SHA: deadbeef"}`, http.StatusUnprocessableEntity)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /repos/showmino/payments/contents/forge.yaml", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			http.NotFound(w, r)
			return
		}
		enc := base64.StdEncoding.EncodeToString([]byte("run:\n  port: 9090\n"))
		_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + enc[:8] + `\n` + enc[8:] + `"}`))
	})
	mux.HandleFunc("GET /repos/showmino/payments/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != sha {
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"name":"ci","status":"completed","conclusion":"failure","html_url":"https://github.com/showmino/payments/actions/runs/1"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	for _, ref := range []string{"main", "feature/x", sha} {
		got, err := c.ResolveRef(ctx, "showmino", "payments", ref)
		if err != nil || got != sha {
			t.Errorf("ResolveRef(%q) = %q, %v", ref, got, err)
		}
	}
	for _, ref := range []string{"nope", "deadbeef"} {
		if _, err := c.ResolveRef(ctx, "showmino", "payments", ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveRef(%q): %v, want ErrNotFound", ref, err)
		}
	}

	data, err := c.FileAt(ctx, "showmino", "payments", "forge.yaml", sha)
	if err != nil || string(data) != "run:\n  port: 9090\n" {
		t.Errorf("FileAt = %q, %v", data, err)
	}
	if _, err := c.FileAt(ctx, "showmino", "payments", "forge.yaml", "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FileAt missing: %v", err)
	}

	runs, err := c.RunsFor(ctx, "showmino", "payments", sha)
	if err != nil || len(runs) != 1 || runs[0].Name != "ci" || runs[0].Conclusion != "failure" || runs[0].HTMLURL == "" {
		t.Errorf("RunsFor = %+v, %v", runs, err)
	}
}
```

- [ ] **Step 2:** `go test ./internal/github -run TestResolveRefFileAtRunsFor`. Expected: build failure, `c.ResolveRef undefined`.

- [ ] **Step 3: Implement** `internal/github/commits.go`:

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
)

// escapeRef escapes a ref for a URL path, keeping the slashes of branch
// names like "feature/x".
func escapeRef(ref string) string {
	return strings.ReplaceAll(url.PathEscape(ref), "%2F", "/")
}

// ResolveRef returns the full SHA of the commit that ref (a branch, tag or
// commit) points to. An unknown ref is ErrNotFound.
func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+escapeRef(ref), nil, &out)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
		// GitHub answers 422 "No commit found for SHA" for unknown SHAs.
		return "", fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("GitHub returned no SHA for %s/%s@%s", owner, repo, ref)
	}
	return out.SHA, nil
}

// FileAt returns the content of path at ref. A missing file is ErrNotFound.
func (c *Client) FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	var out struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	p := repoPath(owner, repo) + "/contents/" + escapeRef(path) + "?ref=" + url.QueryEscape(ref)
	if err := c.doJSON(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("%s at %s: unexpected encoding %q", path, ref, out.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// WorkflowRun is one GitHub Actions run.
type WorkflowRun struct {
	Name string `json:"name"`
	// Status is queued, in_progress, completed, ...
	Status string `json:"status"`
	// Conclusion is success, failure, cancelled, ... once completed.
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// RunsFor returns the Actions runs for commit sha, newest first.
func (c *Client) RunsFor(ctx context.Context, owner, repo, sha string) ([]WorkflowRun, error) {
	var out struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	p := repoPath(owner, repo) + "/actions/runs?per_page=20&head_sha=" + url.QueryEscape(sha)
	if err := c.doJSON(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}
```

  Check `repoPath` in `repos.go`: it must return `/repos/<owner>/<name>` with escaped segments. If it does something else, build the path the same way the existing `RepoExists` does.

- [ ] **Step 4:** `go test ./internal/github`. Expected: PASS.

- [ ] **Step 5:** Update the package comment's second sentence in `github.go` to `App authentication is in app.go; repos.go creates repos and commits.go reads refs, files and CI runs.`. Then `make build test lint` and `git add internal/github && git commit -m "github: resolve refs, read files and CI runs"`.

---

### Task 4: Registry image check

**Files:**
- Create: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

**Interfaces:**
- Produces:
  - `func ParseRef(image string) (Ref, error)`, with `type Ref struct { Host, Repo, Tag string }`.
  - `type Client struct { BaseURL string; HTTP *http.Client; Creds func(ctx context.Context, host string) (user, pass string, err error) }` and `func (c *Client) Exists(ctx context.Context, image string) (bool, error)`.
  - `func CredsFromDockerConfig(data []byte, host string) (user, pass string, err error)`.

  ghcr.io uses the registry token flow. `GET <base>/token?service=<host>&scope=repository:<repo>:pull`, with basic auth, returns `{"token": "..."}`. Then `HEAD <base>/v2/<repo>/manifests/<tag>`, with `Authorization: Bearer <token>`, answers 200 when the image exists and 404 when it doesn't.

- [ ] **Step 1: Write the failing test** `internal/registry/registry_test.go`:

```go
package registry

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	t.Parallel()
	r, err := ParseRef("ghcr.io/showmino/payments:1a2b3c4")
	if err != nil || r != (Ref{Host: "ghcr.io", Repo: "showmino/payments", Tag: "1a2b3c4"}) {
		t.Errorf("ParseRef = %+v, %v", r, err)
	}
	for _, bad := range []string{"payments:1", "ghcr.io/showmino/payments", "ghcr.io/:x", ""} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestCredsFromDockerConfig(t *testing.T) {
	t.Parallel()
	auth := base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	data := []byte(`{"auths":{"ghcr.io":{"auth":"` + auth + `"},"docker.io":{"username":"u","password":"p"}}}`)
	if u, p, err := CredsFromDockerConfig(data, "ghcr.io"); err != nil || u != "bot" || p != "s3cret" {
		t.Errorf("auth: %q %q %v", u, p, err)
	}
	if u, p, err := CredsFromDockerConfig(data, "docker.io"); err != nil || u != "u" || p != "p" {
		t.Errorf("username/password: %q %q %v", u, p, err)
	}
	if _, _, err := CredsFromDockerConfig(data, "quay.io"); err == nil {
		t.Error("missing host accepted")
	}
}

func TestExists(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "bot" || p != "s3cret" || r.URL.Query().Get("scope") != "repository:showmino/payments:pull" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"t0k"}`))
	})
	mux.HandleFunc("HEAD /v2/showmino/payments/manifests/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" || !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PathValue("tag") == "1a2b3c4" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Creds: func(_ context.Context, host string) (string, string, error) {
		if host != "ghcr.io" {
			t.Errorf("creds for %q", host)
		}
		return "bot", "s3cret", nil
	}}
	ctx := context.Background()
	if ok, err := c.Exists(ctx, "ghcr.io/showmino/payments:1a2b3c4"); err != nil || !ok {
		t.Errorf("present: %v %v", ok, err)
	}
	if ok, err := c.Exists(ctx, "ghcr.io/showmino/payments:0000000"); err != nil || ok {
		t.Errorf("absent: %v %v", ok, err)
	}
	bad := &Client{BaseURL: srv.URL, Creds: func(context.Context, string) (string, string, error) { return "bot", "wrong", nil }}
	if _, err := bad.Exists(ctx, "ghcr.io/showmino/payments:1a2b3c4"); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad creds: %v (must fail without echoing the password)", err)
	}
}
```

- [ ] **Step 2:** `go test ./internal/registry`. Expected: build failure (no package).

- [ ] **Step 3: Implement** `internal/registry/registry.go`:

```go
// Package registry checks whether an image exists in an OCI registry that
// uses the registry token flow, such as ghcr.io. Errors never contain
// credentials or tokens.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Ref is a parsed image reference "host/repo:tag".
type Ref struct{ Host, Repo, Tag string }

// ParseRef parses "host/owner/name:tag". A host and a tag are required.
func ParseRef(image string) (Ref, error) {
	host, rest, ok := strings.Cut(image, "/")
	if !ok || !strings.Contains(host, ".") {
		return Ref{}, fmt.Errorf("image %q must be host/repo:tag", image)
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return Ref{}, fmt.Errorf("image %q must be host/repo:tag", image)
	}
	return Ref{Host: host, Repo: rest[:i], Tag: rest[i+1:]}, nil
}

// manifestTypes are the manifest media types Exists accepts.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Client checks images in a registry.
type Client struct {
	// BaseURL overrides "https://<host>" (tests).
	BaseURL string
	// HTTP defaults to a client with a 30s timeout.
	HTTP *http.Client
	// Creds returns the registry login for host.
	Creds func(ctx context.Context, host string) (user, pass string, err error)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Exists reports whether image (host/repo:tag) is in its registry.
func (c *Client) Exists(ctx context.Context, image string) (bool, error) {
	r, err := ParseRef(image)
	if err != nil {
		return false, err
	}
	base := c.BaseURL
	if base == "" {
		base = "https://" + r.Host
	}
	user, pass, err := c.Creds(ctx, r.Host)
	if err != nil {
		return false, fmt.Errorf("registry login for %s: %w", r.Host, err)
	}
	token, err := c.token(ctx, base, r, user, pass)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/v2/"+r.Repo+"/manifests/"+url.PathEscape(r.Tag), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", manifestTypes)
	resp, err := c.http().Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s manifest %s: %s", r.Repo, r.Tag, resp.Status)
	}
}

func (c *Client) token(ctx context.Context, base string, r Ref, user, pass string) (string, error) {
	q := url.Values{"service": {r.Host}, "scope": {"repository:" + r.Repo + ":pull"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/token?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, pass)
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("registry token for %s: %s", r.Repo, resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("registry token for %s: unreadable answer", r.Repo)
	}
	return out.Token, nil
}

// CredsFromDockerConfig returns the login for host from a
// kubernetes.io/dockerconfigjson Secret's .dockerconfigjson.
func CredsFromDockerConfig(data []byte, host string) (string, string, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", errors.New("pull secret is not a docker config")
	}
	a, ok := cfg.Auths[host]
	if !ok {
		return "", "", fmt.Errorf("pull secret has no login for %s", host)
	}
	if a.Username != "" {
		return a.Username, a.Password, nil
	}
	raw, err := base64.StdEncoding.DecodeString(a.Auth)
	if err != nil {
		return "", "", fmt.Errorf("pull secret login for %s is not base64", host)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", fmt.Errorf("pull secret login for %s is not user:password", host)
	}
	return user, pass, nil
}
```

- [ ] **Step 4:** `go test -race ./internal/registry`. Expected: PASS.

- [ ] **Step 5:** `make build test lint`, then `git add internal/registry && git commit -m "registry: check that an image exists"`.

---

### Task 5: Test fakes and the Kubernetes layer

**Files:**
- Modify: `go.mod` and `go.sum` (add `k8s.io/client-go`, `k8s.io/api` and `k8s.io/apimachinery` at v0.35.9)
- Create: `internal/envtest/envtest.go`, `internal/server/env_cluster.go`
- Test: `internal/server/env_cluster_test.go`

**Interfaces:**
- Consumes: `github.WorkflowRun` (Task 3).
- Produces, in package `envtest` (test support; only `_test.go` files import it):
  - `func Kube(objs ...runtime.Object) *fake.Clientset`: a fake clientset that already holds the dockerconfigjson Secret `forge/mino-ghcr`, with login `bot:s3cret` for ghcr.io.
  - `func Dynamic() *dynamicfake.FakeDynamicClient`: knows `httproutes`.
  - `var HTTPRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}`.
  - `type GitHub struct { Refs map[string]string; Files map[string][]byte; Runs map[string][]github.WorkflowRun }`, safe for concurrent use. `Refs` maps ref → SHA, `Files` maps `sha+":"+path` → content, `Runs` maps sha → runs. It implements `ResolveRef`, `FileAt` and `RunsFor` with `github.ErrNotFound` for misses, plus `SetRuns(sha string, runs []github.WorkflowRun)`.
  - `type Images struct{...}` with `func (i *Images) Add(image string)` and `Exists(ctx, image) (bool, error)`.
  - `func MarkAvailable(ctx context.Context, kube kubernetes.Interface, skip func(namespace string) bool)`: until ctx ends, every 5ms, sets `Status.AvailableReplicas = 1` and `ReadyReplicas = 1` on every Deployment not in a skipped namespace.
- Produces, in package `server`:
  - Label and annotation constants, `envPrefix = "preview-"`, `envNamespace(id string) string`, and the state strings `stateCreating`, `stateReady`, `stateFailed` and `stateDestroying`.
  - `errNoEnv` and `errEnvExists`.
  - `type Cluster struct { Kube kubernetes.Interface; Dynamic dynamic.Interface; Gateway, PullSecret string }`.
  - `type Workload struct { Service, Image, Host string; Port int32; Health string }`.
  - Methods:
    - `Env(ctx, id) (*corev1.Namespace, error)`, `Envs(ctx) ([]corev1.Namespace, error)`.
    - `CreateEnv(ctx, id string, labels, annotations map[string]string) error`, `Annotate(ctx, id string, annotations map[string]string) error`.
    - `Deploy(ctx, id string, w Workload) error`, `Status(ctx, id, service string) (ready bool, problem string, err error)`.
    - `DeleteEnv(ctx, id) error`, `EnvGone(ctx, id) (bool, error)`, `PullSecretData(ctx) ([]byte, error)`.

- [ ] **Step 1: Add the dependencies**

```bash
export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
go get k8s.io/client-go@v0.35.9 k8s.io/api@v0.35.9 k8s.io/apimachinery@v0.35.9
```

- [ ] **Step 2: Write `internal/envtest/envtest.go`**

```go
// Package envtest holds fakes for preview environment tests: a Kubernetes
// clientset with forge-api's pull secret, a dynamic client that knows
// HTTPRoutes, GitHub and a registry. Only tests import it.
package envtest

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/showmino/forge/internal/github"
)

// HTTPRoutes is the Gateway API HTTPRoute resource.
var HTTPRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

// PullSecretJSON is the .dockerconfigjson of the fake pull secret forge/mino-ghcr.
var PullSecretJSON = []byte(`{"auths":{"ghcr.io":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("bot:s3cret")) + `"}}}`)

// Kube returns a fake clientset holding objs and the pull secret forge/mino-ghcr.
func Kube(objs ...runtime.Object) *fake.Clientset {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mino-ghcr", Namespace: "forge"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: PullSecretJSON},
	}
	return fake.NewClientset(append([]runtime.Object{secret}, objs...)...)
}

// Dynamic returns a fake dynamic client that knows HTTPRoutes.
func Dynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{HTTPRoutes: "HTTPRouteList"})
}

// GitHub is an in-memory GitHub: refs, files and Actions runs.
type GitHub struct {
	mu sync.Mutex
	// Refs maps a ref to its commit SHA.
	Refs map[string]string
	// Files maps "<sha>:<path>" to content.
	Files map[string][]byte
	// Runs maps a SHA to its runs, newest first.
	Runs map[string][]github.WorkflowRun
}

func (g *GitHub) ResolveRef(_ context.Context, _, _, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha, ok := g.Refs[ref]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("ref %s: %w", ref, github.ErrNotFound)
}

func (g *GitHub) FileAt(_ context.Context, _, _, path, ref string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if data, ok := g.Files[ref+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("%s at %s: %w", path, ref, github.ErrNotFound)
}

func (g *GitHub) RunsFor(_ context.Context, _, _, sha string) ([]github.WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]github.WorkflowRun(nil), g.Runs[sha]...), nil
}

// SetRuns replaces the runs of sha.
func (g *GitHub) SetRuns(sha string, runs []github.WorkflowRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Runs == nil {
		g.Runs = map[string][]github.WorkflowRun{}
	}
	g.Runs[sha] = runs
}

// Images is an in-memory registry.
type Images struct {
	mu      sync.Mutex
	present map[string]bool
}

// Add makes image exist.
func (i *Images) Add(image string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.present == nil {
		i.present = map[string]bool{}
	}
	i.present[image] = true
}

func (i *Images) Exists(_ context.Context, image string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.present[image], nil
}

// MarkAvailable plays the Deployment controller until ctx ends: every
// Deployment outside the namespaces skip selects becomes available.
func MarkAvailable(ctx context.Context, kube kubernetes.Interface, skip func(namespace string) bool) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range list.Items {
			d := &list.Items[i]
			if d.Status.AvailableReplicas > 0 || (skip != nil && skip(d.Namespace)) {
				continue
			}
			d.Status.AvailableReplicas, d.Status.ReadyReplicas = 1, 1
			_, _ = kube.AppsV1().Deployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		}
	}
}
```

- [ ] **Step 3: Write the failing test** `internal/server/env_cluster_test.go`:

```go
package server

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/envtest"
)

func testCluster(objs ...*corev1.Namespace) *Cluster {
	kube := envtest.Kube()
	for _, ns := range objs {
		_, _ = kube.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	}
	return &Cluster{Kube: kube, Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
}

func TestClusterLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster()
	if err := c.CreateEnv(ctx, "pr-1", map[string]string{labelService: "payments"}, map[string]string{annState: stateCreating}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateEnv(ctx, "pr-1", nil, nil); !errors.Is(err, errEnvExists) {
		t.Errorf("second create: %v", err)
	}
	ns, err := c.Env(ctx, "pr-1")
	if err != nil || ns.Name != "preview-pr-1" || ns.Labels[labelManagedBy] != "forge" || ns.Labels[labelEnvironment] != "pr-1" || ns.Labels[labelService] != "payments" {
		t.Fatalf("env: %v %v", ns, err)
	}
	if err := c.Annotate(ctx, "pr-1", map[string]string{annState: stateReady}); err != nil {
		t.Fatal(err)
	}
	if ns, _ := c.Env(ctx, "pr-1"); ns.Annotations[annState] != stateReady {
		t.Errorf("annotate: %v", ns.Annotations)
	}

	w := Workload{Service: "payments", Image: "ghcr.io/showmino/payments:1a2b3c4", Host: "pr-1.preview.example", Port: 9090, Health: "/ready"}
	if err := c.Deploy(ctx, "pr-1", w); err != nil {
		t.Fatal(err)
	}
	d, err := c.Kube.AppsV1().Deployments("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := d.Spec.Template.Spec
	ctr := pod.Containers[0]
	switch {
	case *d.Spec.Replicas != 1:
		t.Error("replicas")
	case ctr.Image != w.Image || ctr.Ports[0].ContainerPort != 9090:
		t.Errorf("container: %+v", ctr)
	case ctr.ReadinessProbe.HTTPGet.Path != "/ready" || ctr.LivenessProbe.HTTPGet.Path != "/ready" || ctr.ReadinessProbe.HTTPGet.Port.IntValue() != 9090:
		t.Errorf("probes: %+v", ctr.ReadinessProbe)
	case len(ctr.Env) != 1 || ctr.Env[0].Name != "PORT" || ctr.Env[0].Value != "9090":
		t.Errorf("env: %+v", ctr.Env)
	case pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot:
		t.Error("runAsNonRoot")
	case ctr.SecurityContext == nil || !*ctr.SecurityContext.ReadOnlyRootFilesystem || *ctr.SecurityContext.AllowPrivilegeEscalation || ctr.SecurityContext.Capabilities.Drop[0] != "ALL":
		t.Errorf("container security: %+v", ctr.SecurityContext)
	case len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "forge-pull":
		t.Errorf("pull secrets: %+v", pod.ImagePullSecrets)
	}
	s, err := c.Kube.CoreV1().Secrets("preview-pr-1").Get(ctx, "forge-pull", metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeDockerConfigJson || string(s.Data[corev1.DockerConfigJsonKey]) != string(envtest.PullSecretJSON) {
		t.Errorf("pull secret copy: %v %v", s, err)
	}
	svc, err := c.Kube.CoreV1().Services("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 9090 {
		t.Errorf("service: %v %v", svc, err)
	}
	q, err := c.Kube.CoreV1().ResourceQuotas("preview-pr-1").Get(ctx, "forge", metav1.GetOptions{})
	if err != nil || q.Spec.Hard.Pods().Value() != 10 || q.Spec.Hard.Name("limits.memory", "").String() != "2Gi" {
		t.Errorf("quota: %v %v", q, err)
	}
	lr, err := c.Kube.CoreV1().LimitRanges("preview-pr-1").Get(ctx, "forge", metav1.GetOptions{})
	if err != nil || lr.Spec.Limits[0].Default.Memory().String() != "512Mi" || lr.Spec.Limits[0].DefaultRequest.Cpu().String() != "50m" {
		t.Errorf("limit range: %v %v", lr, err)
	}
	route, err := c.Dynamic.Resource(envtest.HTTPRoutes).Namespace("preview-pr-1").Get(ctx, "payments", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hosts, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(hosts) != 1 || hosts[0] != "pr-1.preview.example" || len(parents) != 1 ||
		parents[0].(map[string]any)["name"] != "forge-previews" || parents[0].(map[string]any)["namespace"] != "forge" {
		t.Errorf("route: %v", route.Object)
	}

	if ready, problem, err := c.Status(ctx, "pr-1", "payments"); err != nil || ready || problem != "0/1 pods ready" {
		t.Errorf("status before ready: %v %q %v", ready, problem, err)
	}
	_, _ = c.Kube.CoreV1().Pods("preview-pr-1").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}},
		}}},
	}, metav1.CreateOptions{})
	if _, problem, _ := c.Status(ctx, "pr-1", "payments"); problem != "ImagePullBackOff: pull access denied" {
		t.Errorf("pod problem: %q", problem)
	}
	d.Status.AvailableReplicas = 1
	_, _ = c.Kube.AppsV1().Deployments("preview-pr-1").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	if ready, _, err := c.Status(ctx, "pr-1", "payments"); !ready || err != nil {
		t.Errorf("status ready: %v %v", ready, err)
	}

	envs, err := c.Envs(ctx)
	if err != nil || len(envs) != 1 {
		t.Errorf("envs: %v %v", envs, err)
	}
	if err := c.DeleteEnv(ctx, "pr-1"); err != nil {
		t.Fatal(err)
	}
	if gone, err := c.EnvGone(ctx, "pr-1"); !gone || err != nil {
		t.Errorf("gone: %v %v", gone, err)
	}
	if _, err := c.Env(ctx, "pr-1"); !errors.Is(err, errNoEnv) {
		t.Errorf("after delete: %v", err)
	}
}

func TestGuardIgnoresForeignNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-x"}}, // not labelled by Forge
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-y", Labels: map[string]string{labelManagedBy: "forge", labelEnvironment: "other"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "finance", Labels: map[string]string{labelManagedBy: "forge", labelEnvironment: "finance"}}},
	)
	for _, id := range []string{"x", "y"} {
		if _, err := c.Env(ctx, id); !errors.Is(err, errNoEnv) {
			t.Errorf("Env(%s): %v", id, err)
		}
		if err := c.Annotate(ctx, id, map[string]string{annState: stateReady}); !errors.Is(err, errNoEnv) {
			t.Errorf("Annotate(%s): %v", id, err)
		}
		if err := c.DeleteEnv(ctx, id); !errors.Is(err, errNoEnv) {
			t.Errorf("DeleteEnv(%s): %v", id, err)
		}
		if err := c.Deploy(ctx, id, Workload{Service: "a", Image: "ghcr.io/a/a:1", Host: "h", Port: 80, Health: "/"}); !errors.Is(err, errNoEnv) {
			t.Errorf("Deploy(%s): %v", id, err)
		}
	}
	if envs, _ := c.Envs(ctx); len(envs) != 0 {
		t.Errorf("Envs listed foreign namespaces: %v", envs)
	}
	for _, name := range []string{"preview-x", "preview-y", "finance"} {
		if _, err := c.Kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s was touched: %v", name, err)
		}
	}
}
```

- [ ] **Step 4:** `go test ./internal/server -run 'TestClusterLifecycle|TestGuardIgnores'`. Expected: build failure, `undefined: Cluster`.

- [ ] **Step 5: Implement** `internal/server/env_cluster.go`:

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Labels and annotations on an environment's namespace. The namespace is
// the environment's only record, so it survives forge-api restarts.
const (
	envPrefix        = "preview-"
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelEnvironment = "forge.dev/environment"
	labelService     = "forge.dev/service"
	annRef           = "forge.dev/ref"
	annCommit        = "forge.dev/commit"
	annImage         = "forge.dev/image"
	annCreator       = "forge.dev/creator"
	annExpire        = "forge.dev/expire-time"
	annState         = "forge.dev/state"
	annStateMessage  = "forge.dev/state-message"

	stateCreating   = "creating"
	stateReady      = "ready"
	stateFailed     = "failed"
	stateDestroying = "destroying"

	pullSecretName = "forge-pull"
)

var httpRoutes = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

var (
	errNoEnv     = errors.New("no such environment")
	errEnvExists = errors.New("environment already exists")
)

func envNamespace(id string) string { return envPrefix + id }

// Cluster is where preview environments live. Every method goes through the
// guard: it reads and changes only namespaces Forge made for an environment.
type Cluster struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Gateway is the "namespace/name" of the Gateway that routes previews.
	Gateway string
	// PullSecret is the "namespace/name" of a dockerconfigjson Secret
	// copied into each environment and used to check images.
	PullSecret string
}

// Workload is what an environment runs.
type Workload struct {
	Service, Image, Host string
	Port                 int32
	Health               string
}

// owned is the guard: a namespace is an environment's only if Forge made it.
func owned(ns *corev1.Namespace) bool {
	id, ok := strings.CutPrefix(ns.Name, envPrefix)
	return ok && ns.Labels[labelManagedBy] == "forge" && ns.Labels[labelEnvironment] == id
}

// Env returns the namespace of environment id, or errNoEnv.
func (c *Cluster) Env(ctx context.Context, id string) (*corev1.Namespace, error) {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, envNamespace(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errNoEnv
	}
	if err != nil {
		return nil, err
	}
	if !owned(ns) {
		return nil, errNoEnv
	}
	return ns, nil
}

// Envs returns every environment's namespace.
func (c *Cluster) Envs(ctx context.Context) ([]corev1.Namespace, error) {
	list, err := c.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=forge"})
	if err != nil {
		return nil, err
	}
	var out []corev1.Namespace
	for i := range list.Items {
		if owned(&list.Items[i]) {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// CreateEnv creates environment id's namespace, or returns errEnvExists.
func (c *Cluster) CreateEnv(ctx context.Context, id string, labels, annotations map[string]string) error {
	l := map[string]string{labelManagedBy: "forge", labelEnvironment: id}
	for k, v := range labels {
		l[k] = v
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNamespace(id), Labels: l, Annotations: annotations}}
	_, err := c.Kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errEnvExists
	}
	return err
}

// Annotate merges annotations into environment id's namespace.
func (c *Cluster) Annotate(ctx context.Context, id string, annotations map[string]string) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	_, err = c.Kube.CoreV1().Namespaces().Patch(ctx, envNamespace(id), types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return errNoEnv
	}
	return err
}

// DeleteEnv starts deleting environment id's namespace.
func (c *Cluster) DeleteEnv(ctx context.Context, id string) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	err := c.Kube.CoreV1().Namespaces().Delete(ctx, envNamespace(id), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// EnvGone reports whether environment id's namespace no longer exists.
func (c *Cluster) EnvGone(ctx context.Context, id string) (bool, error) {
	_, err := c.Kube.CoreV1().Namespaces().Get(ctx, envNamespace(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

// PullSecretData returns the .dockerconfigjson of the configured pull secret.
func (c *Cluster) PullSecretData(ctx context.Context) ([]byte, error) {
	ns, name, ok := strings.Cut(c.PullSecret, "/")
	if !ok {
		return nil, fmt.Errorf("pull secret %q must be namespace/name", c.PullSecret)
	}
	s, err := c.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pull secret %s: %w", c.PullSecret, err)
	}
	data, ok := s.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("pull secret %s has no %s", c.PullSecret, corev1.DockerConfigJsonKey)
	}
	return data, nil
}

// Deploy creates what environment id runs: the pull secret copy, quota and
// limits, the Deployment, its Service and an HTTPRoute for w.Host. The
// namespace is new, so every object is created once.
func (c *Cluster) Deploy(ctx context.Context, id string, w Workload) error {
	if _, err := c.Env(ctx, id); err != nil {
		return err
	}
	ns := envNamespace(id)
	pull, err := c.PullSecretData(ctx)
	if err != nil {
		return err
	}
	core := c.Kube.CoreV1()
	meta := metav1.ObjectMeta{Name: "forge"}
	if _, err := core.Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: pull},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("pull secret: %w", err)
	}
	if _, err := core.ResourceQuotas(ns).Create(ctx, &corev1.ResourceQuota{ObjectMeta: meta, Spec: corev1.ResourceQuotaSpec{
		Hard: corev1.ResourceList{
			"requests.cpu": resource.MustParse("1"), "requests.memory": resource.MustParse("1Gi"),
			"limits.cpu": resource.MustParse("2"), "limits.memory": resource.MustParse("2Gi"),
			corev1.ResourcePods: resource.MustParse("10"),
		},
	}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("resource quota: %w", err)
	}
	if _, err := core.LimitRanges(ns).Create(ctx, &corev1.LimitRange{ObjectMeta: meta, Spec: corev1.LimitRangeSpec{
		Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
		}},
	}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("limit range: %w", err)
	}
	if _, err := c.Kube.AppsV1().Deployments(ns).Create(ctx, deployment(w), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if _, err := core.Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: w.Service},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": w.Service},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(w.Port)}},
		},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	gwNS, gwName, ok := strings.Cut(c.Gateway, "/")
	if !ok {
		return fmt.Errorf("gateway %q must be namespace/name", c.Gateway)
	}
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": w.Service, "namespace": ns},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": gwName, "namespace": gwNS}},
			"hostnames":  []any{w.Host},
			"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"name": w.Service, "port": int64(80)}}}},
		},
	}}
	if _, err := c.Dynamic.Resource(httpRoutes).Namespace(ns).Create(ctx, route, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("route: %w", err)
	}
	return nil
}

func deployment(w Workload) *appsv1.Deployment {
	yes, no := true, false
	one := int32(1)
	labels := map[string]string{"app": w.Service}
	probe := func(delay int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: w.Health, Port: intstr.FromInt32(w.Port)}},
			InitialDelaySeconds: delay,
			PeriodSeconds:       5,
		}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: w.Service, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: pullSecretName}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &yes,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:           w.Service,
						Image:          w.Image,
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: w.Port}},
						Env:            []corev1.EnvVar{{Name: "PORT", Value: strconv.Itoa(int(w.Port))}},
						ReadinessProbe: probe(0),
						LivenessProbe:  probe(10),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

// Status reports whether the service's Deployment in environment id is
// available and, when it is not, the most telling problem seen.
func (c *Cluster) Status(ctx context.Context, id, service string) (bool, string, error) {
	ns := envNamespace(id)
	d, err := c.Kube.AppsV1().Deployments(ns).Get(ctx, service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "deployment " + service + " is missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if d.Status.AvailableReplicas >= 1 {
		return true, "", nil
	}
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + service})
	if err != nil {
		return false, "", err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				return false, clip(w.Reason + ": " + w.Message), nil
			}
			if cs.RestartCount > 0 {
				return false, fmt.Sprintf("container restarted %d times", cs.RestartCount), nil
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				return false, clip("not scheduled: " + cond.Message), nil
			}
		}
	}
	return false, fmt.Sprintf("%d/1 pods ready", d.Status.ReadyReplicas), nil
}

// clip keeps a problem message short enough for an annotation and a table.
func clip(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
```

`Status`'s `clip("ImagePullBackOff: pull access denied")` returns the text unchanged. When a waiting reason has no message, the result would end in `": "`; `TrimSuffix(TrimSpace(...), ":")` removes it, so the output is `ImagePullBackOff`.

- [ ] **Step 6:** `go mod tidy && go test -race ./internal/server -run 'TestClusterLifecycle|TestGuardIgnores'`. Expected: PASS.

- [ ] **Step 7:** `make build test lint`. If golangci-lint or govulncheck (CI) flags the new k8s modules, report it rather than changing versions. Then `git add go.mod go.sum internal/envtest internal/server/env_cluster.go internal/server/env_cluster_test.go && git commit -m "server: Kubernetes layer for preview environments"`.

---

### Task 6: `EnvironmentService`, part 1: create

**Files:**
- Create: `internal/server/environments.go`, `internal/server/env_create.go`
- Modify: `internal/server/catalog.go` (add `get`), `internal/server/server.go` (`Options.Environments` and handler registration)
- Test: `internal/server/environments_test.go`

**Interfaces:**
- Consumes:
  - Task 5's `Cluster`, constants, `errNoEnv` and `errEnvExists`; `envtest.*`.
  - `Operations.Start`, `List`, `Cancel` and `Wait`; `OpError`.
  - `require`, `PermServicesRead` and `PermServicesWrite`; `shortSHA(sha string) string` (in `create.go`); `servicePrefix` (in `catalog.go`).
  - `manifest.Parse`, `RunPort` and `RunHealth`; `github.ErrNotFound` and `github.WorkflowRun`.
- Produces:
  - `type EnvGitHub interface { ResolveRef; FileAt; RunsFor }` (the `*github.Client` methods from Task 3) and `type ImageChecker interface { Exists(ctx, image string) (bool, error) }`.
  - `type EnvOptions struct { Cluster *Cluster; GitHub EnvGitHub; Images ImageChecker; Catalog *Catalog; Operations *Operations; Org, Domain string; Max int; ImageTimeout, ReadyTimeout, DestroyTimeout, ImagePoll, Poll time.Duration; Now func() time.Time; Log *slog.Logger }`.
  - `func NewEnvironments(o EnvOptions) *Environments`, which applies defaults (Max 10; timeouts 10m, 5m, 3m; ImagePoll 10s; Poll 2s; Now `time.Now`; Log discard).
  - `Options.Environments *Environments`. A nil `*Environments` answers `FailedPrecondition` "previews are not configured on this Forge API".
  - Operation kinds `kindCreateEnv = "create_environment"` and `kindDestroyEnv = "destroy_environment"`; `envNamePrefix = "environments/"`.
  - `(*Environments).envFromNamespace(ns *corev1.Namespace) *forgev1.Environment`, `(*Environments).url(id string) string`, `(*Environments).removeNamespace(ctx, id string) error`.
  - `(*Catalog).get(name string) (*forgev1.Service, bool)`.
- Get, List, Extend and Destroy return `CodeUnimplemented` in this task; Task 7 replaces them.

- [ ] **Step 1: Write the failing tests** `internal/server/environments_test.go`:

```go
package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"google.golang.org/protobuf/types/known/durationpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
)

const envSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// envFixture is a forge-api with previews on fakes: service "payments"
// (repo showmino/payments, forge.yaml with run port 9090 and health /ready),
// ref main at envSHA, and its image present unless noImage.
type envFixture struct {
	cluster *Cluster
	gh      *envtest.GitHub
	images  *envtest.Images
	ops     *Operations
	envs    *Environments
	client  forgev1connect.EnvironmentServiceClient
	clock   *clock
	image   string
}

func newEnvFixture(t *testing.T, noImage bool, tweak func(*EnvOptions)) *envFixture {
	t.Helper()
	f := &envFixture{
		cluster: &Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"},
		gh: &envtest.GitHub{
			Refs:  map[string]string{"main": envSHA},
			Files: map[string][]byte{envSHA + ":forge.yaml": []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  health: /ready\n")},
		},
		images: &envtest.Images{},
		ops:    NewOperations(nil),
		clock:  newClock(),
		image:  "ghcr.io/showmino/payments:1a2b3c4",
	}
	if !noImage {
		f.images.Add(f.image)
	}
	catalog := NewCatalog(nil)
	_, err := catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "norepo", Service: &forgev1.Service{OwnerTeam: "billing"},
	}))
	_, _ = catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "elsewhere", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/acme/elsewhere"},
	}))
	o := EnvOptions{
		Cluster: f.cluster, GitHub: f.gh, Images: f.images, Catalog: catalog, Operations: f.ops,
		Org: "showmino", Domain: "preview.example",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, ReadyTimeout: 5 * time.Second, DestroyTimeout: 5 * time.Second,
		Now: f.clock.now,
	}
	if tweak != nil {
		tweak(&o)
	}
	f.envs = NewEnvironments(o)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer:   Authorizer{WriterGroups: []string{"platform"}},
		Catalog:      catalog,
		Operations:   f.ops,
		Environments: f.envs,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewEnvironmentServiceClient(srv.Client(), srv.URL)
	return f
}

// available plays the Deployment controller for the test's lifetime.
func (f *envFixture) available(t *testing.T, skip func(string) bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { envtest.MarkAvailable(ctx, f.cluster.Kube, skip) })
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func (f *envFixture) create(t *testing.T, req *forgev1.CreateEnvironmentRequest) (*forgev1.CreateEnvironmentResponse, error) {
	t.Helper()
	if req.Service == "" {
		req.Service = "services/payments"
	}
	resp, err := f.client.CreateEnvironment(context.Background(), as("writer", req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f *envFixture) wait(t *testing.T, op *forgev1.Operation) *forgev1.Operation {
	t.Helper()
	return wait(t, f.ops, op.GetName())
}

func eventSteps(op *forgev1.Operation) string {
	var steps []string
	for _, ev := range op.GetEvents() {
		if len(steps) == 0 || steps[len(steps)-1] != ev.GetStep() {
			steps = append(steps, ev.GetStep())
		}
	}
	return strings.Join(steps, ",")
}

func TestCreateEnvironmentReady(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-1", Ttl: durationpb.New(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	env := resp.GetEnvironment()
	if env.GetName() != "environments/pr-1" || env.GetCommit() != envSHA || env.GetImage() != f.image || env.GetUrl() != "http://pr-1.preview.example" {
		t.Errorf("response environment: %v", env)
	}
	op := f.wait(t, resp.GetOperation())
	if op.GetError() != nil {
		t.Fatalf("operation failed: %v", op.GetError())
	}
	if got := eventSteps(op); got != "resolve,image,namespace,apply,ready" {
		t.Errorf("steps %s", got)
	}
	ns, err := f.cluster.Env(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	a := ns.Annotations
	wantExpire := f.clock.now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if a[annState] != stateReady || a[annRef] != "main" || a[annCommit] != envSHA || a[annImage] != f.image || a[annCreator] != "ana" || a[annExpire] != wantExpire {
		t.Errorf("annotations: %v", a)
	}
	if ns.Labels[labelService] != "payments" {
		t.Errorf("labels: %v", ns.Labels)
	}
	d, err := f.cluster.Kube.AppsV1().Deployments("preview-pr-1").Get(context.Background(), "payments", metav1.GetOptions{})
	if err != nil || d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != 9090 || d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Errorf("deployment from forge.yaml run: %v %v", d, err)
	}
}

func TestCreateEnvironmentDefaultsWithoutForgeYAML(t *testing.T) {
	t.Parallel()
	// Swap GitHub through the options, never by writing a fake the running
	// server reads: the race detector sees no ordering through loopback TCP.
	f := newEnvFixture(t, false, func(o *EnvOptions) { o.GitHub = &envtest.GitHub{Refs: map[string]string{"main": envSHA}} })
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-2"})
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	d, _ := f.cluster.Kube.AppsV1().Deployments("preview-pr-2").Get(context.Background(), "payments", metav1.GetOptions{})
	if c := d.Spec.Template.Spec.Containers[0]; c.Ports[0].ContainerPort != 8080 || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("defaults: %+v", c)
	}
	ns, _ := f.cluster.Env(context.Background(), "pr-2")
	if ns.Annotations[annExpire] != f.clock.now().Add(24*time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("default ttl: %v", ns.Annotations[annExpire])
	}
}

func TestCreateEnvironmentValidation(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	for _, tc := range []struct {
		name string
		req  *forgev1.CreateEnvironmentRequest
		code connect.Code
	}{
		{"bad id", &forgev1.CreateEnvironmentRequest{EnvironmentId: "PR_1"}, connect.CodeInvalidArgument},
		{"41 chars", &forgev1.CreateEnvironmentRequest{EnvironmentId: strings.Repeat("a", 41)}, connect.CodeInvalidArgument},
		{"ttl too short", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ttl: durationpb.New(30 * time.Minute)}, connect.CodeInvalidArgument},
		{"ttl too long", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ttl: durationpb.New(169 * time.Hour)}, connect.CodeInvalidArgument},
		{"bad ref", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ref: "a b"}, connect.CodeInvalidArgument},
		{"bad service name", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "payments"}, connect.CodeInvalidArgument},
		{"unknown service", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/nope"}, connect.CodeNotFound},
		{"no repo", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/norepo"}, connect.CodeFailedPrecondition},
		{"repo in another org", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/elsewhere"}, connect.CodeFailedPrecondition},
		{"unknown ref", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ref: "nope"}, connect.CodeNotFound},
	} {
		if _, err := f.create(t, tc.req); connect.CodeOf(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.code)
		}
	}
	bad := newEnvFixture(t, false, func(o *EnvOptions) {
		o.GitHub = &envtest.GitHub{Refs: map[string]string{"main": envSHA}, Files: map[string][]byte{envSHA + ":forge.yaml": []byte("run: [")}}
	})
	if _, err := bad.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("invalid forge.yaml: %v", err)
	}
	if _, err := f.client.CreateEnvironment(context.Background(), as("reader", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/payments"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader: %v", err)
	}
	if envs, _ := f.cluster.Envs(context.Background()); len(envs) != 0 {
		t.Errorf("a refused create left namespaces: %v", envs)
	}

	dry, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "dry", ValidateOnly: true})
	if err != nil || dry.GetOperation() != nil || dry.GetEnvironment().GetImage() != f.image || dry.GetEnvironment().GetNamespace() != "preview-dry" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if _, err := f.cluster.Env(context.Background(), "dry"); !errors.Is(err, errNoEnv) {
		t.Error("validate_only created a namespace")
	}
}

func TestCreateEnvironmentsOffAnswersFailedPrecondition(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Authenticator: StaticAuthenticator{"writer": {Subject: "ana"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := forgev1connect.NewEnvironmentServiceClient(srv.Client(), srv.URL)
	_, err = c.CreateEnvironment(context.Background(), as("writer", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/x"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "previews are not configured") {
		t.Errorf("previews off: %v", err)
	}
}

func TestCreateRefusesDuplicatesAndLimit(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, func(o *EnvOptions) { o.Max = 2 }) // no image: creates stay pending
	a, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate while pending: %v", err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "c"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("limit counts pending creates: %v", err)
	}
	f.ops.Cancel(a.GetOperation().GetName())
	f.wait(t, a.GetOperation())
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "c"}); err != nil {
		t.Errorf("after a cancel frees a slot: %v", err)
	}
	for _, op := range f.ops.List("", true) {
		f.ops.Cancel(op.GetName())
	}
}

func TestCreateEnvironmentRequestID(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, nil)
	req := &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", RequestId: "k1"}
	first, err := f.create(t, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", RequestId: "k1"})
	if err != nil || again.GetOperation().GetName() != first.GetOperation().GetName() {
		t.Errorf("retry: %v %v", again, err)
	}
	f.ops.Cancel(first.GetOperation().GetName())
	f.wait(t, first.GetOperation())
}

func TestCreateWaitsForImageAndCI(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, nil)
	f.available(t, nil)
	f.gh.SetRuns(envSHA, []github.WorkflowRun{{Name: "ci", Status: "in_progress"}})
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	name := resp.GetOperation().GetName()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		op, _ := f.ops.Get(name)
		if n := len(op.GetEvents()); n > 1 && strings.Contains(op.GetEvents()[n-1].GetMessage(), "CI: in_progress") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no CI status event: %v", op.GetEvents())
		}
	}
	if _, err := f.cluster.Env(context.Background(), "a"); !errors.Is(err, errNoEnv) {
		t.Error("namespace created before the image exists")
	}
	f.images.Add(f.image)
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Errorf("after the image appeared: %v", op.GetError())
	}

	failing := newEnvFixture(t, true, nil)
	failing.gh.SetRuns(envSHA, []github.WorkflowRun{
		{Name: "ci", Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/showmino/payments/actions/runs/7"},
		{Name: "ci", Status: "completed", Conclusion: "success"}, // older run of the same workflow
	})
	resp, err = failing.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	op := failing.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != "ci_failed" || !strings.Contains(op.GetError().GetMessage(), "actions/runs/7") {
		t.Errorf("ci failure: %v", op.GetError())
	}

	slow := newEnvFixture(t, true, func(o *EnvOptions) { o.ImageTimeout = 30 * time.Millisecond })
	resp, err = slow.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if op := slow.wait(t, resp.GetOperation()); op.GetError().GetCode() != "image_timeout" {
		t.Errorf("image timeout: %v", op.GetError())
	}
	for _, fx := range []*envFixture{failing, slow} {
		if envs, _ := fx.cluster.Envs(context.Background()); len(envs) != 0 {
			t.Errorf("failure before the namespace left one: %v", envs)
		}
	}
}

func TestCreateNotReady(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, func(o *EnvOptions) { o.ReadyTimeout = 100 * time.Millisecond })
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// The pod cannot pull its image.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := f.cluster.Kube.AppsV1().Deployments("preview-a").Get(context.Background(), "payments", metav1.GetOptions{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no deployment")
		}
	}
	_, _ = f.cluster.Kube.CoreV1().Pods("preview-a").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	op := f.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != "not_ready" || !strings.Contains(op.GetError().GetMessage(), "ImagePullBackOff") {
		t.Errorf("not ready: %v", op.GetError())
	}
	ns, err := f.cluster.Env(context.Background(), "a")
	if err != nil || ns.Annotations[annState] != stateFailed || ns.Annotations[annStateMessage] == "" {
		t.Errorf("failed env kept for debugging: %v %v", ns, err)
	}
}

func TestCancelCreate(t *testing.T) {
	t.Parallel()
	// Cancelled while waiting for the image: nothing to remove.
	f := newEnvFixture(t, true, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	f.ops.Cancel(resp.GetOperation().GetName())
	if op := f.wait(t, resp.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("cancel while waiting: %v", op.GetError())
	}

	// Cancelled after the namespace exists (never ready): it is removed.
	g := newEnvFixture(t, false, nil)
	resp, err = g.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		op, _ := g.ops.Get(resp.GetOperation().GetName())
		if strings.Contains(eventSteps(op), "apply") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never applied")
		}
	}
	g.ops.Cancel(resp.GetOperation().GetName())
	op := g.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != OpCodeCancelled || !strings.Contains(eventSteps(op), "cancel") {
		t.Errorf("cancel after namespace: %v %s", op.GetError(), eventSteps(op))
	}
	if gone, _ := g.cluster.EnvGone(context.Background(), "b"); !gone {
		t.Error("namespace left after cancel")
	}
}
```

- [ ] **Step 2:** `go test ./internal/server -run 'TestCreate|TestCancelCreate'`. Expected: build failure, `undefined: EnvOptions`.

- [ ] **Step 3: Add `get` to the catalog** (`internal/server/catalog.go`, after `has`):

```go
// get returns a copy of the service registered under name.
func (c *Catalog) get(name string) (*forgev1.Service, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.services[name]
	if !ok {
		return nil, false
	}
	return cloneSvc(s), true
}
```

- [ ] **Step 4: Write `internal/server/environments.go`**

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

const (
	envNamePrefix  = "environments/"
	kindCreateEnv  = "create_environment"
	kindDestroyEnv = "destroy_environment"
	defaultTTL     = 24 * time.Hour
	minTTL         = time.Hour
	maxTTL         = 168 * time.Hour
)

var (
	envIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	refRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
)

// EnvGitHub is what environments need from GitHub (*github.Client as the App).
type EnvGitHub interface {
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	RunsFor(ctx context.Context, owner, repo, sha string) ([]github.WorkflowRun, error)
}

// ImageChecker reports whether an image exists in its registry.
type ImageChecker interface {
	Exists(ctx context.Context, image string) (bool, error)
}

// EnvOptions configure preview environments.
type EnvOptions struct {
	Cluster    *Cluster
	GitHub     EnvGitHub
	Images     ImageChecker
	Catalog    *Catalog
	Operations *Operations
	// Org is the GitHub org services' repos must be in.
	Org string
	// Domain: environment hosts are <environment>.<Domain>.
	Domain string
	// Max is how many environments may exist at once (default 10).
	Max int
	// Step timeouts: defaults 10m, 5m and 3m.
	ImageTimeout, ReadyTimeout, DestroyTimeout time.Duration
	// How often to look for the image (default 10s) and at the cluster (default 2s).
	ImagePoll, Poll time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Environments serves EnvironmentService. A nil *Environments answers every
// call with FAILED_PRECONDITION: previews are off.
type Environments struct {
	o EnvOptions

	mu sync.Mutex
	// pending holds environments whose create has not made the namespace yet.
	pending map[string]bool
	// requests maps a create's request_id to its operation.
	requests map[string]string
}

// NewEnvironments returns the environment service.
func NewEnvironments(o EnvOptions) *Environments {
	if o.Max == 0 {
		o.Max = 10
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.ImageTimeout, 10 * time.Minute}, {&o.ReadyTimeout, 5 * time.Minute}, {&o.DestroyTimeout, 3 * time.Minute}, {&o.ImagePoll, 10 * time.Second}, {&o.Poll, 2 * time.Second}} {
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
	return &Environments{o: o, pending: map[string]bool{}, requests: map[string]string{}}
}

func (e *Environments) ready() error {
	if e == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("previews are not configured on this Forge API"))
	}
	return nil
}

// url is where environment id is reached.
func (e *Environments) url(id string) string { return "http://" + id + "." + e.o.Domain }

func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// parseEnvName turns "environments/x" into "x".
func parseEnvName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, envNamePrefix)
	if !ok || !envIDRE.MatchString(id) {
		return "", invalidArg("%q is not an environment name (environments/<name>, a lowercase DNS label of at most 40 characters)", name)
	}
	return id, nil
}

// ttlOf checks a requested lifetime; nil means the default.
func ttlOf(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return defaultTTL, nil
	}
	t := d.AsDuration()
	if t < minTTL || t > maxTTL {
		return 0, invalidArg("ttl %s must be between 1h and 168h", t)
	}
	return t, nil
}

// envErr turns a cluster error into an RPC error.
func envErr(id string, err error) error {
	if errors.Is(err, errNoEnv) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", envNamePrefix, id))
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("kubernetes: %w", err))
}

// repoOf returns owner and repo of a service's GitHub repo in org.
func repoOf(repoURL, org string) (string, string, bool) {
	rest, ok := strings.CutPrefix(repoURL, "https://github.com/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	if !ok || !strings.EqualFold(owner, org) || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

var states = map[string]forgev1.Environment_State{
	stateCreating:   forgev1.Environment_STATE_CREATING,
	stateReady:      forgev1.Environment_STATE_READY,
	stateFailed:     forgev1.Environment_STATE_FAILED,
	stateDestroying: forgev1.Environment_STATE_DESTROYING,
}

// envFromNamespace reads an environment from its namespace.
func (e *Environments) envFromNamespace(ns *corev1.Namespace) *forgev1.Environment {
	id := ns.Labels[labelEnvironment]
	a := ns.Annotations
	env := &forgev1.Environment{
		Name:         envNamePrefix + id,
		Service:      servicePrefix + ns.Labels[labelService],
		Ref:          a[annRef],
		Commit:       a[annCommit],
		Image:        a[annImage],
		Url:          e.url(id),
		State:        states[a[annState]],
		StateMessage: a[annStateMessage],
		Namespace:    ns.Name,
		Creator:      a[annCreator],
		CreateTime:   timestamppb.New(ns.CreationTimestamp.Time),
	}
	if t, err := time.Parse(time.RFC3339, a[annExpire]); err == nil {
		env.ExpireTime = timestamppb.New(t)
	}
	if ops := e.o.Operations.List(env.Name, false); len(ops) > 0 {
		env.Operation = ops[0].GetName()
	}
	return env
}

// countEnvs is how many environments exist or are being created.
func (e *Environments) countEnvs(nss []corev1.Namespace) int {
	n := len(e.pending)
	for i := range nss {
		if !e.pending[nss[i].Labels[labelEnvironment]] {
			n++
		}
	}
	return n
}

func (e *Environments) GetEnvironment(context.Context, *connect.Request[forgev1.GetEnvironmentRequest]) (*connect.Response[forgev1.GetEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GetEnvironment is not implemented yet"))
}

func (e *Environments) ListEnvironments(context.Context, *connect.Request[forgev1.ListEnvironmentsRequest]) (*connect.Response[forgev1.ListEnvironmentsResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListEnvironments is not implemented yet"))
}

func (e *Environments) ExtendEnvironment(context.Context, *connect.Request[forgev1.ExtendEnvironmentRequest]) (*connect.Response[forgev1.ExtendEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ExtendEnvironment is not implemented yet"))
}

func (e *Environments) DestroyEnvironment(context.Context, *connect.Request[forgev1.DestroyEnvironmentRequest]) (*connect.Response[forgev1.DestroyEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("DestroyEnvironment is not implemented yet"))
}
```

- [ ] **Step 5: Write `internal/server/env_create.go`**

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// envPlan is a checked, resolved create.
type envPlan struct {
	id, serviceID, owner, repo string
	ref, commit, image, creator string
	port                        int32
	health                      string
	ttl                         time.Duration
}

func (e *Environments) CreateEnvironment(ctx context.Context, req *connect.Request[forgev1.CreateEnvironmentRequest]) (*connect.Response[forgev1.CreateEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	m := req.Msg
	p, err := e.plan(ctx, m)
	if err != nil {
		return nil, err
	}
	preview := &forgev1.Environment{
		Name: envNamePrefix + p.id, Service: servicePrefix + p.serviceID, Ref: p.ref, Commit: p.commit,
		Image: p.image, Url: e.url(p.id), Namespace: envNamespace(p.id), Creator: p.creator,
		State: forgev1.Environment_STATE_CREATING,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if key := m.GetRequestId(); key != "" && !m.GetValidateOnly() {
		if name, ok := e.requests[key]; ok {
			if op, ok := e.o.Operations.Get(name); ok {
				return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Operation: op, Environment: preview}), nil
			}
		}
	}
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return nil, envErr(p.id, err)
	}
	if e.pending[p.id] || slicesContainsEnv(nss, p.id) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", envNamePrefix, p.id))
	}
	if n := e.countEnvs(nss); n >= e.o.Max {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%d environments exist, the most this Forge API allows; destroy one first", n))
	}
	if m.GetValidateOnly() {
		preview.State = forgev1.Environment_STATE_UNSPECIFIED
		preview.ExpireTime = timestamppb.New(e.o.Now().Add(p.ttl))
		return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Environment: preview}), nil
	}
	e.pending[p.id] = true
	op := e.o.Operations.Start(ctx, kindCreateEnv, envNamePrefix+p.id, p.creator, e.createWork(p))
	if key := m.GetRequestId(); key != "" {
		e.requests[key] = op.GetName()
	}
	return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Operation: op, Environment: preview}), nil
}

func slicesContainsEnv(nss []corev1.Namespace, id string) bool {
	for i := range nss {
		if nss[i].Labels[labelEnvironment] == id {
			return true
		}
	}
	return false
}

// plan checks the request and resolves the ref, forge.yaml and image.
func (e *Environments) plan(ctx context.Context, m *forgev1.CreateEnvironmentRequest) (envPlan, error) {
	p := envPlan{id: m.GetEnvironmentId(), ref: m.GetRef()}
	if !envIDRE.MatchString(p.id) {
		return p, invalidArg("%q is not a valid environment name (lowercase letters, digits and '-', at most 40)", p.id)
	}
	ttl, err := ttlOf(m.GetTtl())
	if err != nil {
		return p, err
	}
	p.ttl = ttl
	if p.ref == "" {
		p.ref = "main"
	}
	if !refRE.MatchString(p.ref) || strings.Contains(p.ref, "..") {
		return p, invalidArg("%q is not a valid Git ref", p.ref)
	}
	sid, ok := strings.CutPrefix(m.GetService(), servicePrefix)
	if !ok || sid == "" {
		return p, invalidArg("service %q must be services/<name>", m.GetService())
	}
	p.serviceID = sid
	svc, ok := e.o.Catalog.get(servicePrefix + sid)
	if !ok {
		return p, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", servicePrefix, sid))
	}
	if p.owner, p.repo, ok = repoOf(svc.GetRepoUrl(), e.o.Org); !ok {
		return p, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no GitHub repo in %s (repo_url %q)", servicePrefix, sid, e.o.Org, svc.GetRepoUrl()))
	}
	if pr, ok := PrincipalFrom(ctx); ok {
		p.creator = pr.Subject
	}
	p.commit, err = e.o.GitHub.ResolveRef(ctx, p.owner, p.repo, p.ref)
	if errors.Is(err, github.ErrNotFound) {
		return p, connect.NewError(connect.CodeNotFound, fmt.Errorf("ref %q not found in github.com/%s/%s", p.ref, p.owner, p.repo))
	}
	if err != nil {
		return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("resolve %s in github.com/%s/%s: %w", p.ref, p.owner, p.repo, err))
	}
	p.port, p.health = manifest.DefaultRunPort, manifest.DefaultRunHealth
	data, err := e.o.GitHub.FileAt(ctx, p.owner, p.repo, manifest.FileName, p.commit)
	switch {
	case errors.Is(err, github.ErrNotFound): // no forge.yaml: defaults
	case err != nil:
		return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read forge.yaml at %s: %w", shortSHA(p.commit), err))
	default:
		mf, err := manifest.Parse(data)
		if err != nil {
			return p, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("forge.yaml at %s is invalid: %w", shortSHA(p.commit), err))
		}
		p.port, p.health = int32(mf.RunPort()), mf.RunHealth()
	}
	p.image = "ghcr.io/" + strings.ToLower(p.owner) + "/" + p.serviceID + ":" + shortSHA(p.commit)
	return p, nil
}

func (e *Environments) donePending(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pending, id)
}

// createWork is the create operation: image, namespace, apply, ready.
func (e *Environments) createWork(p envPlan) Work {
	return func(ctx context.Context, emit func(string, string)) error {
		defer e.donePending(p.id)
		emit("resolve", fmt.Sprintf("%s is %s", p.ref, shortSHA(p.commit)))
		if err := e.waitForImage(ctx, p, emit); err != nil {
			return err
		}
		ann := map[string]string{
			annRef: p.ref, annCommit: p.commit, annImage: p.image, annCreator: p.creator,
			annExpire: e.o.Now().Add(p.ttl).UTC().Format(time.RFC3339), annState: stateCreating,
		}
		if err := e.o.Cluster.CreateEnv(ctx, p.id, map[string]string{labelService: p.serviceID}, ann); err != nil {
			if errors.Is(err, errEnvExists) {
				return &OpError{Code: "already_exists", Message: envNamePrefix + p.id + " already exists"}
			}
			return err
		}
		e.donePending(p.id) // the namespace counts from now on
		emit("namespace", "created "+envNamespace(p.id))
		err := e.deployAndWait(ctx, p, emit)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return e.undo(ctx, p.id, emit)
		}
		var oe *OpError
		if !errors.As(err, &oe) {
			oe = &OpError{Code: "apply_failed", Message: err.Error()}
		}
		if aerr := e.o.Cluster.Annotate(context.WithoutCancel(ctx), p.id, map[string]string{annState: stateFailed, annStateMessage: clip(oe.Message)}); aerr != nil {
			e.o.Log.Warn("mark environment failed", "environment", p.id, "error", aerr)
		}
		return oe
	}
}

// waitForImage polls the registry until the image exists, failing fast when
// a CI run for the commit finished without success.
func (e *Environments) waitForImage(ctx context.Context, p envPlan, emit func(string, string)) error {
	timeout := time.NewTimer(e.o.ImageTimeout)
	defer timeout.Stop()
	last := ""
	for {
		ok, err := e.o.Images.Exists(ctx, p.image)
		if err == nil && ok {
			emit("image", "found "+p.image)
			return nil
		}
		status := "unknown"
		if runs, rerr := e.o.GitHub.RunsFor(ctx, p.owner, p.repo, p.commit); rerr == nil {
			if r := failedRun(runs); r != nil {
				return &OpError{Code: "ci_failed", Message: fmt.Sprintf("CI %q for %s finished with %s: %s", r.Name, shortSHA(p.commit), r.Conclusion, r.HTMLURL)}
			}
			status = "not started"
			if len(runs) > 0 {
				status = runs[0].Status
			}
		}
		msg := fmt.Sprintf("waiting for %s (CI: %s)", p.image, status)
		if err != nil {
			msg += "; registry: " + clip(err.Error())
		}
		if msg != last {
			emit("image", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "image_timeout", Message: fmt.Sprintf("%s did not appear within %s", p.image, e.o.ImageTimeout)}
		case <-time.After(e.o.ImagePoll):
		}
	}
}

// failedRun returns the newest run of a workflow when it finished without
// success; older runs of the same workflow do not count.
func failedRun(runs []github.WorkflowRun) *github.WorkflowRun {
	seen := map[string]bool{}
	for i := range runs {
		r := &runs[i]
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if r.Status == "completed" && r.Conclusion != "success" && r.Conclusion != "skipped" && r.Conclusion != "neutral" {
			return r
		}
	}
	return nil
}

// deployAndWait creates the workload and waits until it is available.
func (e *Environments) deployAndWait(ctx context.Context, p envPlan, emit func(string, string)) error {
	host := p.id + "." + e.o.Domain
	if err := e.o.Cluster.Deploy(ctx, p.id, Workload{Service: p.serviceID, Image: p.image, Host: host, Port: p.port, Health: p.health}); err != nil {
		return err
	}
	emit("apply", fmt.Sprintf("deployment %s on port %d, route %s", p.serviceID, p.port, host))
	timeout := time.NewTimer(e.o.ReadyTimeout)
	defer timeout.Stop()
	last := ""
	for {
		ready, problem, err := e.o.Cluster.Status(ctx, p.id, p.serviceID)
		if err == nil && ready {
			if err := e.o.Cluster.Annotate(ctx, p.id, map[string]string{annState: stateReady, annStateMessage: ""}); err != nil {
				return err
			}
			emit("ready", e.url(p.id))
			return nil
		}
		if err == nil && problem != "" && problem != last {
			emit("ready", problem)
			last = problem
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "not_ready", Message: fmt.Sprintf("%s was not ready within %s: %s", p.serviceID, e.o.ReadyTimeout, last)}
		case <-time.After(e.o.Poll):
		}
	}
}

// undo removes a cancelled create's namespace. The operation ends
// "cancelled", or "cancel_cleanup_failed" when the namespace stays.
func (e *Environments) undo(ctx context.Context, id string, emit func(string, string)) error {
	emit("cancel", "removing "+envNamespace(id))
	if err := e.removeNamespace(context.WithoutCancel(ctx), id); err != nil {
		return &OpError{Code: "cancel_cleanup_failed", Message: fmt.Sprintf("cancelled, but %s could not be removed: %v", envNamespace(id), err)}
	}
	return ctx.Err()
}

// removeNamespace deletes environment id's namespace and waits until it is gone.
func (e *Environments) removeNamespace(ctx context.Context, id string) error {
	if err := e.o.Cluster.DeleteEnv(ctx, id); err != nil && !errors.Is(err, errNoEnv) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.o.DestroyTimeout)
	defer cancel()
	for {
		if gone, err := e.o.Cluster.EnvGone(ctx, id); err == nil && gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s still exists after %s", envNamespace(id), e.o.DestroyTimeout)
		case <-time.After(e.o.Poll):
		}
	}
}
```

  `undo` returns `ctx.Err()` (a `context.Canceled`) on success, and the store maps that to `cancelled` because the work's context was cancelled.

- [ ] **Step 6: Wire into `server.New`.** In `internal/server/server.go`, add to `Options`, after `Operations`:

```go
	// Environments serves EnvironmentService; nil answers FAILED_PRECONDITION.
	// It must share Catalog and Operations with these Options.
	Environments *Environments
```

and after the OperationService handler:

```go
	mux.Handle(forgev1connect.NewEnvironmentServiceHandler(o.Environments, opts))
```

  Passing a nil `*Environments` is intended: its methods check `e.ready()` first.

- [ ] **Step 7:** `go test -race -count=3 ./internal/server -run 'TestCreate|TestCancelCreate'`. Expected: PASS three times.

- [ ] **Step 8:** `make build test lint`, then `git add internal/server && git commit -m "server: create preview environments"`.

---

### Task 7: `EnvironmentService`, part 2: get, list, extend, destroy and the reaper

**Files:**
- Create: `internal/server/env_destroy.go`
- Modify: `internal/server/environments.go` (replace the four `Unimplemented` methods)
- Test: `internal/server/environments_more_test.go`

**Interfaces:**
- Consumes: everything Task 6 produced; `newEnvFixture`, `(*envFixture).available`, `create`, `wait` and `eventSteps` from `environments_test.go`.
- Produces:
  - `func (e *Environments) Reap(ctx context.Context) error`, `func (e *Environments) RunReaper(ctx context.Context, every time.Duration)`.
  - `func (e *Environments) startDestroy(ctx context.Context, id, creator string) *forgev1.Operation`.
  - `expiry(ns *corev1.Namespace) time.Time`.

- [ ] **Step 1: Write the failing tests** `internal/server/environments_more_test.go`:

```go
package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// readyEnv creates environment id and waits until it is ready.
func (f *envFixture) readyEnv(t *testing.T, id string) {
	t.Helper()
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: id})
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
}

func TestGetListExtend(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	f.readyEnv(t, "b")
	ctx := context.Background()

	got, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	env := got.Msg.GetEnvironment()
	if env.GetState() != forgev1.Environment_STATE_READY || env.GetService() != "services/payments" || env.GetCommit() != envSHA ||
		env.GetUrl() != "http://a.preview.example" || env.GetNamespace() != "preview-a" || env.GetCreator() != "ana" ||
		env.GetOperation() == "" || env.GetExpireTime() == nil {
		t.Errorf("get: %v", env)
	}
	for _, tc := range []struct {
		name string
		code connect.Code
	}{{"environments/nope", connect.CodeNotFound}, {"a", connect.CodeInvalidArgument}, {"environments/A", connect.CodeInvalidArgument}} {
		if _, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: tc.name})); connect.CodeOf(err) != tc.code {
			t.Errorf("get %q: %v", tc.name, err)
		}
	}

	list, err := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{}))
	if err != nil || len(list.Msg.GetEnvironments()) != 2 || list.Msg.GetEnvironments()[0].GetName() != "environments/a" {
		t.Errorf("list: %v %v", list, err)
	}
	if l, _ := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{Service: "services/other"})); len(l.Msg.GetEnvironments()) != 0 {
		t.Errorf("list by service: %v", l.Msg)
	}

	ext, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: durationpb.New(72 * time.Hour)}))
	if err != nil || !ext.Msg.GetEnvironment().GetExpireTime().AsTime().Equal(f.clock.now().Add(72*time.Hour).Truncate(time.Second)) {
		t.Errorf("extend: %v %v", ext, err)
	}
	for _, ttl := range []*durationpb.Duration{nil, durationpb.New(10 * time.Minute), durationpb.New(200 * time.Hour)} {
		if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: ttl})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("extend ttl %v: %v", ttl, err)
		}
	}
	if _, err := f.client.ExtendEnvironment(ctx, as("reader", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader extend: %v", err)
	}
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annState: stateDestroying})
	if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/b", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("extend destroying: %v", err)
	}
}

func TestGetReportsLiveProblem(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	var crashed atomic.Bool
	f.available(t, func(string) bool { return crashed.Load() })
	f.readyEnv(t, "a")
	ctx := context.Background()
	crashed.Store(true) // the fake controller stops making it available
	d, _ := f.cluster.Kube.AppsV1().Deployments("preview-a").Get(ctx, "payments", metav1.GetOptions{})
	d.Status.AvailableReplicas, d.Status.ReadyReplicas = 0, 0
	_, _ = f.cluster.Kube.AppsV1().Deployments("preview-a").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	_, _ = f.cluster.Kube.CoreV1().Pods("preview-a").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	got, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	if env := got.Msg.GetEnvironment(); env.GetState() != forgev1.Environment_STATE_READY || env.GetStateMessage() != "CrashLoopBackOff" {
		t.Errorf("live problem: %v", env)
	}
}

func TestDestroyEnvironment(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	ctx := context.Background()

	dry, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetEnvironment().GetName() != "environments/a" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if _, err := f.cluster.Env(ctx, "a"); err != nil {
		t.Error("validate_only destroyed it")
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("reader", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader destroy: %v", err)
	}
	resp, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	op := f.wait(t, resp.Msg.GetOperation())
	if op.GetError() != nil || op.GetKind() != "destroy_environment" || op.GetTarget() != "environments/a" {
		t.Errorf("destroy op: %v", op)
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("namespace still exists")
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("destroy again: %v", err)
	}
}

func TestDestroyCancelsRunningCreate(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil) // never available: the create waits in "ready"
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := f.cluster.Env(ctx, "a"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no namespace")
		}
	}
	d, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, d.Msg.GetOperation()); op.GetError() != nil {
		t.Errorf("destroy: %v", op.GetError())
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("create: %v", op.GetError())
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("namespace left")
	}
}

func TestGuardOnRPCs(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	ctx := context.Background()
	_, _ = f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-x"}}, metav1.CreateOptions{})
	if _, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("get: %v", err)
	}
	if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/x", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("extend: %v", err)
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("destroy: %v", err)
	}
	if l, _ := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{})); len(l.Msg.GetEnvironments()) != 0 {
		t.Errorf("list: %v", l.Msg)
	}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "preview-x", metav1.GetOptions{}); err != nil {
		t.Errorf("preview-x touched: %v", err)
	}
}

func TestReaper(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	f.readyEnv(t, "b")
	ctx := context.Background()
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annExpire: f.clock.now().Add(72 * time.Hour).UTC().Format(time.RFC3339)})

	if err := f.envs.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if ops := f.ops.List("", true); len(ops) != 0 {
		t.Errorf("reaped before expiry: %v", ops)
	}
	f.clock.add(25 * time.Hour)
	if err := f.envs.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.envs.Reap(ctx); err != nil { // a second pass must not start a second destroy
		t.Fatal(err)
	}
	destroys := 0
	for _, op := range f.ops.List("environments/a", false) {
		if op.GetKind() == "destroy_environment" {
			destroys++
			if op.GetCreator() != "forge-reaper" {
				t.Errorf("creator %q", op.GetCreator())
			}
			wait(t, f.ops, op.GetName())
		}
	}
	if destroys != 1 {
		t.Errorf("%d destroys for a, want 1", destroys)
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("expired a not removed")
	}
	if _, err := f.cluster.Env(ctx, "b"); err != nil {
		t.Error("b (72h) removed early")
	}

	// A malformed expiry counts as creation time + 168h.
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annExpire: "soon"})
	ns, _ := f.cluster.Env(ctx, "b")
	if !expiry(ns).Equal(ns.CreationTimestamp.Add(168 * time.Hour)) {
		t.Errorf("malformed expiry: %v", expiry(ns))
	}
}
```

- [ ] **Step 2:** `go test ./internal/server -run 'TestGetListExtend|TestGetReports|TestDestroy|TestGuardOnRPCs|TestReaper'`. Expected: FAIL. `Unimplemented` errors, and `undefined: expiry`.

- [ ] **Step 3: Replace the four methods in `environments.go`** (delete the `Unimplemented` versions):

```go
func (e *Environments) GetEnvironment(ctx context.Context, req *connect.Request[forgev1.GetEnvironmentRequest]) (*connect.Response[forgev1.GetEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	ns, err := e.o.Cluster.Env(ctx, id)
	if err != nil {
		return nil, envErr(id, err)
	}
	env := e.envFromNamespace(ns)
	if env.GetState() == forgev1.Environment_STATE_READY {
		if ready, problem, err := e.o.Cluster.Status(ctx, id, ns.Labels[labelService]); err == nil && !ready {
			env.StateMessage = problem
		}
	}
	return connect.NewResponse(&forgev1.GetEnvironmentResponse{Environment: env}), nil
}

func (e *Environments) ListEnvironments(ctx context.Context, req *connect.Request[forgev1.ListEnvironmentsRequest]) (*connect.Response[forgev1.ListEnvironmentsResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return nil, envErr("", err)
	}
	var out []*forgev1.Environment
	for i := range nss {
		env := e.envFromNamespace(&nss[i])
		if s := req.Msg.GetService(); s != "" && env.GetService() != s {
			continue
		}
		out = append(out, env)
	}
	slices.SortFunc(out, func(a, b *forgev1.Environment) int { return strings.Compare(a.GetName(), b.GetName()) })
	return connect.NewResponse(&forgev1.ListEnvironmentsResponse{Environments: out}), nil
}

func (e *Environments) ExtendEnvironment(ctx context.Context, req *connect.Request[forgev1.ExtendEnvironmentRequest]) (*connect.Response[forgev1.ExtendEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetTtl() == nil {
		return nil, invalidArg("ttl is required")
	}
	ttl, err := ttlOf(req.Msg.GetTtl())
	if err != nil {
		return nil, err
	}
	ns, err := e.o.Cluster.Env(ctx, id)
	if err != nil {
		return nil, envErr(id, err)
	}
	if ns.Annotations[annState] == stateDestroying {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s is being destroyed", envNamePrefix, id))
	}
	if err := e.o.Cluster.Annotate(ctx, id, map[string]string{annExpire: e.o.Now().Add(ttl).UTC().Format(time.RFC3339)}); err != nil {
		return nil, envErr(id, err)
	}
	if ns, err = e.o.Cluster.Env(ctx, id); err != nil {
		return nil, envErr(id, err)
	}
	return connect.NewResponse(&forgev1.ExtendEnvironmentResponse{Environment: e.envFromNamespace(ns)}), nil
}

func (e *Environments) DestroyEnvironment(ctx context.Context, req *connect.Request[forgev1.DestroyEnvironmentRequest]) (*connect.Response[forgev1.DestroyEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	ns, err := e.o.Cluster.Env(ctx, id)
	if err != nil {
		return nil, envErr(id, err)
	}
	env := e.envFromNamespace(ns)
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.DestroyEnvironmentResponse{Environment: env}), nil
	}
	creator := ""
	if p, ok := PrincipalFrom(ctx); ok {
		creator = p.Subject
	}
	op := e.startDestroy(ctx, id, creator)
	return connect.NewResponse(&forgev1.DestroyEnvironmentResponse{Operation: op, Environment: env}), nil
}
```

  Add `"slices"` to `environments.go`'s imports.

- [ ] **Step 4: Write `internal/server/env_destroy.go`**

```go
package server

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// startDestroy starts removing environment id, or returns the destroy that
// is already running for it.
func (e *Environments) startDestroy(ctx context.Context, id, creator string) *forgev1.Operation {
	target := envNamePrefix + id
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, op := range e.o.Operations.List(target, true) {
		if op.GetKind() == kindDestroyEnv {
			return op
		}
	}
	return e.o.Operations.Start(ctx, kindDestroyEnv, target, creator, e.destroyWork(id))
}

// destroyWork cancels a running create of the environment, then deletes its
// namespace and waits until it is gone.
func (e *Environments) destroyWork(id string) Work {
	return func(ctx context.Context, emit func(string, string)) error {
		for _, op := range e.o.Operations.List(envNamePrefix+id, true) {
			if op.GetKind() != kindCreateEnv {
				continue
			}
			emit("cancel", "stopping "+op.GetName())
			e.o.Operations.Cancel(op.GetName())
			if _, err := e.o.Operations.Wait(ctx, op.GetName()); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if err := e.o.Cluster.Annotate(ctx, id, map[string]string{annState: stateDestroying}); err != nil && !errors.Is(err, errNoEnv) {
			return err
		}
		if err := e.removeNamespace(ctx, id); err != nil {
			return &OpError{Code: "destroy_failed", Message: err.Error()}
		}
		emit("destroy", "removed "+envNamespace(id))
		return nil
	}
}

// expiry is when ns's environment expires: its expire-time annotation, or
// creation plus the longest TTL when that is missing or unreadable.
func expiry(ns *corev1.Namespace) time.Time {
	if t, err := time.Parse(time.RFC3339, ns.Annotations[annExpire]); err == nil {
		return t
	}
	return ns.CreationTimestamp.Add(maxTTL)
}

// Reap starts a destroy for every expired environment.
func (e *Environments) Reap(ctx context.Context) error {
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return err
	}
	now := e.o.Now()
	for i := range nss {
		ns := &nss[i]
		if expiry(ns).After(now) {
			continue
		}
		id := ns.Labels[labelEnvironment]
		op := e.startDestroy(context.Background(), id, "forge-reaper")
		e.o.Log.Info("environment expired", "environment", id, "operation", op.GetName())
	}
	return nil
}

// RunReaper calls Reap every interval until ctx ends.
func (e *Environments) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Reap(ctx); err != nil {
				e.o.Log.Warn("reaper", "error", err)
			}
		}
	}
}
```

  `startDestroy` holds `e.mu` while it calls `Operations.Start`. That is safe because `Start` never calls back into `Environments`. `CreateEnvironment` also holds `e.mu`, so a destroy and a create of the same environment can't interleave their checks.

- [ ] **Step 5:** `go test -race -count=3 ./internal/server`. Expected: PASS, every server test included.

- [ ] **Step 6:** `make build test lint`, then `git add internal/server && git commit -m "server: get, list, extend and destroy environments; the reaper"`.

---

### Task 8: forge-api settings

**Files:**
- Create: `cmd/forge-api/previews.go`
- Modify: `cmd/forge-api/main.go`
- Test: `cmd/forge-api/previews_test.go`

**Interfaces:**
- Consumes: `server.NewEnvironments`, `EnvOptions`, `Cluster` and `RunReaper`; `server.NewCatalog`; `server.NewOperations`; `registry.Client` and `registry.CredsFromDockerConfig`; `*github.App` (`Token`); `github.Client`.
- Produces:
  - `type previewFlags struct { on bool; domain, gateway, pullSecret string; max int; imageTimeout, readyTimeout string }`.
  - `func previews(f previewFlags, app *github.App, catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger, cluster func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Environments, error)`.
  - `func kubeClients() (kubernetes.Interface, dynamic.Interface, error)`.

- [ ] **Step 1: Write the failing test** `cmd/forge-api/previews_test.go`:

```go
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

func fakeKube() (kubernetes.Interface, dynamic.Interface, error) {
	return envtest.Kube(), envtest.Dynamic(), nil
}

func TestPreviews(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &github.App{ID: 1, Key: key, Org: "showmino"}
	ok := previewFlags{on: true, domain: "preview.10.150.1.222.nip.io", gateway: "forge/forge-previews", pullSecret: "forge/mino-ghcr", max: 10, imageTimeout: "10m", readyTimeout: "5m"}
	log := slog.New(slog.DiscardHandler)
	cat, ops := server.NewCatalog(nil), server.NewOperations(nil)

	if envs, err := previews(previewFlags{}, nil, cat, ops, "showmino", log, fakeKube); envs != nil || err != nil {
		t.Errorf("off: %v %v", envs, err)
	}
	if envs, err := previews(ok, app, cat, ops, "showmino", log, fakeKube); envs == nil || err != nil {
		t.Errorf("on: %v %v", envs, err)
	}
	for name, tc := range map[string]struct {
		f   previewFlags
		app *github.App
		msg string
	}{
		"no app":        {ok, nil, "needs the GitHub App"},
		"no domain":     {func() previewFlags { f := ok; f.domain = ""; return f }(), app, "--preview-domain"},
		"bad gateway":   {func() previewFlags { f := ok; f.gateway = "x"; return f }(), app, "namespace/name"},
		"bad pull":      {func() previewFlags { f := ok; f.pullSecret = "x"; return f }(), app, "namespace/name"},
		"bad max":       {func() previewFlags { f := ok; f.max = 0; return f }(), app, "--preview-max"},
		"bad image t/o": {func() previewFlags { f := ok; f.imageTimeout = "soon"; return f }(), app, "FORGE_PREVIEW_IMAGE_TIMEOUT"},
		"bad ready t/o": {func() previewFlags { f := ok; f.readyTimeout = "-1m"; return f }(), app, "FORGE_PREVIEW_READY_TIMEOUT"},
	} {
		if _, err := previews(tc.f, tc.app, cat, ops, "showmino", log, fakeKube); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.msg)
		}
	}
}
```

- [ ] **Step 2:** `go test ./cmd/forge-api -run TestPreviews`. Expected: build failure, `undefined: previewFlags`.

- [ ] **Step 3: Write `cmd/forge-api/previews.go`**

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/registry"
	"github.com/showmino/forge/internal/server"
)

// previewFlags are forge-api's preview environment settings.
type previewFlags struct {
	on                           bool
	domain, gateway, pullSecret  string
	max                          int
	imageTimeout, readyTimeout   string
}

// previews returns the environment service, or nil when previews are off.
// cluster connects to Kubernetes (kubeClients outside tests).
func previews(f previewFlags, app *github.App, catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger,
	cluster func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Environments, error) {
	if !f.on {
		return nil, nil
	}
	switch {
	case app == nil:
		return nil, errors.New("--previews needs the GitHub App (--github-app-id and --github-app-key-file)")
	case f.domain == "":
		return nil, errors.New("--previews needs --preview-domain (FORGE_PREVIEW_DOMAIN)")
	case !strings.Contains(f.gateway, "/"):
		return nil, fmt.Errorf("--preview-gateway %q must be namespace/name", f.gateway)
	case !strings.Contains(f.pullSecret, "/"):
		return nil, fmt.Errorf("--preview-pull-secret %q must be namespace/name", f.pullSecret)
	case f.max < 1:
		return nil, fmt.Errorf("--preview-max %d must be at least 1", f.max)
	}
	imageTimeout, err := positiveDuration("FORGE_PREVIEW_IMAGE_TIMEOUT", f.imageTimeout)
	if err != nil {
		return nil, err
	}
	readyTimeout, err := positiveDuration("FORGE_PREVIEW_READY_TIMEOUT", f.readyTimeout)
	if err != nil {
		return nil, err
	}
	kube, dyn, err := cluster()
	if err != nil {
		return nil, fmt.Errorf("previews: connect to Kubernetes: %w", err)
	}
	c := &server.Cluster{Kube: kube, Dynamic: dyn, Gateway: f.gateway, PullSecret: f.pullSecret}
	images := &registry.Client{Creds: func(ctx context.Context, host string) (string, string, error) {
		data, err := c.PullSecretData(ctx)
		if err != nil {
			return "", "", err
		}
		return registry.CredsFromDockerConfig(data, host)
	}}
	return server.NewEnvironments(server.EnvOptions{
		Cluster: c, GitHub: &github.Client{Token: app.Token}, Images: images,
		Catalog: catalog, Operations: ops, Org: org, Domain: f.domain, Max: f.max,
		ImageTimeout: imageTimeout, ReadyTimeout: readyTimeout, Log: log,
	}), nil
}

func positiveDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive duration such as 10m", name, v)
	}
	return d, nil
}

// kubeClients connects with the pod's service account, or with KUBECONFIG
// (default ~/.kube/config) outside a cluster.
func kubeClients() (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
	}
	if err != nil {
		return nil, nil, err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return kube, dyn, nil
}
```

- [ ] **Step 4: Wire into `main.go`.**
  - Add to the `var (...)` flag block:

```go
		previewsOn     = flag.Bool("previews", env("FORGE_PREVIEWS", "") == "1" || env("FORGE_PREVIEWS", "") == "true", "serve preview environments (needs the GitHub App and Kubernetes access)")
		previewDomain  = flag.String("preview-domain", env("FORGE_PREVIEW_DOMAIN", ""), "preview hosts are <environment>.<domain>, for example preview.10.150.1.222.nip.io")
		previewGateway = flag.String("preview-gateway", env("FORGE_PREVIEW_GATEWAY", "forge/forge-previews"), "Gateway (namespace/name) that routes previews")
		previewPull    = flag.String("preview-pull-secret", env("FORGE_PREVIEW_PULL_SECRET", "forge/mino-ghcr"), "dockerconfigjson Secret (namespace/name) copied into previews and used to check images")
		previewMax     = flag.Int("preview-max", envInt("FORGE_PREVIEW_MAX", 10), "most preview environments at once")
```

  - Add the helper:

```go
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		return -1 // invalid: --preview-max validation reports it
	}
	return def
}
```

  Import `strconv`.
  - After `templates` is set up, replace the `server.New(server.Options{...})` call so that it shares one catalog and one operations store:

```go
	catalog, ops := server.NewCatalog(nil), server.NewOperations(nil)
	envs, err := previews(previewFlags{
		on: *previewsOn, domain: *previewDomain, gateway: *previewGateway, pullSecret: *previewPull, max: *previewMax,
		imageTimeout: env("FORGE_PREVIEW_IMAGE_TIMEOUT", "10m"), readyTimeout: env("FORGE_PREVIEW_READY_TIMEOUT", "5m"),
	}, app, catalog, ops, *githubOrg, log, kubeClients)
	if err != nil {
		return err
	}
	if envs == nil {
		log.Info("preview environments are off; set --previews")
	} else {
		go envs.RunReaper(ctx, time.Minute)
	}
```

  and add `Catalog: catalog, Operations: ops, Environments: envs,` to the `server.Options` literal.
  - Update the command comment to: `Command forge-api is the Forge API server: the service catalog (in memory), templates and new services, operations and preview environments.`

- [ ] **Step 5:** `go mod tidy && go test ./cmd/forge-api && go build ./...`. Expected: PASS. `clientcmd` adds dependencies; let `go mod tidy` record them.

- [ ] **Step 6:** `make build test lint`, then `git add go.mod go.sum cmd/forge-api && git commit -m "forge-api: --previews settings and the reaper"`.

---

### Task 9: `forge env`

**Files:**
- Create: `internal/cli/env.go`
- Modify: `internal/cli/root.go` (register `env` in the core group after `create`), `internal/cli/cli_test.go` (harness field `envs`)
- Test: `internal/cli/env_test.go`, plus goldens `internal/cli/testdata/env_list_table.golden` and `internal/cli/testdata/env_get_table.golden`

**Interfaces:**
- Consumes:
  - `client.Client.Environments` (Task 1).
  - Milestone 1's `f.watchOperation`, `opFailure`, `opName`, `opID` and `printOperation`; `f.confirmDestructive`; `f.Manifest()`; `f.withTimeout` and `f.StreamClient`; `client.NewRequestID`; `exitcode.*`; `output.*`.
  - Server: `server.NewEnvironments`, `EnvOptions`, `Cluster`, `NewCatalog` and `NewOperations`; `envtest.*`.
- Produces: `func envName(s string) (string, error)` → `environments/<id>`; `func until(d time.Duration) string`, which reads `in 23h`, `in 5m`, `in <1m` or `expired`.

**CLI behaviour (exact strings):**
- **`forge env create <name> [--service S] [--ref main] [--ttl 24h] [--dry-run] [--wait=true]`:**
  - `--service` defaults to `forge.yaml`'s `service.name`; outside a repo it fails `--service is required outside a service repo` (exit 2).
  - **Dry run:** prints `Would create environments/<n> for services/<s> at <ref> (<sha7>), expiring in <ttl rounded>:` then two lines, `  url:   <url>` and `  image: <image>`.
  - **`--wait=false`:** prints `operations/<id>` and infos `Watch it with: forge op watch <id>`.
  - **Default:** watches the operation, then prints the environment as `env get` does. A failed operation exits 7 with the hint `see 'forge env get <n>'`.
- **`forge env list [--service S]`:** columns `NAME SERVICE STATE REF COMMIT EXPIRES URL`.
  - `NAME` has no `environments/` prefix and `SERVICE` no `services/` prefix.
  - `STATE` is lower-case (`creating|ready|failed|destroying`); `COMMIT` shows 7 characters; `EXPIRES` uses `until(expire - now)`.
  - When there are none: info `No environments.`; JSON `{"environments": []}`.
- **`forge env get <name>`:** a field/value table with the fields `name`, `service`, `state`, `message` (only when set), `ref`, `commit`, `image`, `url`, `namespace`, `creator`, `expires` (RFC 3339 + ` (` + until + `)`) and `operation` (only when set).
- **`forge env extend <name> --ttl 48h`:** `--ttl` is required (exit 2). It prints `environments/<n> expires <RFC3339> (in 48h)`.
- **`forge env destroy <name> [--yes] [--dry-run] [--wait=true]`:**
  - **Dry run:** prints `Would destroy environments/<n> (namespace preview-<n>)`.
  - **Confirmation:** without `--yes` and without a terminal, exit 2 (`confirmDestructive("destroy", ...)`).
  - **Default:** watches, then prints `environments/<n> destroyed`. `--wait=false` prints the operation name.

- [ ] **Step 1: Harness support.** In `internal/cli/cli_test.go`:
  - Add the field `envs *server.Environments` to `harness`.
  - Add `Catalog: h.catalog, Environments: h.envs,` to `startAPI`'s `server.Options`, along with a field `catalog *server.Catalog` set in `newHarness` to `server.NewCatalog(nil)`. Before, `server.New` created its own catalog; the harness now owns one so the environment service can share it. The catalog therefore survives `startAPI` restarts (for example `serveTemplates`). Run the whole CLI suite after this change; if an existing test relied on a restart emptying the catalog, give that test a fresh `server.NewCatalog(nil)` before the restart, not a different design.

- [ ] **Step 2: Write the failing tests** `internal/cli/env_test.go`:

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

const cliSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// servePreviews restarts the harness API with previews on fakes; services
// must be registered with --repo https://github.com/showmino/<name>.
func (h *harness) servePreviews(available bool) *server.Cluster {
	h.t.Helper()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	images := &envtest.Images{}
	images.Add("ghcr.io/showmino/payments:1a2b3c4")
	h.envs = server.NewEnvironments(server.EnvOptions{
		Cluster: cluster, Images: images, Catalog: h.catalog, Operations: h.ops,
		GitHub: &envtest.GitHub{Refs: map[string]string{"main": cliSHA}},
		Org:    "showmino", Domain: "preview.example",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, ReadyTimeout: 300 * time.Millisecond, DestroyTimeout: 5 * time.Second,
	})
	h.startAPI(h.authn)
	if available {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { envtest.MarkAvailable(ctx, cluster.Kube, nil) })
		h.t.Cleanup(func() { cancel(); wg.Wait() })
	}
	return cluster
}

func TestEnvLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(true)
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	r := h.ok("env", "create", "pr-1", "--service", "payments", "--dry-run")
	if !strings.HasPrefix(r.out, "Would create environments/pr-1 for services/payments at main (1a2b3c4), expiring in 24h:\n") ||
		!strings.Contains(r.out, "  url:   http://pr-1.preview.example\n") || !strings.Contains(r.out, "  image: ghcr.io/showmino/payments:1a2b3c4\n") {
		t.Errorf("dry run:\n%s", r.out)
	}
	h.fails(3, "env", "get", "pr-1") // the dry run created nothing

	r = h.ok("env", "create", "pr-1", "--service", "payments")
	for _, step := range []string{"resolve", "image", "namespace", "apply", "ready"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	if !strings.Contains(r.out, "state       ready") || !strings.Contains(r.out, "url         http://pr-1.preview.example") {
		t.Errorf("printed environment:\n%s", r.out)
	}
	h.fails(6, "env", "create", "pr-1", "--service", "payments") // exists

	golden(t, "env_list_table", h.ok("env", "list").out)
	if r := h.ok("env", "list", "-o", "json"); !strings.Contains(r.out, `"name": "environments/pr-1"`) || !strings.Contains(r.out, `"state": "STATE_READY"`) {
		t.Errorf("list json:\n%s", r.out)
	}
	get := h.ok("env", "get", "pr-1").out
	golden(t, "env_get_table", get[:strings.Index(get, "expires")]) // expiry and operation id vary
	h.ok("env", "get", "environments/pr-1", "-o", "name")

	if r := h.ok("env", "extend", "pr-1", "--ttl", "48h"); !strings.HasPrefix(r.out, "environments/pr-1 expires ") || !strings.HasSuffix(r.out, " (in 47h)\n") {
		t.Errorf("extend: %q", r.out)
	}
	h.fails(2, "env", "extend", "pr-1")                   // --ttl required
	h.fails(2, "env", "extend", "pr-1", "--ttl", "200h") // over 168h

	if r := h.ok("env", "destroy", "pr-1", "--dry-run"); r.out != "Would destroy environments/pr-1 (namespace preview-pr-1)\n" {
		t.Errorf("destroy dry run: %q", r.out)
	}
	h.fails(2, "env", "destroy", "pr-1") // no --yes, no terminal
	if r := h.ok("env", "destroy", "pr-1", "--yes"); r.out != "environments/pr-1 destroyed\n" || !strings.Contains(r.err, "  destroy") {
		t.Errorf("destroy: %v", r)
	}
	h.fails(3, "env", "get", "pr-1")
	if r := h.ok("env", "list"); !strings.Contains(r.err, "No environments.") {
		t.Errorf("empty list: %v", r)
	}
	if r := h.ok("env", "list", "-o", "json"); !strings.Contains(r.out, `"environments": []`) {
		t.Errorf("empty json: %q", r.out)
	}
}

func TestEnvCreateNoWaitAndFailures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(false) // never ready: creates fail not_ready after 300ms
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	r := h.ok("env", "create", "a", "--service", "payments", "--wait=false")
	if !strings.HasPrefix(r.out, "operations/op_") || !strings.Contains(r.err, "Watch it with: forge op watch op_") {
		t.Errorf("--wait=false: %v", r)
	}
	r = h.fails(7, "env", "create", "b", "--service", "payments")
	if !strings.Contains(r.err, "(not_ready)") || !strings.Contains(r.err, "hint: see 'forge env get b'") {
		t.Errorf("not ready: %s", r.err)
	}
	h.fails(2, "env", "create", "Bad_Name", "--service", "payments")
	h.fails(2, "env", "create", "c") // no --service outside a repo
	h.fails(2, "env", "create", "c", "--service", "payments", "--ttl", "30m")
	h.fails(3, "env", "create", "c", "--service", "nope")
	h.fails(3, "env", "create", "c", "--service", "payments", "--ref", "nope")
}

func TestEnvServiceFromManifest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(true)
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("init", "--team", "billing", "--name", "payments")
	if r := h.ok("env", "create", "m", "--dry-run"); !strings.Contains(r.out, "for services/payments") {
		t.Errorf("service from forge.yaml: %q", r.out)
	}
}

func TestEnvPreviewsOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(6, "env", "list"); !strings.Contains(r.err, "previews are not configured") {
		t.Errorf("off: %s", r.err)
	}
}

func TestUntil(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		-time.Second: "expired", 0: "expired", 30 * time.Second: "in <1m", 5 * time.Minute: "in 5m",
		23*time.Hour + 59*time.Minute: "in 23h", 50 * time.Hour: "in 2d",
	} {
		if got := until(d); got != want {
			t.Errorf("until(%v) = %q, want %q", d, got, want)
		}
	}
}
```

Write `internal/cli/testdata/env_list_table.golden`:

```
NAME   SERVICE    STATE   REF    COMMIT    EXPIRES   URL
pr-1   payments   ready   main   1a2b3c4   in 23h    http://pr-1.preview.example
```

and `internal/cli/testdata/env_get_table.golden`:

```
FIELD       VALUE
name        environments/pr-1
service     services/payments
state       ready
ref         main
commit      1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d
image       ghcr.io/showmino/payments:1a2b3c4
url         http://pr-1.preview.example
namespace   preview-pr-1
creator     ana
```

  If the table writer's spacing differs, regenerate with `go test ./internal/cli -run TestEnvLifecycle -update` and check the diff is spacing only.

- [ ] **Step 3:** `go test ./internal/cli -run 'TestEnv|TestUntil'`. Expected: FAIL (`unknown command "env"`, and `undefined: until`).

- [ ] **Step 4: Write `internal/cli/env.go`**

```go
package cli

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

var envIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// envName accepts "x" or "environments/x" and returns "environments/x".
func envName(s string) (string, error) {
	id := strings.TrimPrefix(s, "environments/")
	if !envIDPattern.MatchString(id) {
		return "", exitcode.Usagef("%q is not a valid environment name (lowercase letters, digits and '-', at most 40)", s)
	}
	return "environments/" + id, nil
}

// until reads time left coarsely: "in 23h", "in 5m", "in <1m", "expired".
func until(d time.Duration) string {
	if d <= 0 {
		return "expired"
	}
	return "in " + age(d)
}

func newEnvCmd(f *Factory) *cobra.Command {
	cmd := group("env", "Create and manage preview environments", "envs", "environment", "environments")
	cmd.Long = `A preview environment runs a service's CI image for one Git ref in its own
Kubernetes namespace, with a URL, until its TTL runs out. Creating and
destroying one are operations: forge watches them, and 'forge op watch'
picks them up again.`
	cmd.AddCommand(newEnvCreateCmd(f), newEnvListCmd(f), newEnvGetCmd(f), newEnvExtendCmd(f), newEnvDestroyCmd(f))
	return cmd
}

func newEnvCreateCmd(f *Factory) *cobra.Command {
	var service, ref string
	var ttl time.Duration
	var dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Run a service's CI image in a new preview environment",
		Long: `Create a preview environment: the Forge API resolves --ref in the service's
repo, waits until CI has pushed the image for that commit, creates the
namespace preview-<name> and waits until the service is ready. Inside a
service repo, --service defaults to forge.yaml's service.`,
		Example: `  forge env create pr-142 --service payments --ref feature/x --ttl 48h
  forge env create demo                  # in a service repo
  forge env create demo --wait=false     # then: forge op watch <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if service == "" {
				m, _, err := f.Manifest()
				if err != nil {
					return err
				}
				if m == nil {
					return exitcode.Usagef("--service is required outside a service repo")
				}
				service = m.Service.Name
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Environments.CreateEnvironment(ctx, connect.NewRequest(&forgev1.CreateEnvironmentRequest{
				EnvironmentId: strings.TrimPrefix(name, "environments/"),
				Service:       "services/" + strings.TrimPrefix(service, "services/"),
				Ref:           ref,
				Ttl:           durationpb.New(ttl),
				RequestId:     client.NewRequestID(),
				ValidateOnly:  dryRun,
			}))
			if err != nil {
				return err
			}
			env := resp.Msg.GetEnvironment()
			if dryRun {
				if format != output.Table {
					return output.Print(f.IO.Out, format, output.View{Names: []string{env.GetName()}, Data: env})
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would create %s for %s at %s (%s), expiring in %s:\n  url:   %s\n  image: %s\n",
					env.GetName(), env.GetService(), env.GetRef(), short(env.GetCommit()), age(ttl), env.GetUrl(), env.GetImage())
				return err
			}
			op := resp.Msg.GetOperation()
			if !wait {
				return f.printStarted(format, op)
			}
			if op, err = f.watchOperation(ctx, c, op.GetName()); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return exitcode.From(err).WithHint(fmt.Sprintf("see 'forge env get %s'", strings.TrimPrefix(name, "environments/")))
			}
			got, err := c.Environments.GetEnvironment(ctx, connect.NewRequest(&forgev1.GetEnvironmentRequest{Name: name}))
			if err != nil {
				return err
			}
			return f.printEnv(format, got.Msg.GetEnvironment())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&service, "service", "", "service to run (default: forge.yaml's service)")
	fl.StringVar(&ref, "ref", "main", "branch, tag or commit to run")
	fl.DurationVar(&ttl, "ttl", 24*time.Hour, "lifetime, 1h to 168h")
	fl.BoolVar(&dryRun, "dry-run", false, "check and resolve without creating anything")
	fl.BoolVar(&wait, "wait", true, "watch the create until it ends")
	return cmd
}

// printStarted reports an operation started with --wait=false: its name,
// and on stderr how to watch it.
func (f *Factory) printStarted(format output.Format, op *forgev1.Operation) error {
	f.infof("Watch it with: forge op watch %s", opID(op.GetName()))
	if format != output.Table {
		return f.printOperation(format, op)
	}
	_, err := fmt.Fprintln(f.IO.Out, op.GetName())
	return err
}

// short is a commit's first 7 characters.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func stateName(s forgev1.Environment_State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATE_"))
}

func (f *Factory) expires(env *forgev1.Environment) string {
	if env.GetExpireTime() == nil {
		return "-"
	}
	return until(env.GetExpireTime().AsTime().Sub(f.Now()))
}

// printEnv prints one environment.
func (f *Factory) printEnv(format output.Format, env *forgev1.Environment) error {
	if format != output.Table {
		return output.Print(f.IO.Out, format, output.View{Names: []string{env.GetName()}, Data: env})
	}
	rows := [][]string{
		{"name", env.GetName()},
		{"service", env.GetService()},
		{"state", stateName(env.GetState())},
	}
	if env.GetStateMessage() != "" {
		rows = append(rows, []string{"message", env.GetStateMessage()})
	}
	rows = append(rows,
		[]string{"ref", env.GetRef()},
		[]string{"commit", env.GetCommit()},
		[]string{"image", env.GetImage()},
		[]string{"url", env.GetUrl()},
		[]string{"namespace", env.GetNamespace()},
		[]string{"creator", env.GetCreator()},
		[]string{"expires", env.GetExpireTime().AsTime().UTC().Format(time.RFC3339) + " (" + f.expires(env) + ")"},
	)
	if env.GetOperation() != "" {
		rows = append(rows, []string{"operation", env.GetOperation()})
	}
	return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows})
}

func newEnvListCmd(f *Factory) *cobra.Command {
	var service string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List preview environments",
		Example: `  forge env list
  forge env list --service payments -o json`,
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
			req := &forgev1.ListEnvironmentsRequest{}
			if service != "" {
				req.Service = "services/" + strings.TrimPrefix(service, "services/")
			}
			resp, err := c.Environments.ListEnvironments(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			envs := resp.Msg.GetEnvironments()
			if len(envs) == 0 && format == output.Table {
				f.infof("No environments.")
				return nil
			}
			v := output.View{Headers: []string{"name", "service", "state", "ref", "commit", "expires", "url"}, Data: resp.Msg}
			if len(envs) == 0 {
				v.Data = map[string]any{"environments": []any{}} // keep '.environments[]' working in jq
			}
			for _, e := range envs {
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(e.GetName(), "environments/"), strings.TrimPrefix(e.GetService(), "services/"),
					stateName(e.GetState()), e.GetRef(), short(e.GetCommit()), f.expires(e), e.GetUrl(),
				})
				v.Names = append(v.Names, e.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only environments of this service")
	return cmd
}

func newEnvGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <name>",
		Short:   "Show a preview environment",
		Example: `  forge env get pr-142`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
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
			resp, err := c.Environments.GetEnvironment(ctx, connect.NewRequest(&forgev1.GetEnvironmentRequest{Name: name}))
			if err != nil {
				return err
			}
			return f.printEnv(format, resp.Msg.GetEnvironment())
		},
	}
}

func newEnvExtendCmd(f *Factory) *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:     "extend <name> --ttl <duration>",
		Short:   "Set a preview environment to expire after --ttl from now",
		Example: `  forge env extend pr-142 --ttl 48h`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			if ttl == 0 {
				return exitcode.Usagef("--ttl is required, for example --ttl 48h")
			}
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
			resp, err := c.Environments.ExtendEnvironment(ctx, connect.NewRequest(&forgev1.ExtendEnvironmentRequest{Name: name, Ttl: durationpb.New(ttl)}))
			if err != nil {
				return err
			}
			env := resp.Msg.GetEnvironment()
			if format != output.Table {
				return f.printEnv(format, env)
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s expires %s (%s)\n", env.GetName(), env.GetExpireTime().AsTime().UTC().Format(time.RFC3339), f.expires(env))
			return err
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "new lifetime from now, 1h to 168h")
	return cmd
}

func newEnvDestroyCmd(f *Factory) *cobra.Command {
	var yes, dryRun, wait bool
	cmd := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Remove a preview environment",
		Long: `Remove a preview environment and its namespace. A create still running for
it is cancelled first. Asks for the name unless --yes.`,
		Example: `  forge env destroy pr-142 --yes
  forge env destroy pr-142 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := envName(args[0])
			if err != nil {
				return err
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			if !dryRun {
				if err := f.confirmDestructive("destroy", name, yes); err != nil {
					return err
				}
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Environments.DestroyEnvironment(ctx, connect.NewRequest(&forgev1.DestroyEnvironmentRequest{Name: name, ValidateOnly: dryRun}))
			if err != nil {
				return err
			}
			if dryRun {
				if format != output.Table {
					return f.printEnv(format, resp.Msg.GetEnvironment())
				}
				_, err := fmt.Fprintf(f.IO.Out, "Would destroy %s (namespace %s)\n", name, resp.Msg.GetEnvironment().GetNamespace())
				return err
			}
			op := resp.Msg.GetOperation()
			if !wait {
				return f.printStarted(format, op)
			}
			if op, err = f.watchOperation(ctx, c, op.GetName()); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			if format != output.Table {
				return f.printOperation(format, op)
			}
			_, err = fmt.Fprintf(f.IO.Out, "%s destroyed\n", name)
			return err
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check without destroying anything")
	cmd.Flags().BoolVar(&wait, "wait", true, "watch the destroy until it ends")
	return cmd
}
```

**Notes:**
- **`opFailure` and the hint:** `opFailure` returns an `*exitcode.Error`. `exitcode.From(err)` keeps it as is, and `.WithHint` adds the hint.
- **Extend's `(in 47h)`:** the test expects 47h because `until` rounds down: 48h minus a few milliseconds of test time reads `47h`. That holds when `age` floors hours, which milestone 1's `age` does (`int(d/time.Hour)`). If it does not, adjust the test's expectation to what `age` gives, not the code.
- **`printStarted`:** `--wait=false` prints the bare operation name in table format, so scripts can capture it, rather than `printOperation`'s `operations/<id> running`.

- [ ] **Step 5: Register the command.** In `internal/cli/root.go`, change the core list to `newServiceCmd(f), newTemplateCmd(f), newCreateCmd(f), newEnvCmd(f), newOpCmd(f)`.

- [ ] **Step 6:** `go test -race ./internal/cli`. Expected: PASS, every CLI test included.

- [ ] **Step 7:** `make build test lint`, then `git add internal/cli && git commit -m "cli: forge env create, list, get, extend and destroy"`.

---

### Task 10: Docs and smoke cases

**Files:**
- Modify: `.claude/skills/forge/scripts/cli-smoke.sh`, `.claude/skills/forge/SKILL.md`, `README.md`, `docs/plans/2026-10-08-phase-3-m2-environments.md` (tick the boxes)

- [ ] **Step 1: Smoke cases.** In `cli-smoke.sh`, after the `#### operations` block, add these cases. They need previews on the API and no side effects. The service `payments` is registered at the top of the script, but without a repo URL.

```bash
echo; echo "#### environments (no side effects)"
t 0 "env list" -- f env list
t 0 "env list json" -- f env list -o json
t 3 "env get missing" -- f env get nope-env
t 2 "env get bad name" -- f env get Bad_Name
t 6 "env create, service without a repo" -- f env create smoke-env --service payments --dry-run
t 2 "env create bad ttl" -- f env create smoke-env --service payments --ttl 30m
t 2 "env destroy without --yes (no tty)" -- f env destroy nope-env
t 3 "env destroy missing" -- f env destroy nope-env --yes
```

Also update the header comment's last sentence to say that the script exercises `forge env` too and needs previews on the API.

- [ ] **Step 2: Docs.**
  - **`SKILL.md`:** in the Phase 3 bullet, add after the milestone 1 sentence: `Milestone 2: preview environments (EnvironmentService, internal/server/env_*.go, internal/registry, forge env create/list/get/extend/destroy; forge-api --previews) per docs/plans/2026-10-08-phase-3-m2-environments.md.`
  - **README:** after the `forge op` paragraph, add a blank line and:

```
`forge env create <name> --service <service>` runs the service's CI image for
`--ref` (default `main`) in its own namespace with a URL, until `--ttl` (default
24h) runs out; `forge env list / get / extend / destroy` manage them. forge-api
serves them with `--previews` (see the k3s section).
```

- [ ] **Step 3: Gate and commit** (no push; the controller pushes after the final review):

```bash
make build test lint
git add -A .claude README.md docs
git commit -m "docs: Phase 3 milestone 2 (preview environments)"
```
