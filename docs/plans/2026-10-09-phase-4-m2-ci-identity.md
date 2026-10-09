# Phase 4 Milestone 2: CI Workload Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A GitHub Actions job of a Forge service's repo can run `forge deploy start --env staging` with its own OIDC token, and do nothing else.

**Architecture:** forge-api routes each bearer token by its unverified `iss` claim: GitHub's issuer goes to a new `GitHubOIDCAuthenticator`, everything else to the existing authenticator (Dex or the static dev token). The GitHub authenticator verifies the token, maps its claims to a `Principal` carrying a `Workload` (repo, service, sha) and the single permission `deploy:<service>:staging`. The auth interceptor lets a workload call only four RPCs, and those handlers narrow it further: its own service, staging only, its token's commit, its own operations. The CLI fetches the Actions token itself when the runner offers one.

**Tech Stack:** Go 1.27.2, ConnectRPC, `github.com/coreos/go-oidc/v3` v3.11.0 (`oidc.NewRemoteKeySet`, `oidc.NewVerifier`), `github.com/go-jose/go-jose/v4` (tests only, already used by `server_test.go`), `golang.org/x/oauth2` v0.30.0 (`ReuseTokenSourceWithExpiry`).

**Spec:** `docs/specs/2026-10-09-phase-4-deployments-design.md`, section 3 (Authentication and permissions), section 4 ("In GitHub Actions"), section 5 (`FORGE_GITHUB_OIDC_AUDIENCE`), section 6 (errors, security, testing).

## Global Constraints

- CI issuer `https://token.actions.githubusercontent.com`; audience `forge`, overridable with `FORGE_GITHUB_OIDC_AUDIENCE`; enabled only when deploys are on (`--deploys`).
- The issuer in the token picks the verifier. A token whose `iss` matches neither is `Unauthenticated`.
- `repository_owner` must be the configured org (`showmino`), compared case-insensitively.
- `repository` maps to the catalog entry whose `repo_url` is `https://github.com/<org>/<repo>`; none → `PermissionDenied`.
- `ref` must be the repo's default branch (`refs/heads/<default>`), read through the App and cached for 10 minutes.
- A CI deploy always deploys exactly the token's `sha`; its request's `ref` is ignored.
- CI Principal: subject `github:<org>/<repo>`, one permission `deploy:<service>:staging`.
- A CI principal may call `StartDeploy` (own service, staging), `GetDeployment` (own service's staging deployment), `GetOperation` and `WatchOperation` (operations it started). Every other RPC is `PermissionDenied`, including all other reads.
- A failed check names the claim that didn't match. Token values are never logged or printed.
- Exit codes: bad, expired or untrusted token → `Unauthenticated` (4); CI token outside its scope → `PermissionDenied` (5).
- CLI: when `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN` are set and `FORGE_TOKEN` isn't, request an OIDC token with `audience=forge` and use it as the bearer. With `GITHUB_SHA` set and no `--ref`, `deploy start` deploys `GITHUB_SHA`.
- Conventions: no package globals other than constant tables, commands get everything through `Deps`, `make test lint` before every commit, golangci-lint v2.14.0.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.

## Review Focus

1. **The CI token expires mid-deploy.** GitHub's token lives 5 to 10 minutes; a deploy can wait 10 minutes for the image and 10 for Argo. The CLI must fetch a fresh token for every call made after the old one is within a minute of expiry (reattaching the watch, the final `GetDeployment`), not reuse the first one. Pinned in Task 5 (`TestActionsTokenRefetchesNearExpiry`).
2. **GitHub is unreachable while forge-api checks the default branch.** That is `Unavailable` (exit 1, retry), not `Unauthenticated` (exit 4, "your token is bad"), and the failure must not be cached for 10 minutes. Pinned in Task 1 (`TestGitHubOIDCBranchLookup`).
3. **The workflow lacks `permissions: id-token: write`.** The runner then sets no Actions token variables, and the CLI would say "run 'forge auth login'", which means nothing in CI. With `GITHUB_ACTIONS=true` the hint names `id-token: write`. Pinned in Task 5 (`TestActionsTokenHints`).
4. **Case in repo names.** GitHub names are case-insensitive: a token for `showmino/Payments` must match the catalog's `https://github.com/showmino/payments`. Pinned in Task 1 (case row of `TestGitHubOIDCClaims`).
5. **Claims missing or empty.** A token without `sha`, `ref` or `repository` (another issuer's shape, a hand-made token) is denied naming that claim, never treated as "deploy main". Pinned in Task 1 (empty-claim rows of `TestGitHubOIDCClaims`).

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/github/repos.go` (modify) | `Client.DefaultBranch`: a repo's default branch. |
| `internal/server/catalog.go` (modify) | `Catalog.servicesForRepo`: catalog ids whose `repo_url` is a given GitHub repo. |
| `internal/server/authn.go` (modify) | `Workload`, `Principal.Workload`, `PermDeploy`, `workloadOf`; `ByIssuer`; the interceptor's workload allowlist and pass-through of `*connect.Error`. |
| `internal/server/ci_auth.go` (create) | `GitHubOIDCAuthenticator`: verify a GitHub Actions token and map its claims to a workload Principal. |
| `internal/server/ci_scope.go` (create) | What a workload may do inside the four RPCs: `startRef`, `ownDeployment`, `ownOperation`, `requestKey`. |
| `internal/server/deploy_work.go`, `deploys.go`, `operation_service.go` (modify) | Call the `ci_scope.go` checks. |
| `internal/envtest/envtest.go` (modify) | The fake GitHub resolves a known commit SHA as a ref, as GitHub does. |
| `cmd/forge-api/deploys.go`, `main.go` (modify) | `withCITokens`: wrap the authenticator when deploys are on. |
| `internal/auth/actions.go` (create) | `ActionsToken`: a token source over the Actions OIDC endpoint. |
| `internal/cli/factory.go`, `deploy.go` (modify) | Pick the Actions token up; hints; `GITHUB_SHA` as the default ref. |
| `README.md`, `.claude/skills/forge/SKILL.md` (modify) | Docs and status. |

---

### Task 1: GitHub OIDC authenticator

**Files:**
- Modify: `internal/github/repos.go` (add `DefaultBranch` after `RepoExists`)
- Test: `internal/github/repos_test.go`
- Modify: `internal/server/catalog.go` (add `servicesForRepo` after `get`)
- Modify: `internal/server/authn.go` (add `Workload`, `Principal.Workload`, `PermDeploy`, `workloadOf`)
- Create: `internal/server/ci_auth.go`
- Test: `internal/server/ci_auth_test.go`

**Interfaces:**
- Consumes: `repoOf(repoURL, org string) (owner, repo string, ok bool)` in `environments.go`; `Catalog.RegisterService`; `writerCtx()` and `envSHA` from existing tests.
- Produces:
  - `func (c *github.Client) DefaultBranch(ctx context.Context, owner, name string) (string, error)`
  - `func (c *Catalog) servicesForRepo(org, repo string) []string`
  - `type Workload struct { Repository, Service, SHA string }`; `Principal.Workload *Workload`
  - `func PermDeploy(service, env string) string` → `"deploy:<service>:<env>"`
  - `func workloadOf(ctx context.Context) *Workload`
  - `const GitHubIssuer = "https://token.actions.githubusercontent.com"`
  - `type DefaultBranches interface { DefaultBranch(ctx context.Context, owner, repo string) (string, error) }`
  - `type GitHubOIDCOptions struct { Issuer, JWKSURL, Audience, Org string; Catalog *Catalog; Branches DefaultBranches; BranchTTL time.Duration; Now func() time.Time }`
  - `func NewGitHubOIDCAuthenticator(ctx context.Context, o GitHubOIDCOptions) *GitHubOIDCAuthenticator`
  - `func (a *GitHubOIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error)`: a plain error for a token that fails verification (the interceptor makes it `Unauthenticated`), a `*connect.Error` with `PermissionDenied` for a claim that doesn't match, `Unavailable` when the default branch can't be read.
  - test helpers `newActionsIssuer(t)`, `(*actionsIssuer).claims(now, over)`, `(*actionsIssuer).sign(t, key, claims)`, `ciCatalog(t, map[string]string)`, `ciClock`, `fakeBranches` (used again in Task 2).

- [ ] **Step 1: Write the failing test for `DefaultBranch`**

Append to `internal/github/repos_test.go`:

```go
func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/svc", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"full_name":"showmino/svc","default_branch":"trunk"}`)
	})
	mux.HandleFunc("GET /repos/showmino/blank", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"full_name":"showmino/blank"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()
	if b, err := c.DefaultBranch(ctx, "showmino", "svc"); err != nil || b != "trunk" {
		t.Errorf("svc: %q %v", b, err)
	}
	if _, err := c.DefaultBranch(ctx, "showmino", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing repo: %v", err)
	}
	if _, err := c.DefaultBranch(ctx, "showmino", "blank"); err == nil {
		t.Error("no default_branch: want an error")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && go test ./internal/github -run TestDefaultBranch`
Expected: FAIL, `c.DefaultBranch undefined`.

- [ ] **Step 3: Implement `DefaultBranch`**

In `internal/github/repos.go`, after `RepoExists`:

```go
// DefaultBranch returns owner/name's default branch. An unknown repo is ErrNotFound.
func (c *Client) DefaultBranch(ctx context.Context, owner, name string) (string, error) {
	var r Repo
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, name), nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("GitHub returned no default branch for %s/%s", owner, name)
	}
	return r.DefaultBranch, nil
}
```

Run: `go test ./internal/github -run TestDefaultBranch`
Expected: PASS.

- [ ] **Step 4: Write the failing authenticator tests**

Create `internal/server/ci_auth_test.go`:

```go
package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// actionsIssuer plays token.actions.githubusercontent.com: a JWKS endpoint at
// /.well-known/jwks and the key that signs test tokens.
type actionsIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newActionsIssuer(t *testing.T) *actionsIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &actionsIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "gh1", Algorithm: "RS256", Use: "sig"}}})
	})
	i.srv = httptest.NewServer(mux)
	t.Cleanup(i.srv.Close)
	return i
}

// claims are a push to main of showmino/payments at envSHA, valid for five
// minutes from now, changed by over; a nil value in over deletes the claim.
func (i *actionsIssuer) claims(now time.Time, over map[string]any) map[string]any {
	c := map[string]any{
		"iss": i.srv.URL, "aud": "forge", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"sub":              "repo:showmino/payments:ref:refs/heads/main",
		"repository":       "showmino/payments",
		"repository_owner": "showmino",
		"ref":              "refs/heads/main",
		"sha":              envSHA,
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// sign signs claims with key (nil: the issuer's own key).
func (i *actionsIssuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	if key == nil {
		key = i.key
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "gh1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ciCatalog registers services (id → repo_url).
func ciCatalog(t *testing.T, services map[string]string) *Catalog {
	t.Helper()
	c := NewCatalog(nil)
	for id, repo := range services {
		if _, err := c.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
			ServiceId: id, Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: repo},
		})); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// ciClock is a settable clock.
type ciClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ciClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ciClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeBranches answers DefaultBranch from branch ("owner/repo" → name, default
// "main") or with err, counting calls.
type fakeBranches struct {
	mu     sync.Mutex
	branch map[string]string
	err    error
	calls  int
}

func (f *fakeBranches) DefaultBranch(_ context.Context, owner, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if b, ok := f.branch[owner+"/"+repo]; ok {
		return b, nil
	}
	return "main", nil
}

func (f *fakeBranches) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBranches) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newCIAuth(t *testing.T, iss *actionsIssuer, cat *Catalog, br *fakeBranches, clk *ciClock) *GitHubOIDCAuthenticator {
	t.Helper()
	return NewGitHubOIDCAuthenticator(t.Context(), GitHubOIDCOptions{
		Issuer: iss.srv.URL, Org: "showmino", Catalog: cat, Branches: br, Now: clk.Now,
	})
}

func TestGitHubOIDCClaims(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	cat := ciCatalog(t, map[string]string{
		"payments": "https://github.com/showmino/payments",
		"trunked":  "https://github.com/showmino/trunked",
		"twin-a":   "https://github.com/showmino/twin",
		"twin-b":   "https://github.com/showmino/twin",
	})
	br := &fakeBranches{branch: map[string]string{"showmino/trunked": "trunk"}}
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, cat, br, clk)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	// code is the result's class: CodeUnknown means a plain error, which the
	// interceptor turns into Unauthenticated; 0 means success.
	for _, tc := range []struct {
		name string
		over map[string]any
		key  *rsa.PrivateKey
		code connect.Code
		want string // in the error message, or the Principal's service
	}{
		{name: "push to main", want: "payments"},
		{name: "case-insensitive repo and owner", over: map[string]any{"repository": "Showmino/Payments", "repository_owner": "ShowMino"}, want: "payments"},
		{name: "other default branch", over: map[string]any{"repository": "showmino/trunked", "ref": "refs/heads/trunk"}, want: "trunked"},
		{name: "wrong audience", over: map[string]any{"aud": "sts.amazonaws.com"}, code: connect.CodeUnknown},
		{name: "wrong issuer", over: map[string]any{"iss": "https://evil.example"}, code: connect.CodeUnknown},
		{name: "expired", over: map[string]any{"exp": clk.Now().Add(-time.Minute).Unix()}, code: connect.CodeUnknown},
		{name: "other key", key: otherKey, code: connect.CodeUnknown},
		{name: "other owner", over: map[string]any{"repository_owner": "evil", "repository": "evil/payments"}, code: connect.CodePermissionDenied, want: "repository_owner"},
		{name: "repository in another org", over: map[string]any{"repository": "evil/payments"}, code: connect.CodePermissionDenied, want: "repository"},
		{name: "unknown repository", over: map[string]any{"repository": "showmino/nope"}, code: connect.CodePermissionDenied, want: "not a Forge service"},
		{name: "repo of two services", over: map[string]any{"repository": "showmino/twin"}, code: connect.CodePermissionDenied, want: "twin-a, twin-b"},
		{name: "feature branch", over: map[string]any{"ref": "refs/heads/feature"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "tag", over: map[string]any{"ref": "refs/tags/v1.0.0"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "pull request", over: map[string]any{"ref": "refs/pull/7/merge"}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "no ref", over: map[string]any{"ref": nil}, code: connect.CodePermissionDenied, want: "ref"},
		{name: "no sha", over: map[string]any{"sha": nil}, code: connect.CodePermissionDenied, want: "sha"},
		{name: "sha not a commit", over: map[string]any{"sha": "main"}, code: connect.CodePermissionDenied, want: "sha"},
		{name: "no repository", over: map[string]any{"repository": nil}, code: connect.CodePermissionDenied, want: "repository"},
		{name: "no owner", over: map[string]any{"repository_owner": nil}, code: connect.CodePermissionDenied, want: "repository_owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := iss.sign(t, tc.key, iss.claims(clk.Now(), tc.over))
			p, err := a.Authenticate(context.Background(), token)
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if p.Workload == nil || p.Workload.Service != tc.want || p.Workload.SHA != envSHA {
					t.Fatalf("workload: %+v", p.Workload)
				}
				if !strings.HasPrefix(p.Subject, "github:") || len(p.Permissions) != 1 || p.Permissions[0] != PermDeploy(tc.want, "staging") {
					t.Fatalf("principal: %+v", p)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error, got %+v", p)
			}
			if got := connect.CodeOf(err); got != tc.code {
				t.Fatalf("code %v, want %v: %v", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token) {
				t.Error("the error contains the token")
			}
		})
	}
}

func TestGitHubOIDCSubject(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, ciCatalog(t, map[string]string{"payments": "https://github.com/showmino/payments"}), &fakeBranches{}, clk)
	p, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
	if err != nil {
		t.Fatal(err)
	}
	want := Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA}
	if p.Subject != "github:showmino/payments" || p.Email != "" || len(p.Groups) != 0 || *p.Workload != want {
		t.Errorf("principal: %+v %+v", p, p.Workload)
	}
}

func TestGitHubOIDCBranchLookup(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	br := &fakeBranches{}
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, ciCatalog(t, map[string]string{"payments": "https://github.com/showmino/payments"}), br, clk)
	auth := func() error {
		_, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
		return err
	}

	br.set(errors.New("connection refused"))
	if err := auth(); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("GitHub down: %v (want Unavailable)", err)
	}
	br.set(nil)
	if err := auth(); err != nil {
		t.Fatalf("GitHub back, failure not cached: %v", err)
	}
	calls := br.count()
	if err := auth(); err != nil || br.count() != calls {
		t.Fatalf("second token within 10 minutes: %v, %d lookups (want %d)", err, br.count(), calls)
	}
	clk.Add(11 * time.Minute)
	if err := auth(); err != nil || br.count() != calls+1 {
		t.Fatalf("after 11 minutes: %v, %d lookups (want %d)", err, br.count(), calls+1)
	}
}

func TestGitHubOIDCDefaults(t *testing.T) {
	t.Parallel()
	a := NewGitHubOIDCAuthenticator(t.Context(), GitHubOIDCOptions{Org: "showmino", Catalog: NewCatalog(nil), Branches: &fakeBranches{}})
	got := map[string]any{"issuer": a.o.Issuer, "jwks": a.o.JWKSURL, "audience": a.o.Audience, "ttl": a.o.BranchTTL}
	want := map[string]any{
		"issuer": GitHubIssuer, "jwks": GitHubIssuer + "/.well-known/jwks", "audience": "forge", "ttl": 10 * time.Minute,
	}
	if !maps.Equal(got, want) {
		t.Errorf("defaults %v, want %v", got, want)
	}
}
```

- [ ] **Step 5: Run them to see them fail**

Run: `go test ./internal/server -run 'TestGitHubOIDC'`
Expected: FAIL to compile: `undefined: NewGitHubOIDCAuthenticator`, `GitHubOIDCOptions`, `PermDeploy`, `Workload`.

- [ ] **Step 6: Add `Workload`, `PermDeploy` and `workloadOf` to `authn.go`**

In `internal/server/authn.go`, replace the `Principal` type with:

```go
// Principal is an authenticated caller.
type Principal struct {
	Subject     string
	Email       string
	Groups      []string
	Permissions []string
	// Workload is set for a CI caller. Its Permissions come from its token,
	// not from the Authorizer.
	Workload *Workload
}

// Workload is a CI job of a service's repo, from its GitHub Actions token.
type Workload struct {
	// Repository is "<org>/<repo>" as the token spells it.
	Repository string
	// Service is the catalog id, without "services/".
	Service string
	// SHA is the commit the job runs at.
	SHA string
}

// PermDeploy is a CI caller's permission to deploy service to env.
func PermDeploy(service, env string) string { return "deploy:" + service + ":" + env }

// workloadOf returns the caller's Workload, or nil for a human.
func workloadOf(ctx context.Context) *Workload {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.Workload
	}
	return nil
}
```

- [ ] **Step 7: Add `servicesForRepo` to the catalog**

In `internal/server/catalog.go`, after `get`:

```go
// servicesForRepo returns the ids, sorted, of the services whose repo_url is
// https://github.com/<org>/<repo>, both compared case-insensitively.
func (c *Catalog) servicesForRepo(org, repo string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for name, s := range c.services {
		if _, r, ok := repoOf(s.GetRepoUrl(), org); ok && strings.EqualFold(r, repo) {
			ids = append(ids, strings.TrimPrefix(name, servicePrefix))
		}
	}
	slices.Sort(ids)
	return ids
}
```

(`repoOf` already compares the owner with `org` case-insensitively.)

- [ ] **Step 8: Write the authenticator**

Create `internal/server/ci_auth.go`:

```go
package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
)

// GitHubIssuer is GitHub Actions' OIDC issuer.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DefaultBranches reads a repo's default branch (*github.Client).
type DefaultBranches interface {
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
}

// GitHubOIDCOptions configure GitHubOIDCAuthenticator.
type GitHubOIDCOptions struct {
	// Issuer defaults to GitHubIssuer; JWKSURL to Issuer + "/.well-known/jwks".
	Issuer, JWKSURL string
	// Audience the tokens must carry (default "forge").
	Audience string
	// Org the repos must be in.
	Org      string
	Catalog  *Catalog
	Branches DefaultBranches
	// BranchTTL is how long a default branch is cached (default 10m).
	BranchTTL time.Duration
	Now       func() time.Time
}

// GitHubOIDCAuthenticator turns a GitHub Actions OIDC token from a Forge
// service's repo into a workload Principal that may deploy that service to
// staging at the token's commit.
type GitHubOIDCAuthenticator struct {
	o        GitHubOIDCOptions
	verifier *oidc.IDTokenVerifier

	mu       sync.Mutex
	branches map[string]branchEntry // by lowercased "owner/repo"
}

type branchEntry struct {
	name string
	at   time.Time
}

// NewGitHubOIDCAuthenticator fetches signing keys from o.JWKSURL when it first
// needs them, so it starts even while GitHub is unreachable.
func NewGitHubOIDCAuthenticator(ctx context.Context, o GitHubOIDCOptions) *GitHubOIDCAuthenticator {
	if o.Issuer == "" {
		o.Issuer = GitHubIssuer
	}
	if o.JWKSURL == "" {
		o.JWKSURL = o.Issuer + "/.well-known/jwks"
	}
	if o.Audience == "" {
		o.Audience = "forge"
	}
	if o.BranchTTL == 0 {
		o.BranchTTL = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	keys := oidc.NewRemoteKeySet(ctx, o.JWKSURL)
	return &GitHubOIDCAuthenticator{
		o:        o,
		verifier: oidc.NewVerifier(o.Issuer, keys, &oidc.Config{ClientID: o.Audience, Now: o.Now}),
		branches: map[string]branchEntry{},
	}
}

// ciDenied is a token that verified but may not do what it asks; the message names the claim.
func ciDenied(format string, a ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("CI token: "+format, a...))
}

// Authenticate verifies the token and checks its claims against the org, the
// catalog and the repo's default branch.
func (a *GitHubOIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	idt, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("invalid GitHub Actions token: %w", err)
	}
	var c struct {
		Owner      string `json:"repository_owner"`
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
	}
	if err := idt.Claims(&c); err != nil {
		return nil, fmt.Errorf("invalid GitHub Actions token claims: %w", err)
	}
	if !strings.EqualFold(c.Owner, a.o.Org) {
		return nil, ciDenied("repository_owner %q is not %s", c.Owner, a.o.Org)
	}
	owner, repo, ok := strings.Cut(c.Repository, "/")
	if !ok || !strings.EqualFold(owner, a.o.Org) || repo == "" || strings.Contains(repo, "/") {
		return nil, ciDenied("repository %q is not a repository of %s", c.Repository, a.o.Org)
	}
	ids := a.o.Catalog.servicesForRepo(a.o.Org, repo)
	switch {
	case len(ids) == 0:
		return nil, ciDenied("repository %q is not a Forge service: no catalog entry has repo_url https://github.com/%s/%s", c.Repository, a.o.Org, repo)
	case len(ids) > 1:
		return nil, ciDenied("repository %q is the repo of several services (%s)", c.Repository, strings.Join(ids, ", "))
	}
	if !shaRE.MatchString(c.SHA) {
		return nil, ciDenied("sha %q is not a commit SHA", c.SHA)
	}
	branch, err := a.defaultBranch(ctx, owner, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the default branch of %s: %w", c.Repository, err))
	}
	if c.Ref != "refs/heads/"+branch {
		return nil, ciDenied("ref %q is not the default branch of %s (refs/heads/%s)", c.Ref, c.Repository, branch)
	}
	return &Principal{
		Subject:     "github:" + c.Repository,
		Permissions: []string{PermDeploy(ids[0], "staging")},
		Workload:    &Workload{Repository: c.Repository, Service: ids[0], SHA: c.SHA},
	}, nil
}

// defaultBranch reads owner/repo's default branch, cached for BranchTTL.
// Failures are not cached.
func (a *GitHubOIDCAuthenticator) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	key := strings.ToLower(owner + "/" + repo)
	a.mu.Lock()
	e, ok := a.branches[key]
	a.mu.Unlock()
	if ok && a.o.Now().Sub(e.at) < a.o.BranchTTL {
		return e.name, nil
	}
	name, err := a.o.Branches.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.branches[key] = branchEntry{name: name, at: a.o.Now()}
	a.mu.Unlock()
	return name, nil
}
```

- [ ] **Step 9: Run the tests**

Run: `go test -race ./internal/server -run 'TestGitHubOIDC' -v`
Expected: PASS, every subtest.

If "other default branch" fails because `trunked` is looked up as `Showmino/trunked`: the fake keys on the token's spelling `showmino/trunked`, which matches; do not lowercase in the fake.

- [ ] **Step 10: Run the package and lint, then commit**

Run: `make test lint`
Expected: PASS, no lint findings.

```bash
git add internal/github/repos.go internal/github/repos_test.go internal/server/authn.go internal/server/catalog.go internal/server/ci_auth.go internal/server/ci_auth_test.go
git commit -m "server: GitHub Actions OIDC authenticator for CI workloads

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Route tokens by issuer; confine workloads in the interceptor

**Files:**
- Modify: `internal/server/authn.go` (`ByIssuer`, `unverifiedIssuer`, `workloadProcedures`, `authInterceptor`)
- Test: `internal/server/ci_auth_test.go` (append)

**Interfaces:**
- Consumes: `Workload`, `PermDeploy`, `ciDenied`, the Task 1 test helpers.
- Produces:
  - `type ByIssuer struct { Default Authenticator; Issuers map[string]Authenticator }` with `Authenticate`.
  - The interceptor: a `*connect.Error` from an Authenticator keeps its code (others stay `Unauthenticated`); a workload Principal skips `Authorizer.Grant`; a workload may call only `StartDeploy`, `GetDeployment`, `GetOperation`, `WatchOperation`, and gets `PermissionDenied` naming the procedure otherwise.
  - test helper `type authFunc func(ctx context.Context, token string) (*Principal, error)` (an `Authenticator`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/ci_auth_test.go` (add `"github.com/showmino/forge/gen/forge/v1/forgev1connect"` to the imports):

```go
// authFunc adapts a function to Authenticator.
type authFunc func(ctx context.Context, token string) (*Principal, error)

func (f authFunc) Authenticate(ctx context.Context, token string) (*Principal, error) { return f(ctx, token) }

// unsignedJWT is a JWT-shaped token with claims and no valid signature.
func unsignedJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestByIssuer(t *testing.T) {
	t.Parallel()
	var routed []string
	gh := authFunc(func(_ context.Context, token string) (*Principal, error) {
		routed = append(routed, "github")
		return &Principal{Subject: "github:showmino/payments"}, nil
	})
	b := ByIssuer{
		Default: StaticAuthenticator{"dev": {Subject: "dev"}},
		Issuers: map[string]Authenticator{GitHubIssuer: gh},
	}
	ctx := context.Background()
	if p, err := b.Authenticate(ctx, unsignedJWT(map[string]any{"iss": GitHubIssuer})); err != nil || p.Subject != "github:showmino/payments" {
		t.Errorf("GitHub token: %+v %v", p, err)
	}
	if p, err := b.Authenticate(ctx, "dev"); err != nil || p.Subject != "dev" {
		t.Errorf("non-JWT token to Default: %+v %v", p, err)
	}
	for _, tok := range []string{
		unsignedJWT(map[string]any{"iss": "https://dex.example/dex"}),
		unsignedJWT(map[string]any{"iss": GitHubIssuer + "/"}),
		"a.b.c",
		"a.!!!.c",
		"",
	} {
		if _, err := b.Authenticate(ctx, tok); err == nil {
			t.Errorf("%q: Default should reject it", tok)
		}
	}
	if len(routed) != 1 {
		t.Errorf("GitHub authenticator called %d times, want 1", len(routed))
	}
}

func TestWorkloadInterceptor(t *testing.T) {
	t.Parallel()
	ci := Principal{
		Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
		Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA},
	}
	authn := authFunc(func(_ context.Context, token string) (*Principal, error) {
		switch token {
		case "ci":
			c := ci
			return &c, nil
		case "writer":
			return &Principal{Subject: "ana", Groups: []string{"platform"}}, nil
		case "denied":
			return nil, ciDenied("ref %q is not the default branch", "refs/heads/x")
		}
		return nil, errors.New("unknown token")
	})
	h, err := New(Options{Authenticator: authn, Authorizer: Authorizer{WriterGroups: []string{"platform"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cat := forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	authc := forgev1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	opc := forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
	envc := forgev1connect.NewEnvironmentServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"ListServices": func() error {
			_, err := cat.ListServices(ctx, as("ci", &forgev1.ListServicesRequest{}))
			return err
		},
		"GetService": func() error {
			_, err := cat.GetService(ctx, as("ci", &forgev1.GetServiceRequest{Name: "services/payments"}))
			return err
		},
		"GetStatus": func() error {
			_, err := authc.GetStatus(ctx, as("ci", &forgev1.GetStatusRequest{}))
			return err
		},
		"ListOperations": func() error {
			_, err := opc.ListOperations(ctx, as("ci", &forgev1.ListOperationsRequest{}))
			return err
		},
		"CancelOperation": func() error {
			_, err := opc.CancelOperation(ctx, as("ci", &forgev1.CancelOperationRequest{Name: "operations/op_000000000000"}))
			return err
		},
		"CreateEnvironment": func() error {
			_, err := envc.CreateEnvironment(ctx, as("ci", &forgev1.CreateEnvironmentRequest{}))
			return err
		},
	} {
		err := call()
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "CI token") {
			t.Errorf("%s as CI: %v (want PermissionDenied from the interceptor)", name, err)
		}
	}

	// Allowed procedures reach their handler; deploys are off here.
	if _, err := forgev1connect.NewDeployServiceClient(srv.Client(), srv.URL).StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("StartDeploy as CI with deploys off: %v", err)
	}
	// A *connect.Error from the Authenticator keeps its code; others are Unauthenticated.
	if _, err := cat.ListServices(ctx, as("denied", &forgev1.ListServicesRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "ref") {
		t.Errorf("denied token: %v", err)
	}
	if _, err := cat.ListServices(ctx, as("bogus", &forgev1.ListServicesRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("bogus token: %v", err)
	}
	// Humans are unchanged.
	if _, err := cat.ListServices(ctx, as("writer", &forgev1.ListServicesRequest{})); err != nil {
		t.Errorf("writer: %v", err)
	}
	// Streaming calls are confined too.
	stream, err := opc.WatchOperation(ctx, as("ci", &forgev1.WatchOperationRequest{Name: "operations/op_000000000000"}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if c := connect.CodeOf(err); c == connect.CodeUnauthenticated || c == connect.CodeOK {
		t.Errorf("WatchOperation as CI should pass the interceptor and fail in the handler: %v", err)
	}
}
```

Add `"encoding/base64"` to the imports.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/server -run 'TestByIssuer|TestWorkloadInterceptor'`
Expected: FAIL to compile, `undefined: ByIssuer`.

- [ ] **Step 3: Implement `ByIssuer`**

In `internal/server/authn.go` (add `"encoding/base64"` and `"encoding/json"` to the imports), after `StaticAuthenticator`:

```go
// ByIssuer sends a JWT whose iss claim is a key of Issuers to that
// Authenticator, and every other token, JWT or not, to Default. The claim is
// read unverified: the chosen Authenticator verifies the token, issuer included.
type ByIssuer struct {
	Default Authenticator
	Issuers map[string]Authenticator
}

// Authenticate routes the token.
func (b ByIssuer) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if a, ok := b.Issuers[unverifiedIssuer(token)]; ok {
		return a.Authenticate(ctx, token)
	}
	return b.Default.Authenticate(ctx, token)
}

// unverifiedIssuer reads a JWT's iss claim without checking anything; "" when
// the token is not a JWT.
func unverifiedIssuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}
```

- [ ] **Step 4: Confine workloads in the interceptor**

In `internal/server/authn.go`, after `var public = ...`:

```go
// workloadProcedures are the only RPCs a CI caller may make; their handlers
// narrow each one to the caller's own service, staging and operations.
var workloadProcedures = []string{
	forgev1connect.DeployServiceStartDeployProcedure,
	forgev1connect.DeployServiceGetDeploymentProcedure,
	forgev1connect.OperationServiceGetOperationProcedure,
	forgev1connect.OperationServiceWatchOperationProcedure,
}
```

Pass the procedure to `authenticate`. In `WrapUnary`:

```go
		ctx, err := i.authenticate(ctx, req.Spec().Procedure, req.Header().Get("Authorization"))
```

In `WrapStreamingHandler`:

```go
		ctx, err := i.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader().Get("Authorization"))
```

Replace `authenticate` with:

```go
func (i *authInterceptor) authenticate(ctx context.Context, procedure, header string) (context.Context, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	p, err := i.authn.Authenticate(ctx, token)
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ctx, err // the Authenticator chose the code (a CI token out of scope)
		}
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if p.Workload != nil {
		if !slices.Contains(workloadProcedures, procedure) {
			return ctx, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("CI token %s may only start and follow its own staging deploy, not call %s", p.Subject, procedure))
		}
	} else {
		i.authz.Grant(p)
	}
	return context.WithValue(ctx, principalKey{}, p), nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./internal/server -run 'TestByIssuer|TestWorkloadInterceptor|TestOIDC' -v`
Expected: PASS. `WatchOperation` as CI ends `PermissionDenied` from the handler (it still requires `services.read` until Task 3), which the test accepts.

- [ ] **Step 6: Run everything, lint, commit**

Run: `make test lint`
Expected: PASS.

```bash
git add internal/server/authn.go internal/server/ci_auth_test.go
git commit -m "server: route tokens by issuer; CI workloads may call four RPCs

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Scope the four RPCs to the workload; request_id per caller

**Files:**
- Create: `internal/server/ci_scope.go`
- Modify: `internal/server/deploy_work.go` (`StartDeploy`)
- Modify: `internal/server/deploys.go` (`GetDeployment`)
- Modify: `internal/server/operation_service.go` (`GetOperation`, `WatchOperation`)
- Modify: `internal/envtest/envtest.go` (`GitHub.ResolveRef`)
- Modify: `internal/server/deploys_test.go` (fixture: CI tokens, operation client)
- Test: `internal/server/ci_scope_test.go`

**Interfaces:**
- Consumes: `workloadOf`, `PermDeploy`, `Workload` (Task 1); the interceptor (Task 2); `newDeployFixture`, `as`, `envSHA`.
- Produces:
  - `func startRef(ctx context.Context, service, env, ref string) (string, error)`: humans need `services.write` and keep `ref`; a workload needs `deploy:<svc>:<env>` and gets its token's SHA.
  - `func ownDeployment(ctx context.Context, svc, env string) error`
  - `func ownOperation(ctx context.Context, op *forgev1.Operation) error`
  - `func requestKey(ctx context.Context, id string) string`
  - Deploy fixture fields `opsClient forgev1connect.OperationServiceClient`; fixture tokens `"ci"` (payments) and `"ci-billing"` (billing).

- [ ] **Step 1: Let the fake GitHub resolve a commit SHA**

In `internal/envtest/envtest.go`, replace `GitHub.ResolveRef` with:

```go
func (g *GitHub) ResolveRef(_ context.Context, _, _, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sha, ok := g.Refs[ref]; ok {
		return sha, nil
	}
	for _, sha := range g.Refs {
		if sha == ref { // a commit some ref points at, as GitHub resolves SHAs
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref %s: %w", ref, github.ErrNotFound)
}
```

- [ ] **Step 2: Give the deploy fixture CI tokens and an operation client**

In `internal/server/deploys_test.go`, add a field to `deployFixture`:

```go
	opsClient forgev1connect.OperationServiceClient
```

In `newDeployFixture`, extend the `StaticAuthenticator`:

```go
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
			"ci": {
				Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
				Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA},
			},
			"ci-billing": {
				Subject: "github:showmino/billing", Permissions: []string{PermDeploy("billing", "staging")},
				Workload: &Workload{Repository: "showmino/billing", Service: "billing", SHA: envSHA},
			},
		},
```

and after `f.client = ...`:

```go
	f.opsClient = forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
```

`StaticAuthenticator.Authenticate` copies the Principal and shares the `Workload` pointer and `Permissions` slice. That is safe because the interceptor no longer calls `Grant` (which replaces `Permissions`) for workloads.

- [ ] **Step 3: Write the failing tests**

Create `internal/server/ci_scope_test.go`:

```go
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// waitCIOp polls until the operation is done.
func waitCIOp(t *testing.T, ops *Operations, name string) *forgev1.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op, ok := ops.Get(name); ok && op.GetDone() {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not finish", name)
	return nil
}

func TestCIStartDeploy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()

	// The request's ref is ignored: CI deploys its token's commit.
	resp, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "no-such-ref"}))
	if err != nil {
		t.Fatal(err)
	}
	op := resp.Msg.GetOperation()
	if op.GetCreator() != "github:showmino/payments" {
		t.Errorf("creator %q", op.GetCreator())
	}
	if done := waitCIOp(t, f.ops, op.GetName()); done.GetError() != nil {
		t.Fatalf("deploy failed: %v", done.GetError())
	}
	got, err := f.client.GetDeployment(ctx, as("ci", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Msg.GetDeployment(); d.GetCommit() != envSHA || d.GetDeployedBy() != "github:showmino/payments" {
		t.Errorf("deployment: commit %s by %s", d.GetCommit(), d.GetDeployedBy())
	}

	// Its own operation: readable and watchable.
	if _, err := f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: op.GetName()})); err != nil {
		t.Errorf("GetOperation own: %v", err)
	}
	stream, err := f.opsClient.WatchOperation(ctx, as("ci", &forgev1.WatchOperationRequest{Name: op.GetName()}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Errorf("WatchOperation own: %v", err)
	}
}

func TestCIScope(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	denied := func(name string, err error, want string) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want PermissionDenied naming %q)", name, err, want)
		}
	}

	_, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production"}))
	denied("production", err, "only deploy services/payments to staging")
	_, err = f.client.StartDeploy(ctx, as("ci-billing", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging"}))
	denied("another service", err, "only deploy services/billing to staging")
	_, err = f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "payments", Env: "staging"}))
	denied("malformed service name", err, "only deploy services/payments")

	// A human's deploy to production, then CI tries to read it.
	human, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production"}))
	if err != nil {
		t.Fatal(err)
	}
	hop := human.Msg.GetOperation().GetName()
	waitCIOp(t, f.ops, hop)
	_, err = f.client.GetDeployment(ctx, as("ci", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	denied("GetDeployment production", err, "deployments/payments/staging")
	_, err = f.client.GetDeployment(ctx, as("ci-billing", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	denied("GetDeployment other service", err, "deployments/billing/staging")
	_, err = f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: hop}))
	denied("GetOperation of a human's operation", err, "operations it started")
	stream, err := f.opsClient.WatchOperation(ctx, as("ci", &forgev1.WatchOperationRequest{Name: hop}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	denied("WatchOperation of a human's operation", err, "operations it started")
	_, err = f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: "operations/op_000000000000"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown operation: %v", err)
	}

	// The other DeployService RPCs are closed to CI.
	_, err = f.client.ListDeployments(ctx, as("ci", &forgev1.ListDeploymentsRequest{}))
	denied("ListDeployments", err, "CI token")
	_, err = f.client.ListDeployHistory(ctx, as("ci", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging"}))
	denied("ListDeployHistory", err, "CI token")
	_, err = f.client.RollbackDeploy(ctx, as("ci", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	denied("RollbackDeploy", err, "CI token")
	_, err = f.client.PromoteDeploy(ctx, as("ci", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	denied("PromoteDeploy", err, "CI token")
}

func TestRequestIDPerCaller(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	human, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production", RequestId: "same"}))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", RequestId: "same"}))
	if err != nil {
		t.Fatal(err)
	}
	if ci.Msg.GetOperation().GetName() == human.Msg.GetOperation().GetName() {
		t.Fatal("CI got the human's operation back for the same request_id")
	}
	if ci.Msg.GetOperation().GetTarget() != "deployments/payments/staging" {
		t.Errorf("CI operation target %q", ci.Msg.GetOperation().GetTarget())
	}
	again, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production", RequestId: "same"}))
	if err != nil || again.Msg.GetOperation().GetName() != human.Msg.GetOperation().GetName() {
		t.Errorf("the human's retry should return its own operation: %v", err)
	}
	waitCIOp(t, f.ops, human.Msg.GetOperation().GetName())
	waitCIOp(t, f.ops, ci.Msg.GetOperation().GetName())
}
```

- [ ] **Step 4: Run them to see them fail**

Run: `go test ./internal/server -run 'TestCIStartDeploy|TestCIScope|TestRequestIDPerCaller'`
Expected: FAIL. `TestCIStartDeploy` gets `PermissionDenied` ("lacks permission services.write"), `TestRequestIDPerCaller` gets the human's operation back.

- [ ] **Step 5: Write the scope checks**

Create `internal/server/ci_scope.go`:

```go
package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// What a CI workload may do inside the RPCs the interceptor lets it call.

func workloadDenied(w *Workload, format string, a ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("a CI token from %s may "+format, append([]any{w.Repository}, a...)...))
}

// startRef checks that the caller may deploy service to env and returns the
// ref to deploy: a human's ref, or a CI caller's token commit.
func startRef(ctx context.Context, service, env, ref string) (string, error) {
	w := workloadOf(ctx)
	if w == nil {
		return ref, require(ctx, PermServicesWrite)
	}
	if service != servicePrefix+w.Service || require(ctx, PermDeploy(w.Service, env)) != nil {
		return "", workloadDenied(w, "only deploy %s%s to staging, not %s to %s", servicePrefix, w.Service, service, env)
	}
	return w.SHA, nil
}

// ownDeployment checks a CI caller's read of svc/env: its own staging only.
func ownDeployment(ctx context.Context, svc, env string) error {
	w := workloadOf(ctx)
	if w == nil || (svc == w.Service && env == "staging") {
		return nil
	}
	return workloadDenied(w, "only read %s%s/staging", deployNamePrefix, w.Service)
}

// ownOperation checks a CI caller's read of op: only operations it started.
func ownOperation(ctx context.Context, op *forgev1.Operation) error {
	w := workloadOf(ctx)
	if w == nil {
		return nil
	}
	if p, _ := PrincipalFrom(ctx); op.GetCreator() == p.Subject {
		return nil
	}
	return workloadDenied(w, "only read operations it started")
}

// requestKey scopes a request_id to its caller, so one caller's id never
// returns another caller's operation.
func requestKey(ctx context.Context, id string) string {
	subject := ""
	if p, ok := PrincipalFrom(ctx); ok {
		subject = p.Subject
	}
	return subject + "\x00" + id
}
```

`startRef` compares `service` with the exact resource name before checking the permission, so a malformed name like `payments` is denied rather than resolved.

- [ ] **Step 6: Use them in the handlers**

In `internal/server/deploy_work.go`, `StartDeploy`: replace

```go
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, m.GetService(), m.GetRef())
```

with

```go
	ref, err := startRef(ctx, m.GetService(), m.GetEnv(), m.GetRef())
	if err != nil {
		return nil, err
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, m.GetService(), ref)
```

and both `d.requests[key]` uses with `d.requests[requestKey(ctx, key)]`:

```go
	if key := m.GetRequestId(); key != "" {
		if name, ok := d.requests[requestKey(ctx, key)]; ok {
```

```go
	if key := m.GetRequestId(); key != "" {
		d.requests[requestKey(ctx, key)] = op.GetName()
	}
```

Update the `requests` field comment in `deploys.go` to `// requests maps a caller's StartDeploy request_id (requestKey) to its operation.`

In `internal/server/deploys.go`, `GetDeployment`: replace

```go
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
```

with

```go
	if workloadOf(ctx) == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := ownDeployment(ctx, svc, env); err != nil {
		return nil, err
	}
```

In `internal/server/operation_service.go`, `GetOperation` becomes:

```go
func (s *operationService) GetOperation(ctx context.Context, req *connect.Request[forgev1.GetOperationRequest]) (*connect.Response[forgev1.GetOperationResponse], error) {
	if workloadOf(ctx) == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return nil, err
	}
	op, ok := s.ops.Get(name)
	if !ok {
		return nil, opNotFound(name)
	}
	if err := ownOperation(ctx, op); err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetOperationResponse{Operation: op}), nil
}
```

and `WatchOperation` becomes:

```go
func (s *operationService) WatchOperation(ctx context.Context, req *connect.Request[forgev1.WatchOperationRequest], stream *connect.ServerStream[forgev1.WatchOperationResponse]) error {
	if workloadOf(ctx) == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return err
		}
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return err
	}
	if workloadOf(ctx) != nil {
		op, ok := s.ops.Get(name)
		if !ok {
			return opNotFound(name)
		}
		if err := ownOperation(ctx, op); err != nil {
			return err
		}
	}
	err := s.ops.Watch(ctx, name, stream.Send)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // the client went away; nothing to report to it
	}
	return err
}
```

`ListOperations` and `CancelOperation` stay as they are: the interceptor already refuses them to workloads, and they still require `services.read` / `services.write`, which workloads never have.

- [ ] **Step 7: Run the tests**

Run: `go test -race ./internal/server -run 'TestCI|TestRequestIDPerCaller|TestWorkloadInterceptor|TestStartDeploy|TestOperationService' -v`
Expected: PASS. In `TestWorkloadInterceptor`, `WatchOperation` as CI now ends `NotFound` (the op does not exist), which the test accepts.

- [ ] **Step 8: Run everything, lint, commit**

Run: `make test lint`
Expected: PASS.

```bash
git add internal/server/ci_scope.go internal/server/ci_scope_test.go internal/server/deploy_work.go internal/server/deploys.go internal/server/deploys_test.go internal/server/operation_service.go internal/envtest/envtest.go
git commit -m "server: CI workloads deploy their own service to staging at the token's commit

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: forge-api accepts GitHub Actions tokens when deploys are on

**Files:**
- Modify: `cmd/forge-api/deploys.go` (add `withCITokens`)
- Modify: `cmd/forge-api/main.go` (wrap `authn` after `deploys(...)`)
- Test: `cmd/forge-api/deploys_test.go`

**Interfaces:**
- Consumes: `server.ByIssuer`, `server.NewGitHubOIDCAuthenticator`, `server.GitHubOIDCOptions`, `server.GitHubIssuer`, `server.DefaultBranches` (Tasks 1-2); `github.Client.DefaultBranch` (Task 1).
- Produces: `func withCITokens(ctx context.Context, base server.Authenticator, audience string, branches server.DefaultBranches, catalog *server.Catalog, org string) server.Authenticator`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/forge-api/deploys_test.go` (add `"context"` and the `server` import if missing):

```go
func TestWithCITokens(t *testing.T) {
	t.Parallel()
	base := server.StaticAuthenticator{"dev": {Subject: "dev"}}
	got := withCITokens(t.Context(), base, "forge", nil, server.NewCatalog(nil), "showmino")
	b, ok := got.(server.ByIssuer)
	if !ok {
		t.Fatalf("got %T, want server.ByIssuer", got)
	}
	if _, ok := b.Issuers[server.GitHubIssuer].(*server.GitHubOIDCAuthenticator); !ok || len(b.Issuers) != 1 {
		t.Errorf("issuers: %v", b.Issuers)
	}
	if p, err := got.Authenticate(context.Background(), "dev"); err != nil || p.Subject != "dev" {
		t.Errorf("the dev token must still work: %+v %v", p, err)
	}
}
```

This test never sends a GitHub-issued token, so it makes no network call; Task 1 covers verification.

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./cmd/forge-api -run TestWithCITokens`
Expected: FAIL, `undefined: withCITokens`.

- [ ] **Step 3: Implement `withCITokens`**

In `cmd/forge-api/deploys.go` (add `"context"` to the imports):

```go
// withCITokens makes forge-api accept GitHub Actions OIDC tokens next to
// base's: tokens issued by GitHub go to the GitHub verifier, all others to base.
func withCITokens(ctx context.Context, base server.Authenticator, audience string, branches server.DefaultBranches,
	catalog *server.Catalog, org string) server.Authenticator {
	gh := server.NewGitHubOIDCAuthenticator(ctx, server.GitHubOIDCOptions{
		Audience: audience, Org: org, Catalog: catalog, Branches: branches,
	})
	return server.ByIssuer{Default: base, Issuers: map[string]server.Authenticator{server.GitHubIssuer: gh}}
}
```

- [ ] **Step 4: Wire it into `main.go`**

In `cmd/forge-api/main.go`, replace

```go
	if deps == nil {
		log.Info("deploys are off; set --deploys")
	}
```

with

```go
	if deps == nil {
		log.Info("deploys are off; set --deploys")
	} else {
		audience := env("FORGE_GITHUB_OIDC_AUDIENCE", "forge")
		authn = withCITokens(ctx, authn, audience, &github.Client{Token: app.Token}, catalog, *githubOrg)
		log.Info("accepting GitHub Actions OIDC tokens for staging deploys", "issuer", server.GitHubIssuer, "audience", audience)
	}
```

`deploys(...)` returns a non-nil `*server.Deploys` only when `app` is set, so `app.Token` is safe here. Add the `github` import to `main.go` if it isn't there (`"github.com/showmino/forge/internal/github"`).

- [ ] **Step 5: Run the tests**

Run: `go test -race ./cmd/forge-api -v -run 'TestWithCITokens|TestDeploys'`
Expected: PASS.

- [ ] **Step 6: Run everything, lint, commit**

Run: `make test lint`
Expected: PASS.

```bash
git add cmd/forge-api/deploys.go cmd/forge-api/deploys_test.go cmd/forge-api/main.go
git commit -m "forge-api: accept GitHub Actions OIDC tokens when deploys are on

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: The CLI picks up the Actions token; docs

**Files:**
- Create: `internal/auth/actions.go`
- Test: `internal/auth/actions_test.go`
- Modify: `internal/cli/factory.go` (`TokenSource`, `loginHint`, new `actionsHint`)
- Modify: `internal/cli/deploy.go` (`GITHUB_SHA` default ref, flag help)
- Test: `internal/cli/ci_test.go`
- Modify: `README.md`, `.claude/skills/forge/SKILL.md`

**Interfaces:**
- Consumes: `server.Workload`, `server.PermDeploy` (Task 1), the CI-scoped deploy RPCs (Task 3); `harness`, `newHarness`, `startAPI`, `serveDeploys`, `writerToken`, `cliSHA` from `internal/cli` tests.
- Produces:
  - `const auth.ActionsAudience = "forge"`
  - `func auth.ActionsToken(hc *http.Client, requestURL, requestToken, audience string) oauth2.TokenSource`: reuses a token until a minute before its `exp`.

- [ ] **Step 1: Write the failing token-source tests**

Create `internal/auth/actions_test.go`:

```go
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT is a JWT-shaped string whose payload says exp.
func fakeJWT(exp time.Time) string {
	b, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// actionsEndpoint serves tokens from next and counts requests; it insists on
// the request token and the forge audience, and keeps the URL's own query.
func actionsEndpoint(t *testing.T, next func() string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer req-token" {
			http.Error(w, "bad request token", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("audience") != "forge" || r.URL.Query().Get("api-version") != "2.0" {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"count":1,"value":%q}`, next())
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestActionsTokenReused(t *testing.T) {
	t.Parallel()
	jwt := fakeJWT(time.Now().Add(5 * time.Minute))
	srv, calls := actionsEndpoint(t, func() string { return jwt })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 3 {
		tok, err := ts.Token()
		if err != nil {
			t.Fatal(err)
		}
		if tok.AccessToken != jwt {
			t.Fatalf("token %q", tok.AccessToken)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests for a token valid five more minutes, want 1", calls.Load())
	}
}

func TestActionsTokenRefetchesNearExpiry(t *testing.T) {
	t.Parallel()
	srv, calls := actionsEndpoint(t, func() string { return fakeJWT(time.Now().Add(30 * time.Second)) })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 2 {
		if _, err := ts.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests for tokens within a minute of expiry, want 2", calls.Load())
	}
}

func TestActionsTokenUnparseableExpiry(t *testing.T) {
	t.Parallel()
	srv, calls := actionsEndpoint(t, func() string { return "not-a-jwt" })
	ts := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience)
	for range 2 {
		if _, err := ts.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("a token without a readable exp must not be cached: %d requests", calls.Load())
	}
}

func TestActionsTokenErrors(t *testing.T) {
	t.Parallel()
	srv, _ := actionsEndpoint(t, func() string { return "" })
	// Wrong request token: 403, and the error must not echo either token.
	_, err := ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "wrong", ActionsAudience).Token()
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("403: %v", err)
	}
	// An empty value.
	_, err = ActionsToken(srv.Client(), srv.URL+"/token?api-version=2.0", "req-token", ActionsAudience).Token()
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("empty value: %v", err)
	}
	// A request URL that isn't one.
	if _, err := ActionsToken(srv.Client(), "://bad", "req-token", ActionsAudience).Token(); err == nil {
		t.Error("bad URL: want an error")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/auth -run TestActionsToken`
Expected: FAIL to compile, `undefined: ActionsToken`.

- [ ] **Step 3: Implement `ActionsToken`**

Create `internal/auth/actions.go`:

```go
package auth

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

	"golang.org/x/oauth2"
)

// ActionsAudience is the audience forge-api expects on GitHub Actions tokens.
const ActionsAudience = "forge"

// ActionsToken returns GitHub Actions OIDC tokens for audience, requested
// from requestURL (ACTIONS_ID_TOKEN_REQUEST_URL) with requestToken
// (ACTIONS_ID_TOKEN_REQUEST_TOKEN). A token is reused until a minute before
// its exp, so a long deploy gets a fresh one for its later calls.
func ActionsToken(hc *http.Client, requestURL, requestToken, audience string) oauth2.TokenSource {
	if hc == nil {
		hc = http.DefaultClient
	}
	return oauth2.ReuseTokenSourceWithExpiry(nil, &actionsSource{hc: hc, url: requestURL, token: requestToken, audience: audience}, time.Minute)
}

type actionsSource struct {
	hc                   *http.Client
	url, token, audience string
}

func (s *actionsSource) Token() (*oauth2.Token, error) {
	u, err := url.Parse(s.url)
	if err != nil || u.Scheme == "" {
		return nil, errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is not a URL")
	}
	q := u.Query()
	q.Set("audience", s.audience)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %s", resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Value == "" {
		return nil, errors.New("request a GitHub Actions OIDC token: the response has no token")
	}
	return &oauth2.Token{AccessToken: out.Value, TokenType: "Bearer", Expiry: jwtExpiry(out.Value)}, nil
}

// jwtExpiry reads a JWT's exp without verifying it. When it can't, it
// returns now, so the token is used once and never cached.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Now()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Now()
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Exp == 0 {
		return time.Now()
	}
	return time.Unix(c.Exp, 0)
}
```

Run: `go test -race ./internal/auth -run TestActionsToken -v`
Expected: PASS.

- [ ] **Step 4: Write the failing CLI tests**

Create `internal/cli/ci_test.go`:

```go
package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showmino/forge/internal/server"
)

// actionsJWT is what the fake Actions endpoint hands out; the API's static
// authenticator maps it to a CI workload of showmino/payments.
func actionsJWT() string {
	b, _ := json.Marshal(map[string]any{"exp": time.Now().Add(5 * time.Minute).Unix()})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// inActions serves the Actions token endpoint and sets the runner's variables
// (and drops FORGE_TOKEN), returning the request counter.
func (h *harness) inActions(token string, status int) *atomic.Int32 {
	h.t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer req-token" || r.URL.Query().Get("audience") != "forge" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		fmt.Fprintf(w, `{"value":%q}`, token)
	}))
	h.t.Cleanup(srv.Close)
	delete(h.env, "FORGE_TOKEN")
	h.env["GITHUB_ACTIONS"] = "true"
	h.env["ACTIONS_ID_TOKEN_REQUEST_URL"] = srv.URL + "/token?api-version=2.0"
	h.env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "req-token"
	h.env["GITHUB_SHA"] = cliSHA
	return &calls
}

func TestDeployFromActions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	jwt := actionsJWT()
	h.startAPI(server.StaticAuthenticator{
		writerToken: {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
		jwt: {
			Subject: "github:showmino/payments", Permissions: []string{server.PermDeploy("payments", "staging")},
			Workload: &server.Workload{Repository: "showmino/payments", Service: "payments", SHA: cliSHA},
		},
	})
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	// A human token in CI deploys GITHUB_SHA when --ref is not given.
	h.env["GITHUB_SHA"] = cliSHA
	if r := h.ok("deploy", "start", "payments", "--env", "staging", "--dry-run"); !strings.Contains(r.out, "("+cliSHA+" at 1a2b3c4)") {
		t.Errorf("dry run with GITHUB_SHA: %q", r.out)
	}

	calls := h.inActions(jwt, http.StatusOK)
	r := h.ok("deploy", "start", "payments", "--env", "staging")
	if !strings.Contains(r.out, "health          healthy") {
		t.Errorf("printed deployment:\n%s", r.out)
	}
	if calls.Load() != 1 {
		t.Errorf("%d token requests for one deploy, want 1 (the token is reused)", calls.Load())
	}
	if strings.Contains(r.out+r.err, jwt) {
		t.Error("the token was printed")
	}
	h.fails(5, "deploy", "start", "payments", "--env", "production")
	h.fails(5, "deploy", "status", "payments")
	h.fails(5, "service", "list")
}

func TestActionsTokenHints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// The endpoint refuses: exit 4, the hint names the permission, no token echoed.
	h.inActions("unused", http.StatusForbidden)
	r := h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "id-token: write") || strings.Contains(r.err, "req-token") {
		t.Errorf("refused token request:\n%s", r.err)
	}

	// No id-token permission: the runner sets no Actions variables.
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_URL")
	delete(h.env, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	r = h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "id-token: write") {
		t.Errorf("no Actions token in GitHub Actions:\n%s", r.err)
	}

	// Outside GitHub Actions the hint stays "forge auth login".
	delete(h.env, "GITHUB_ACTIONS")
	r = h.fails(4, "deploy", "status", "payments", "--env", "staging")
	if !strings.Contains(r.err, "forge auth login") || strings.Contains(r.err, "id-token") {
		t.Errorf("outside Actions:\n%s", r.err)
	}
}
```

If the harness's `serveDeploys` needs deploys to answer `deploy status` before the login fails, it doesn't: the token source fails before any request is sent. `TestActionsTokenHints` therefore needs no `serveDeploys`.

- [ ] **Step 5: Run them to see them fail**

Run: `go test ./internal/cli -run 'TestDeployFromActions|TestActionsTokenHints'`
Expected: FAIL. `TestDeployFromActions`: the dry run shows `(main at 1a2b3c4)`, and the Actions deploy exits 4 (no stored credentials). `TestActionsTokenHints`: hints say "run 'forge auth login'".

- [ ] **Step 6: Pick the token up in the Factory**

In `internal/cli/factory.go`, replace `TokenSource`, `loginHint` and its `Token` with:

```go
// TokenSource returns FORGE_TOKEN if set; in GitHub Actions with an OIDC
// token available, that token (audience "forge"); otherwise stored credentials.
func (f *Factory) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	r, err := f.Resolved()
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		return auth.StaticToken(r.Token), nil
	}
	reqURL, _ := f.Env("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqToken, _ := f.Env("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL != "" && reqToken != "" {
		return &actionsHint{auth.ActionsToken(f.HTTPClient, reqURL, reqToken, auth.ActionsAudience)}, nil
	}
	hint := "run 'forge auth login'"
	if v, _ := f.Env("GITHUB_ACTIONS"); v == "true" {
		hint = "in GitHub Actions, give the job 'permissions: id-token: write' so forge can use its OIDC token, or set FORGE_TOKEN"
	}
	return &loginHint{ts: auth.TokenSource(ctx, f.CredentialStore(), r.CredentialKey(), f.HTTPClient, f.Now), hint: hint}, nil
}

// loginHint turns ErrLoginRequired into exit code 4 with a hint.
type loginHint struct {
	ts   oauth2.TokenSource
	hint string
}

func (l *loginHint) Token() (*oauth2.Token, error) {
	t, err := l.ts.Token()
	if errors.Is(err, auth.ErrLoginRequired) {
		return nil, exitcode.New(exitcode.Unauthenticated, "%v", err).WithHint(l.hint)
	}
	return t, err
}

// actionsHint turns a failed GitHub Actions token request into exit code 4.
type actionsHint struct{ ts oauth2.TokenSource }

func (a *actionsHint) Token() (*oauth2.Token, error) {
	t, err := a.ts.Token()
	if err != nil {
		return nil, exitcode.New(exitcode.Unauthenticated, "%v", err).WithHint("the job needs 'permissions: id-token: write'")
	}
	return t, nil
}
```

This replaces the package's only `loginHint{` literal.

The token source is built once per command and shared by every call the command makes, so `ReuseTokenSourceWithExpiry` inside `ActionsToken` serves the start, the watch and the final read from one token while it is fresh.

- [ ] **Step 7: Default `deploy start`'s ref to `GITHUB_SHA`**

In `internal/cli/deploy.go`, `newDeployStartCmd`'s `RunE`, after `svc, err := f.deployService(args)` and its error check:

```go
			if ref == "" {
				if sha, _ := f.Env("GITHUB_SHA"); sha != "" {
					ref = sha // in GitHub Actions: the commit the job runs at
				}
			}
```

and change the flag help:

```go
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit (default $GITHUB_SHA in GitHub Actions, else main)")
```

- [ ] **Step 8: Run the tests**

Run: `go test -race ./internal/cli ./internal/auth -v -run 'TestDeployFromActions|TestActionsToken|TestDeploy|TestAuth'`
Expected: PASS.

If a CLI golden of `forge deploy start --help` exists (`grep -rln "branch, tag or commit" internal/cli/testdata`), run `make update-golden` and check the diff is only the flag help.

- [ ] **Step 9: Document it**

In `README.md`, after the paragraph that starts "`forge deploy start <service> --env staging|production` commits the service's version", add:

```markdown
From GitHub Actions, `forge deploy start --env staging` needs no secret: with
`permissions: id-token: write`, the CLI requests the job's OIDC token
(audience `forge`) and forge-api accepts it when `--deploys` is on. The token
only lets the job deploy its own repo's service to staging, at the commit it
runs at (`GITHUB_SHA`), from the default branch, and follow that deploy.
Production stays with humans (`forge deploy promote`). The runner must reach
the API (`FORGE_API_URL`).
```

In `.claude/skills/forge/SKILL.md`, replace the Phase 4 status line with:

```markdown
- **Phase 4 (in progress):** spec docs/specs/2026-10-09-phase-4-deployments-design.md. Milestone 1 (done, PR #14): deploy engine (DeployService, internal/server/deploy*.go, github.RepoFiles, deploy/argocd/chart with make chart-test, forge deploy) per docs/plans/2026-10-09-phase-4-m1-deploy-engine.md. Milestone 2: CI workload identity (internal/server/ci_auth.go verifies GitHub Actions tokens, ByIssuer routes by iss, ci_scope.go confines workloads; internal/auth/actions.go and the CLI's Actions token pick-up) per docs/plans/2026-10-09-phase-4-m2-ci-identity.md. Then milestone 3 (k3s and the exit gate).
```

and add to "Common mistakes":

```markdown
- Giving a CI workload a new RPC by adding a permission: workloads pass the interceptor only for the procedures in `workloadProcedures` (`authn.go`), and each of those handlers must call its `ci_scope.go` check.
```

- [ ] **Step 10: Run everything, lint, commit**

Run: `make test lint`
Expected: PASS.

```bash
git add internal/auth/actions.go internal/auth/actions_test.go internal/cli/factory.go internal/cli/deploy.go internal/cli/ci_test.go README.md .claude/skills/forge/SKILL.md
git commit -m "cli: use the GitHub Actions OIDC token and GITHUB_SHA in CI

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## Not in this milestone

- The `deploy` job in go-grpc v1.2.0, the k3s rollout and the exit gate: milestone 3.
- `forge auth status` for a CI token: `GetStatus` is closed to workloads, as the spec's "every other RPC" says.
- Cancelling a CI deploy from CI: `CancelOperation` is closed to workloads; a human cancels with `forge op cancel`.
