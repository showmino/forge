# Groundwork: GitHub login and durable storage — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On k3s, only `showmino` org members log in (through GitHub) and only `showmino:platform-engineering` may write. The service catalog and Dex's data live in PostgreSQL, so restarts lose nothing.

**Architecture:**
- A new package `internal/store` holds a `Store` interface with two implementations: `Memory` (today's maps) and `Postgres` (pgx v5, hand-written SQL, embedded migrations).
- `server.Catalog` keeps every RPC handler and rule, and stores through the interface; `forge-api --database-url` picks Postgres.
- The k3s render adds a CloudNativePG cluster `forge-db`. Dex moves to Postgres storage and a GitHub connector, and forge-api gets `FORGE_WRITER_GROUPS` and `FORGE_DATABASE_URL`.

**Tech Stack:** Go 1.27, ConnectRPC, `github.com/jackc/pgx/v5` (new), protojson, CloudNativePG 1.30, Dex v2.41.1, POSIX sh (render.sh).

**Spec:** `docs/specs/2026-10-09-groundwork-auth-storage-design.md`

## Global Constraints

- **New dependency:** only `github.com/jackc/pgx/v5`, the latest v5 release, via `go get github.com/jackc/pgx/v5@latest`. `go mod tidy -diff` must stay clean.
- **Secrets:**
  - Never print, log or commit a secret value: the database URL or password, the OAuth client secret, or the GitHub App key `mino-forge.2026-10-08.private-key.pem`.
  - Logs and errors may name the database host, never the URL.
  - `--database-url` must not take its flag default from the environment; `--help` prints defaults. Read `FORGE_DATABASE_URL` after `flag.Parse`.
- **Tests:**
  - `go test ./...` passes with no database: Postgres tests skip unless `FORGE_TEST_DATABASE_URL` is set.
  - Tasks that touch Postgres run their tests both with and without it.
- **Local test database:** `make db` starts `mirror.gcr.io/library/postgres:18` as container `forge-test-db` on port 55432. Its URL is `postgres://postgres:forge@localhost:55432/postgres?sslmode=disable`.
- **Exact names:**
  - **Kubernetes objects:** CNPG Cluster `forge-db`; Secret `forge-db-app` (keys `uri`, `password`); CNPG Database `forge-db-dex` (database `dex`, owner `forge`); Secret `dex-github` (keys `client_id`, `client_secret`); OnePasswordItem `dex-github`.
  - **render.sh variables:**
    - `FORGE_DEX_GITHUB_ORG`, default: `FORGE_GITHUB_ORG`, else `showmino`;
    - `FORGE_WRITER_GROUPS`, default `<dex-org>:platform-engineering`; an explicitly empty value is refused;
    - `FORGE_DEX_ONEPASSWORD_ITEM`.
  - **forge-api:** the flag `--database-url`, with env `FORGE_DATABASE_URL`.
- **Images:**
  - Dex `ghcr.io/dexidp/dex:v2.41.1` (unchanged);
  - Postgres on k3s: `ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie`;
  - tests and CI: `mirror.gcr.io/library/postgres:18`.
- **The cluster is production:**
  - Implementers may only run read-only `kubectl` commands and `kubectl apply --dry-run=client`.
  - Implementers never apply, never push, never merge.
  - Task 9 is for the controller only, with the owner's OK.
- **Checks:** `golangci-lint` v2.14.0 must be clean (`make lint`). Run `go test -race ./...` and `sh deploy/k3s/render_test.sh`; render_test needs `kubectl` with cluster access for its client dry-run checks.
- **Local development:** `deploy/dex/config.yaml` keeps its demo user and memory storage; `make api-dev` keeps the `dev` token.

## Review Focus

1. **Storage down.** The catalog's storage fails: catalog RPCs, `forge create`, previews, deploys and CI-token login must return `Unavailable`. A missing service must keep returning `NotFound`, and an unknown repo `PermissionDenied`. Pinned by `TestCatalogStoreDown` and `TestGitHubOIDCStoreDown` (Task 4).
2. **Validation errors inside a write.** A connect error returned inside `Store.Write`, such as AlreadyExists, Aborted on an etag mismatch, or FailedPrecondition for a dependency, keeps its code; it is never wrapped into `Unavailable`. Pinned by the existing catalog tests (memory) and by `TestCatalogOnPostgres` (Task 4).
3. **A request ID retried across a restart.** It returns the stored service, not AlreadyExists. Pinned in `TestCatalogSurvivesRestart` (Task 5).
4. **An explicitly empty `FORGE_WRITER_GROUPS=`.** It is refused, never rendered as "everyone writes". Pinned in render_test (Task 6).
5. **Labels, dependencies and timestamps.** They round-trip through jsonb unchanged, and updating `repo_url` moves the repo lookup. Pinned in the shared store suite (Tasks 2 and 3).

---

## File structure

| File | Responsibility |
| --- | --- |
| `internal/store/store.go` | `Store`, `Tx`, `ListQuery`, `ErrNotFound`, `RepoKey` |
| `internal/store/memory.go` | `Memory` store |
| `internal/store/postgres.go` | `Postgres` store, `OpenPostgres`, `Migrate` |
| `internal/store/migrations/0001_services.sql` | the first schema |
| `internal/store/suite_test.go` | the shared suite, `runSuite` |
| `internal/store/memory_test.go`, `postgres_test.go` | run the suite on each store; Postgres-only migration tests |
| `internal/store/pgtest/pgtest.go` | test helper: a fresh schema in `FORGE_TEST_DATABASE_URL`, or skip |
| `internal/server/catalog.go` | Catalog on `store.Store` |
| `internal/server/catalog_store_test.go` | failing-store tests, catalog suite on Postgres |
| `cmd/forge-api/store.go`, `store_test.go` | `openStore`, the restart test |
| `deploy/k3s/forge.yaml.tmpl` | Dex connector and storage, forge-api env |
| `deploy/k3s/database.yaml.tmpl` | CNPG Cluster and Database |
| `deploy/k3s/dex-onepassword.yaml.tmpl` | OnePasswordItem `dex-github` |
| `deploy/k3s/render.sh`, `render_test.sh`, `testdata/*.golden` | rendering and its tests |
| `.github/workflows/ci.yml`, `Makefile` | Postgres service container, `make db` |
| `README.md`, `docs/index.md`, `.claude/skills/forge/SKILL.md` | docs |

---

### Task 1: Writer groups from GitHub teams (tests)

The `Authorizer` already grants `services.write` only to `WriterGroups`. This task pins that with group names as Dex's GitHub connector sends them, and covers the two write RPCs that no test yet checks with a non-writer.

**Files:**
- Modify: `internal/server/server_test.go` (after `TestPermissions`)
- Modify: `internal/server/environments_more_test.go`, `internal/server/deploys_more_test.go`

**Interfaces:** none new.

- [ ] **Step 1: Write the tests**

In `server_test.go`, after `TestPermissions`:

```go
// Dex's GitHub connector sends groups as "<org>:<team-slug>"; only the writer
// team may write, and every member may read.
func TestWriterGroupsGitHubTeams(t *testing.T) {
	t.Parallel()
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"platform": {Subject: "ana", Groups: []string{"showmino:platform-engineering", "showmino:mino"}},
			"member":   {Subject: "bo", Groups: []string{"showmino:test-team"}},
			"nogroups": {Subject: "cy"},
		},
		Authorizer: Authorizer{WriterGroups: []string{"showmino:platform-engineering"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	ctx := context.Background()
	reg := func(token, id string) error {
		_, err := client.RegisterService(ctx, as(token, &forgev1.RegisterServiceRequest{
			ServiceId: id, Service: &forgev1.Service{OwnerTeam: "billing"},
		}))
		return err
	}
	for _, token := range []string{"member", "nogroups"} {
		if err := reg(token, "x-"+token); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "services.write") {
			t.Errorf("%s register: %v", token, err)
		}
		if _, err := client.ListServices(ctx, as(token, &forgev1.ListServicesRequest{})); err != nil {
			t.Errorf("%s list: %v", token, err)
		}
	}
	if err := reg("platform", "payments"); err != nil {
		t.Errorf("platform-engineering register: %v", err)
	}
}
```

Add imports where they're missing: `net/http/httptest`, `strings`, `github.com/showmino/forge/gen/forge/v1/forgev1connect`.

In `environments_more_test.go`, add:

```go
func TestCreateEnvironmentNeedsWrite(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	_, err := f.client.CreateEnvironment(context.Background(), as("reader", &forgev1.CreateEnvironmentRequest{
		Service: "services/payments", EnvironmentId: "pr-9",
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("reader create: %v", err)
	}
}
```

In `deploys_more_test.go`, add:

```go
func TestStartDeployNeedsWrite(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, true, nil)
	for _, env := range []string{"staging", "production"} {
		_, err := f.client.StartDeploy(context.Background(), as("reader", &forgev1.StartDeployRequest{Service: "services/payments", Env: env}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("reader deploy to %s: %v", env, err)
		}
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test -race -run 'TestWriterGroupsGitHubTeams|TestCreateEnvironmentNeedsWrite|TestStartDeployNeedsWrite' ./internal/server/`

Expected: PASS. The behaviour exists; these tests pin it. If one fails, the code allows a non-writer to write: stop and report.

- [ ] **Step 3: Commit**

```bash
git add internal/server/server_test.go internal/server/environments_more_test.go internal/server/deploys_more_test.go
git commit -m "server: pin writer groups as GitHub org:team names"
```

---

### Task 2: `internal/store`: the interface, the memory store and the shared suite

**Files:**
- Create: `internal/store/store.go`, `internal/store/memory.go`, `internal/store/suite_test.go`, `internal/store/memory_test.go`

**Interfaces:**
- Produces (used by Tasks 3 to 5):

```go
var ErrNotFound = errors.New("not found")
type Store interface {
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	List(ctx context.Context, q ListQuery) ([]*forgev1.Service, error)
	Names(ctx context.Context) ([]string, error)
	ForRepo(ctx context.Context, owner, repo string) ([]string, error)
	Write(ctx context.Context, fn func(tx Tx) error) error
	Close()
}
type ListQuery struct { OwnerTeam, After string; Limit int }
type Tx interface {
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	Put(ctx context.Context, svc *forgev1.Service, revision int64) error
	Delete(ctx context.Context, name string) error
	Dependents(ctx context.Context, name string) ([]string, error)
	Recall(ctx context.Context, requestID string) (*forgev1.Service, error)
	Remember(ctx context.Context, requestID string, svc *forgev1.Service) error
}
func RepoKey(repoURL string) string
func NewMemory() *Memory
func runSuite(t *testing.T, open func(t *testing.T) Store) // test-only, suite_test.go
```

- [ ] **Step 1: Write `store.go`**

```go
// Package store keeps the service catalog: in memory for development and
// tests, or in PostgreSQL.
package store

import (
	"context"
	"errors"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// ErrNotFound reports a service name or request ID the store does not hold.
var ErrNotFound = errors.New("not found")

// Store holds catalog services by resource name ("services/<id>"), each with
// a revision. Services are copied in and out: callers never share them with
// the store.
type Store interface {
	// Get returns the service and its revision, or ErrNotFound.
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	// List returns up to q.Limit services, sorted by name.
	List(ctx context.Context, q ListQuery) ([]*forgev1.Service, error)
	// Names returns every service name, sorted.
	Names(ctx context.Context) ([]string, error)
	// ForRepo returns the sorted names of the services whose repo_url is
	// https://github.com/<owner>/<repo>, compared case-insensitively.
	ForRepo(ctx context.Context, owner, repo string) ([]string, error)
	// Write runs fn in a transaction; write transactions never overlap. What
	// fn writes through tx takes effect only if fn returns nil. fn's error is
	// returned unchanged.
	Write(ctx context.Context, fn func(tx Tx) error) error
	// Close releases the store's resources.
	Close()
}

// ListQuery selects services for List.
type ListQuery struct {
	OwnerTeam string // "" means every team
	After     string // only names greater than this
	Limit     int    // at most this many; must be positive
}

// Tx is a write transaction. Its reads see its own writes.
type Tx interface {
	// Get returns the service and its revision, or ErrNotFound.
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	// Put stores svc under svc.Name with revision, replacing any service there.
	Put(ctx context.Context, svc *forgev1.Service, revision int64) error
	// Delete removes the service called name, if there is one.
	Delete(ctx context.Context, name string) error
	// Dependents returns the sorted names of the other services that list
	// name among their dependencies.
	Dependents(ctx context.Context, name string) ([]string, error)
	// Recall returns the service a RegisterService with requestID returned,
	// or ErrNotFound.
	Recall(ctx context.Context, requestID string) (*forgev1.Service, error)
	// Remember records svc as the result of requestID.
	Remember(ctx context.Context, requestID string, svc *forgev1.Service) error
}

// RepoKey returns "<owner>/<repo>", lowercased, for a repo_url of the form
// https://github.com/<owner>/<repo> (".git" and a trailing "/" allowed), and
// "" for any other URL.
func RepoKey(repoURL string) string {
	rest, ok := strings.CutPrefix(repoURL, "https://github.com/")
	if !ok {
		return ""
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return ""
	}
	return strings.ToLower(owner + "/" + repo)
}
```

- [ ] **Step 2: Write the shared suite `suite_test.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runSuite checks a Store. open returns an empty store for one subtest.
func runSuite(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc := func(id, team, repo string, deps ...string) *forgev1.Service {
		return &forgev1.Service{
			Name: "services/" + id, OwnerTeam: team, RepoUrl: repo, Dependencies: deps,
			CreateTime: timestamppb.New(at), UpdateTime: timestamppb.New(at), Etag: `W/"1"`,
		}
	}
	put := func(t *testing.T, s Store, svcs ...*forgev1.Service) {
		t.Helper()
		if err := s.Write(ctx, func(tx Tx) error {
			for _, v := range svcs {
				if err := tx.Put(ctx, v, 1); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(svcs []*forgev1.Service) []string {
		var out []string
		for _, v := range svcs {
			out = append(out, v.GetName())
		}
		return out
	}

	t.Run("get missing", func(t *testing.T) {
		s := open(t)
		if _, _, err := s.Get(ctx, "services/nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		s := open(t)
		in := svc("payments", "billing", "https://github.com/showmino/payments", "services/ledger")
		in.DisplayName, in.Owners, in.Lifecycle = "Payments", []string{"ana@example.com"}, forgev1.Lifecycle_LIFECYCLE_PRODUCTION
		in.Labels = map[string]string{"tier": "1"}
		put(t, s, in)
		got, rev, err := s.Get(ctx, "services/payments")
		if err != nil || rev != 1 || !proto.Equal(got, in) {
			t.Fatalf("got %v rev %d err %v, want %v", got, rev, err, in)
		}
		got.OwnerTeam = "changed" // the store keeps its own copy
		again, _, _ := s.Get(ctx, "services/payments")
		if again.GetOwnerTeam() != "billing" {
			t.Fatal("the store shares its service with the caller")
		}
	})

	t.Run("list, page and filter", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("a", "x", ""), svc("b", "y", ""), svc("c", "x", ""), svc("d", "x", ""), svc("e", "y", ""))
		got, err := s.List(ctx, ListQuery{Limit: 2})
		if err != nil || !slices.Equal(names(got), []string{"services/a", "services/b"}) {
			t.Fatalf("first page %v %v", names(got), err)
		}
		got, _ = s.List(ctx, ListQuery{After: "services/b", Limit: 10})
		if !slices.Equal(names(got), []string{"services/c", "services/d", "services/e"}) {
			t.Fatalf("after b %v", names(got))
		}
		got, _ = s.List(ctx, ListQuery{OwnerTeam: "x", After: "services/a", Limit: 10})
		if !slices.Equal(names(got), []string{"services/c", "services/d"}) {
			t.Fatalf("team x after a %v", names(got))
		}
		all, err := s.Names(ctx)
		if err != nil || !slices.Equal(all, []string{"services/a", "services/b", "services/c", "services/d", "services/e"}) {
			t.Fatalf("names %v %v", all, err)
		}
	})

	t.Run("repo lookup", func(t *testing.T) {
		s := open(t)
		put(t, s,
			svc("payments", "b", "https://github.com/ShowMino/Payments.git"),
			svc("twin-b", "b", "https://github.com/showmino/twin/"),
			svc("twin-a", "b", "https://github.com/showmino/twin"),
			svc("other", "b", "https://github.com/acme/payments"),
			svc("gitlab", "b", "https://gitlab.com/showmino/payments"),
			svc("none", "b", ""),
		)
		for _, tc := range []struct {
			owner, repo string
			want        []string
		}{
			{"showmino", "payments", []string{"services/payments"}},
			{"SHOWMINO", "PAYMENTS", []string{"services/payments"}},
			{"showmino", "twin", []string{"services/twin-a", "services/twin-b"}},
			{"showmino", "nope", nil},
		} {
			got, err := s.ForRepo(ctx, tc.owner, tc.repo)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ForRepo(%s, %s) = %v %v, want %v", tc.owner, tc.repo, got, err, tc.want)
			}
		}
		moved := svc("payments", "b", "https://github.com/showmino/payments-v2")
		if err := s.Write(ctx, func(tx Tx) error { return tx.Put(ctx, moved, 2) }); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ForRepo(ctx, "showmino", "payments"); len(got) != 0 {
			t.Errorf("old repo still maps to %v", got)
		}
		if got, _ := s.ForRepo(ctx, "showmino", "payments-v2"); !slices.Equal(got, []string{"services/payments"}) {
			t.Errorf("new repo maps to %v", got)
		}
	})

	t.Run("update and delete", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("a", "x", ""))
		next := svc("a", "y", "")
		next.Etag = `W/"2"`
		if err := s.Write(ctx, func(tx Tx) error { return tx.Put(ctx, next, 2) }); err != nil {
			t.Fatal(err)
		}
		got, rev, _ := s.Get(ctx, "services/a")
		if rev != 2 || got.GetOwnerTeam() != "y" || got.GetEtag() != `W/"2"` {
			t.Fatalf("after update: %v rev %d", got, rev)
		}
		if err := s.Write(ctx, func(tx Tx) error { return tx.Delete(ctx, "services/a") }); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})

	t.Run("dependents see the transaction's writes", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("b", "x", ""), svc("a", "x", "", "services/b"), svc("c", "x", "", "services/b", "services/a"))
		if err := s.Write(ctx, func(tx Tx) error {
			got, err := tx.Dependents(ctx, "services/b")
			if err != nil || !slices.Equal(got, []string{"services/a", "services/c"}) {
				return fmt.Errorf("dependents of b: %v %v", got, err)
			}
			if err := tx.Delete(ctx, "services/a"); err != nil {
				return err
			}
			got, err = tx.Dependents(ctx, "services/b")
			if err != nil || !slices.Equal(got, []string{"services/c"}) {
				return fmt.Errorf("after deleting a: %v %v", got, err)
			}
			if _, _, err := tx.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("tx get of deleted a: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failed write changes nothing", func(t *testing.T) {
		s := open(t)
		boom := errors.New("boom")
		err := s.Write(ctx, func(tx Tx) error {
			if err := tx.Put(ctx, svc("a", "x", ""), 1); err != nil {
				return err
			}
			if err := tx.Remember(ctx, "req-1", svc("a", "x", "")); err != nil {
				return err
			}
			return boom
		})
		if err != boom { //nolint:errorlint // Write must return fn's error itself
			t.Fatalf("Write returned %v, want fn's error unchanged", err)
		}
		if _, _, err := s.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back put is visible: %v", err)
		}
		_ = s.Write(ctx, func(tx Tx) error {
			if _, err := tx.Recall(ctx, "req-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("rolled-back remember is visible: %v", err)
			}
			return nil
		})
	})

	t.Run("request IDs", func(t *testing.T) {
		s := open(t)
		want := svc("a", "x", "")
		if err := s.Write(ctx, func(tx Tx) error {
			if _, err := tx.Recall(ctx, "req-1"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("recall before remember: %v", err)
			}
			return tx.Remember(ctx, "req-1", want)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Write(ctx, func(tx Tx) error {
			got, err := tx.Recall(ctx, "req-1")
			if err != nil || !proto.Equal(got, want) {
				return fmt.Errorf("recall: %v %v", got, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("writes do not overlap", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("counter", "x", ""))
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := s.Write(ctx, func(tx Tx) error {
					cur, rev, err := tx.Get(ctx, "services/counter")
					if err != nil {
						return err
					}
					return tx.Put(ctx, cur, rev+1)
				}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if _, rev, _ := s.Get(ctx, "services/counter"); rev != 21 {
			t.Fatalf("revision %d after 20 increments from 1, want 21 (lost updates)", rev)
		}
	})
}
```

`memory_test.go`:

```go
package store

import "testing"

func TestMemory(t *testing.T) {
	t.Parallel()
	runSuite(t, func(*testing.T) Store { return NewMemory() })
}

func TestRepoKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://github.com/Showmino/Payments":      "showmino/payments",
		"https://github.com/showmino/payments.git":  "showmino/payments",
		"https://github.com/showmino/payments/":     "showmino/payments",
		"https://github.com/showmino/payments.git/": "showmino/payments",
		"https://github.com/showmino":               "",
		"https://github.com/showmino/a/b":           "",
		"git@github.com:showmino/payments.git":      "",
		"https://gitlab.com/showmino/payments":      "",
		"":                                          "",
	} {
		if got := RepoKey(in); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/store/`. Expected: a build failure, `undefined: NewMemory`.

- [ ] **Step 4: Write `memory.go`**

```go
package store

import (
	"context"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// Memory is a Store in memory: it is lost when the process ends.
type Memory struct {
	mu       sync.Mutex
	services map[string]memEntry
	requests map[string]*forgev1.Service
}

type memEntry struct {
	svc *forgev1.Service
	rev int64
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{services: map[string]memEntry{}, requests: map[string]*forgev1.Service{}}
}

func (m *Memory) Get(_ context.Context, name string) (*forgev1.Service, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.services[name]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return clone(e.svc), e.rev, nil
}

func (m *Memory) List(_ context.Context, q ListQuery) ([]*forgev1.Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for n, e := range m.services {
		if n > q.After && (q.OwnerTeam == "" || e.svc.GetOwnerTeam() == q.OwnerTeam) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	if len(names) > q.Limit {
		names = names[:q.Limit]
	}
	out := make([]*forgev1.Service, 0, len(names))
	for _, n := range names {
		out = append(out, clone(m.services[n].svc))
	}
	return out, nil
}

func (m *Memory) Names(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.services))
	for n := range m.services {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

func (m *Memory) ForRepo(_ context.Context, owner, repo string) ([]string, error) {
	key := strings.ToLower(owner + "/" + repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, e := range m.services {
		if RepoKey(e.svc.GetRepoUrl()) == key {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Write holds the store's lock for fn and applies tx's writes when fn
// succeeds.
func (m *Memory) Write(_ context.Context, fn func(tx Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{m: m, services: map[string]*memEntry{}, requests: map[string]*forgev1.Service{}}
	if err := fn(tx); err != nil {
		return err
	}
	for n, e := range tx.services {
		if e == nil {
			delete(m.services, n)
		} else {
			m.services[n] = *e
		}
	}
	for id, s := range tx.requests {
		m.requests[id] = s
	}
	return nil
}

func (m *Memory) Close() {}

// memTx stages writes; a nil entry is a delete. The caller holds m.mu.
type memTx struct {
	m        *Memory
	services map[string]*memEntry
	requests map[string]*forgev1.Service
}

func (t *memTx) lookup(name string) (memEntry, bool) {
	if e, staged := t.services[name]; staged {
		if e == nil {
			return memEntry{}, false
		}
		return *e, true
	}
	e, ok := t.m.services[name]
	return e, ok
}

func (t *memTx) Get(_ context.Context, name string) (*forgev1.Service, int64, error) {
	e, ok := t.lookup(name)
	if !ok {
		return nil, 0, ErrNotFound
	}
	return clone(e.svc), e.rev, nil
}

func (t *memTx) Put(_ context.Context, svc *forgev1.Service, revision int64) error {
	t.services[svc.GetName()] = &memEntry{svc: clone(svc), rev: revision}
	return nil
}

func (t *memTx) Delete(_ context.Context, name string) error {
	t.services[name] = nil
	return nil
}

func (t *memTx) Dependents(_ context.Context, name string) ([]string, error) {
	seen := map[string]bool{}
	for n := range t.m.services {
		seen[n] = true
	}
	for n := range t.services {
		seen[n] = true
	}
	var out []string
	for n := range seen {
		if e, ok := t.lookup(n); ok && n != name && slices.Contains(e.svc.GetDependencies(), name) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (t *memTx) Recall(_ context.Context, requestID string) (*forgev1.Service, error) {
	if s, ok := t.requests[requestID]; ok {
		return clone(s), nil
	}
	if s, ok := t.m.requests[requestID]; ok {
		return clone(s), nil
	}
	return nil, ErrNotFound
}

func (t *memTx) Remember(_ context.Context, requestID string, svc *forgev1.Service) error {
	t.requests[requestID] = clone(svc)
	return nil
}

func clone(s *forgev1.Service) *forgev1.Service { return proto.Clone(s).(*forgev1.Service) }
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test -race ./internal/store/ && golangci-lint run ./internal/store/...`. Expected: PASS, and lint clean.

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "store: Store interface, the memory store and a shared suite"
```

---

### Task 3: The Postgres store, migrations, `make db`, the CI database

**Files:**
- Create: `internal/store/postgres.go`, `internal/store/migrations/0001_services.sql`, `internal/store/postgres_test.go`, `internal/store/pgtest/pgtest.go`
- Modify: `go.mod`, `go.sum`, `Makefile`, `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `Store`, `Tx`, `ListQuery`, `ErrNotFound`, `RepoKey`, `runSuite` (Task 2).
- Produces:

```go
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) // connects and pings; does not migrate
func (p *Postgres) Migrate(ctx context.Context) error
// package pgtest:
func URL(t testing.TB) string // a fresh, dropped-on-cleanup schema in FORGE_TEST_DATABASE_URL; t.Skip when unset
```

- [ ] **Step 1: Add pgx and start a local database**

```bash
go get github.com/jackc/pgx/v5@latest
```

Add to the `Makefile`: add `db` to `.PHONY`, and after the `dex` target:

```make
db: ## Start a local PostgreSQL on :55432 for the store tests (FORGE_TEST_DATABASE_URL)
	docker run -d --name forge-test-db -p 55432:5432 -e POSTGRES_PASSWORD=forge mirror.gcr.io/library/postgres:18
```

Run `make db` once. If the container already exists, run `docker start forge-test-db`. Then:

```bash
export FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable'
```

- [ ] **Step 2: Write `pgtest.go`**

```go
// Package pgtest gives tests a PostgreSQL database of their own.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// URL returns FORGE_TEST_DATABASE_URL pointed at a new, empty schema that is
// dropped when the test ends. It skips the test when the variable is unset.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("FORGE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("FORGE_TEST_DATABASE_URL is not set (make db)")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "forge_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Errorf("drop %s: %v", schema, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop %s: %v", schema, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema) // pgx sends unknown URL parameters as session settings
	u.RawQuery = q.Encode()
	return u.String()
}
```

- [ ] **Step 3: Write the failing test `postgres_test.go`**

```go
package store

import (
	"context"
	"sync"
	"testing"

	"github.com/showmino/forge/internal/store/pgtest"
)

func openTestPostgres(t *testing.T, url string) *Postgres {
	t.Helper()
	p, err := OpenPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPostgres(t *testing.T) {
	t.Parallel()
	runSuite(t, func(t *testing.T) Store {
		p := openTestPostgres(t, pgtest.URL(t))
		if err := p.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p
	})
}

func TestPostgresMigrate(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	a, b := openTestPostgres(t, url), openTestPostgres(t, url)
	var wg sync.WaitGroup
	for _, p := range []*Postgres{a, b} {
		wg.Go(func() {
			if err := p.Migrate(context.Background()); err != nil {
				t.Errorf("concurrent migrate: %v", err)
			}
		})
	}
	wg.Wait()
	if err := a.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := a.pool.QueryRow(context.Background(), "SELECT count(*) FROM forge_migrations").Scan(&n); err != nil || n != 1 {
		t.Fatalf("forge_migrations has %d rows (%v), want 1", n, err)
	}
}

func TestOpenPostgresBadURL(t *testing.T) {
	t.Parallel()
	_, err := OpenPostgres(context.Background(), "postgres://forge:s3cret-value@127.0.0.1:1/forge?connect_timeout=1")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("the error shows the password: %v", err)
	}
}
```

Add `"strings"` to the imports. `TestOpenPostgresBadURL` needs no database.

Run: `go test ./internal/store/`. Expected: a build failure, `undefined: OpenPostgres`.

- [ ] **Step 4: Write the migration and `postgres.go`**

`internal/store/migrations/0001_services.sql`:

```sql
-- The service catalog. data is the forge.v1.Service message as protojson;
-- the other columns are what queries filter on.
CREATE TABLE services (
    name        text PRIMARY KEY,
    revision    bigint NOT NULL,
    repo        text,
    owner_team  text NOT NULL,
    data        jsonb NOT NULL,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);
CREATE INDEX services_repo ON services (repo);
CREATE INDEX services_owner_team ON services (owner_team);

-- RegisterService results by request_id, kept for 24 hours for retries.
CREATE TABLE service_requests (
    request_id text PRIMARY KEY,
    service    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

`internal/store/postgres.go`:

```go
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Advisory lock keys: arbitrary, fixed, and Forge's own.
const (
	migrateLock int64 = 0x666f726765_01 // "forge", 1
	writeLock   int64 = 0x666f726765_02 // "forge", 2
)

// Postgres is a Store in PostgreSQL.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to the database at url and checks the connection.
// It does not migrate; call Migrate.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database URL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Migrate applies the embedded migrations not yet applied, in order. A
// session advisory lock keeps two processes from migrating at once.
func (p *Postgres) Migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS forge_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, f := range files {
		version, err := strconv.Atoi(strings.SplitN(path.Base(f), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", f)
		}
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM forge_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO forge_migrations (version) VALUES ($1)", version)
			return err
		}); err != nil {
			return fmt.Errorf("migration %s: %w", path.Base(f), err)
		}
	}
	return nil
}

// querier is what pgxpool.Pool and pgx.Tx have in common.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p *Postgres) Get(ctx context.Context, name string) (*forgev1.Service, int64, error) {
	return getService(ctx, p.pool, name)
}

func (p *Postgres) List(ctx context.Context, q ListQuery) ([]*forgev1.Service, error) {
	rows, err := p.pool.Query(ctx, `SELECT data FROM services
		WHERE name > $1 AND ($2::text = '' OR owner_team = $2)
		ORDER BY name LIMIT $3`, q.After, q.OwnerTeam, q.Limit)
	if err != nil {
		return nil, err
	}
	data, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.Service, 0, len(data))
	for _, d := range data {
		s, err := decode(d)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (p *Postgres) Names(ctx context.Context) ([]string, error) {
	return names(ctx, p.pool, "SELECT name FROM services ORDER BY name")
}

func (p *Postgres) ForRepo(ctx context.Context, owner, repo string) ([]string, error) {
	return names(ctx, p.pool, "SELECT name FROM services WHERE repo = $1 ORDER BY name", strings.ToLower(owner+"/"+repo))
}

// Write runs fn in a transaction that first takes a transaction-scoped
// advisory lock, so write transactions run one at a time.
func (p *Postgres) Write(ctx context.Context, fn func(tx Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", writeLock); err != nil {
		return err
	}
	if err := fn(pgTx{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Close() { p.pool.Close() }

type pgTx struct{ tx pgx.Tx }

func (t pgTx) Get(ctx context.Context, name string) (*forgev1.Service, int64, error) {
	return getService(ctx, t.tx, name)
}

func (t pgTx) Put(ctx context.Context, svc *forgev1.Service, revision int64) error {
	data, err := protojson.Marshal(svc)
	if err != nil {
		return err
	}
	var repo *string
	if k := RepoKey(svc.GetRepoUrl()); k != "" {
		repo = &k
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO services (name, revision, repo, owner_team, data, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET revision = EXCLUDED.revision, repo = EXCLUDED.repo,
			owner_team = EXCLUDED.owner_team, data = EXCLUDED.data, update_time = EXCLUDED.update_time`,
		svc.GetName(), revision, repo, svc.GetOwnerTeam(), data, svc.GetCreateTime().AsTime(), svc.GetUpdateTime().AsTime())
	return err
}

func (t pgTx) Delete(ctx context.Context, name string) error {
	_, err := t.tx.Exec(ctx, "DELETE FROM services WHERE name = $1", name)
	return err
}

func (t pgTx) Dependents(ctx context.Context, name string) ([]string, error) {
	return names(ctx, t.tx, `SELECT name FROM services
		WHERE name <> $1 AND data->'dependencies' ? $1 ORDER BY name`, name)
}

func (t pgTx) Recall(ctx context.Context, requestID string) (*forgev1.Service, error) {
	var data []byte
	err := t.tx.QueryRow(ctx, `SELECT service FROM service_requests
		WHERE request_id = $1 AND created_at > now() - interval '24 hours'`, requestID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(data)
}

func (t pgTx) Remember(ctx context.Context, requestID string, svc *forgev1.Service) error {
	data, err := protojson.Marshal(svc)
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, "DELETE FROM service_requests WHERE created_at < now() - interval '24 hours'"); err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO service_requests (request_id, service) VALUES ($1, $2)
		ON CONFLICT (request_id) DO UPDATE SET service = EXCLUDED.service, created_at = now()`, requestID, data)
	return err
}

func getService(ctx context.Context, q querier, name string) (*forgev1.Service, int64, error) {
	var (
		data []byte
		rev  int64
	)
	err := q.QueryRow(ctx, "SELECT data, revision FROM services WHERE name = $1", name).Scan(&data, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	s, err := decode(data)
	return s, rev, err
}

func names(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func decode(data []byte) (*forgev1.Service, error) {
	s := &forgev1.Service{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("decode a stored service: %w", err)
	}
	return s, nil
}
```

`names` returns `nil` for no rows when `pgx.CollectRows` does. If it returns an empty non-nil slice, the suite's `slices.Equal(got, nil)` still holds, because `slices.Equal` treats nil and empty as equal.

- [ ] **Step 5: Run the tests with and without the database**

Run: `FORGE_TEST_DATABASE_URL=... go test -race -count=1 ./internal/store/... -v -run 'TestPostgres|TestOpenPostgres' 2>&1 | tail -30`. Expected: every subtest of `TestPostgres` PASSes, plus `TestPostgresMigrate` and `TestOpenPostgresBadURL`.

Run: `env -u FORGE_TEST_DATABASE_URL go test -race -count=1 ./internal/store/...`. Expected: ok; the Postgres tests SKIP.

If pgx refuses `[]byte` for the jsonb parameter, pass `string(data)` instead, and note it in the report.

- [ ] **Step 6: Run Postgres in CI**

In `.github/workflows/ci.yml`, `jobs.test` gains a service and an env (add both under `runs-on: ubuntu-latest`):

```yaml
    services:
      postgres:
        # Docker Hub's official image through Google's mirror: Docker Hub
        # rate-limits anonymous pulls from shared runner IPs.
        image: mirror.gcr.io/library/postgres:18
        env:
          POSTGRES_PASSWORD: forge
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U postgres"
          --health-interval 5s --health-timeout 5s --health-retries 12
    env:
      FORGE_TEST_DATABASE_URL: postgres://postgres:forge@localhost:5432/postgres?sslmode=disable
```

- [ ] **Step 7: Lint and commit**

Run: `go mod tidy -diff && golangci-lint run ./... && go test -race ./...`. Expected: clean, and PASS.

```bash
git add go.mod go.sum Makefile .github/workflows/ci.yml internal/store
git commit -m "store: PostgreSQL store with embedded migrations; Postgres in CI"
```

---

### Task 4: The catalog on the store

**Files:**
- Modify: `internal/server/catalog.go` (the whole storage part), `internal/server/create.go:163`, `internal/server/source.go:39-42`, `internal/server/deploys.go:309`, `internal/server/ci_auth.go:111`
- Modify tests that call the old helpers: `internal/server/create_test.go:282,304,486`
- Create: `internal/server/catalog_store_test.go`

**Interfaces:**
- Consumes: `store.Store`, `store.Tx`, `store.ListQuery`, `store.ErrNotFound`, `store.NewMemory` (Task 2); `pgtest.URL`, `store.OpenPostgres`, `(*Postgres).Migrate` (Task 3).
- Produces:

```go
func NewCatalog(now func() time.Time) *Catalog                        // unchanged: memory store
func NewCatalogWithStore(s store.Store, now func() time.Time) *Catalog // new
// package-private, now context-aware and fallible:
func (c *Catalog) has(ctx context.Context, name string) (bool, error)
func (c *Catalog) get(ctx context.Context, name string) (*forgev1.Service, error) // NotFound or Unavailable connect errors
func (c *Catalog) ids(ctx context.Context) ([]string, error)
func (c *Catalog) servicesForRepo(ctx context.Context, org, repo string) ([]string, error)
```

**Ruling:** the spec says `NewCatalog(store, now)`. Keeping `NewCatalog(now)` for the memory store and adding `NewCatalogWithStore` avoids touching about twenty test call sites; the behaviour is the same. Task 8 aligns the spec's line.

- [ ] **Step 1: Write the failing tests in `catalog_store_test.go`**

```go
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/internal/store/pgtest"
)

// brokenStore fails every call, like a database that is down.
type brokenStore struct{}

var errDown = errors.New("database is down")

func (brokenStore) Get(context.Context, string) (*forgev1.Service, int64, error) { return nil, 0, errDown }
func (brokenStore) List(context.Context, store.ListQuery) ([]*forgev1.Service, error) {
	return nil, errDown
}
func (brokenStore) Names(context.Context) ([]string, error)                   { return nil, errDown }
func (brokenStore) ForRepo(context.Context, string, string) ([]string, error) { return nil, errDown }
func (brokenStore) Write(context.Context, func(store.Tx) error) error         { return errDown }
func (brokenStore) Close()                                                    {}

func TestCatalogStoreDown(t *testing.T) {
	t.Parallel()
	c := NewCatalogWithStore(brokenStore{}, nil)
	ctx := writerCtx()
	_, err := c.ListServices(ctx, connect.NewRequest(&forgev1.ListServicesRequest{}))
	assertUnavailable(t, "list", err)
	_, err = c.GetService(ctx, connect.NewRequest(&forgev1.GetServiceRequest{Name: "services/a"}))
	assertUnavailable(t, "get", err)
	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "a", Service: &forgev1.Service{OwnerTeam: "t"}}))
	assertUnavailable(t, "register", err)
	_, err = c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/a", OwnerTeam: "u"}, UpdateMask: fieldMask("owner_team"),
	}))
	assertUnavailable(t, "update", err)
	_, err = c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/a"}))
	assertUnavailable(t, "delete", err)
	_, err = c.has(ctx, "services/a")
	assertUnavailable(t, "has", err)
	_, err = c.get(ctx, "services/a")
	assertUnavailable(t, "helper get", err)
	_, err = c.ids(ctx)
	assertUnavailable(t, "ids", err)
	_, err = c.servicesForRepo(ctx, "showmino", "a")
	assertUnavailable(t, "servicesForRepo", err)
}

func assertUnavailable(t *testing.T, what string, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("%s: %v, want Unavailable", what, err)
	}
}

func TestGitHubOIDCStoreDown(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, NewCatalogWithStore(brokenStore{}, nil), &fakeBranches{}, clk)
	_, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("CI login with the catalog down: %v, want Unavailable (not 'not a Forge service')", err)
	}
}

// The catalog's own tests run on the memory store; this runs the rules that
// depend on transactions against PostgreSQL.
func TestCatalogOnPostgres(t *testing.T) {
	t.Parallel()
	pg, err := store.OpenPostgres(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := pg.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := NewCatalogWithStore(pg, nil)
	ctx := writerCtx()
	register(t, c, "ledger")
	pay := register(t, c, "payments", "services/ledger")

	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "t"}}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "x", Service: &forgev1.Service{OwnerTeam: "t", Dependencies: []string{"services/nope"}}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("missing dependency: %v", err)
	}
	_, err = c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", OwnerTeam: "u", Etag: `W/"9"`}, UpdateMask: fieldMask("owner_team"),
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("stale etag: %v", err)
	}
	up, err := c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", OwnerTeam: "u", Etag: pay.GetEtag()}, UpdateMask: fieldMask("owner_team"),
	}))
	if err != nil || up.Msg.GetService().GetEtag() != `W/"2"` {
		t.Fatalf("update: %v %v", up, err)
	}
	_, err = c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/ledger"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete a dependency: %v", err)
	}
	for _, id := range []string{"payments", "ledger"} {
		if _, err := c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/" + id})); err != nil {
			t.Errorf("delete %s: %v", id, err)
		}
	}
	if _, err := c.get(ctx, "services/payments"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("deleted: %v", err)
	}
}
```

Add this helper to the same file; no such helper exists yet, and `server_test.go` builds masks inline:

```go
func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
```

with the import `google.golang.org/protobuf/types/known/fieldmaskpb`.

Run: `go test ./internal/server/`. Expected: a build failure, `undefined: NewCatalogWithStore`.

- [ ] **Step 2: Rewrite the catalog's storage**

In `internal/server/catalog.go`:
- **Imports:** drop `sync` and `sort`; add `"github.com/showmino/forge/internal/store"`.
- **Replace:** everything from the `Catalog` type down to the end of `servicesForRepo`.
- **Rewrite:** `ListServices`' body after the page token is decoded, then `GetService`, `RegisterService`, `UpdateService`, `DeleteService` and `checkDependencies`.
- **Unchanged:** `validateService`, `parseName`, `etag`, `invalid`, `notFound`, `cloneSvc` and `updatable`.

```go
// Catalog is the ServiceCatalogService. Its rules live here; a store.Store
// keeps the services (in memory, or PostgreSQL in production).
type Catalog struct {
	store store.Store
	now   func() time.Time
}

// NewCatalog returns an empty catalog in memory.
func NewCatalog(now func() time.Time) *Catalog { return NewCatalogWithStore(store.NewMemory(), now) }

// NewCatalogWithStore returns a catalog kept in s.
func NewCatalogWithStore(s store.Store, now func() time.Time) *Catalog {
	if now == nil {
		now = time.Now
	}
	return &Catalog{store: s, now: now}
}

// storeErr passes connect errors through and turns a storage failure into
// Unavailable.
func storeErr(err error) error {
	var ce *connect.Error
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("service catalog storage: %w", err))
}

// has reports whether a service is registered under name.
func (c *Catalog) has(ctx context.Context, name string) (bool, error) {
	_, _, err := c.store.Get(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, storeErr(err)
}

// ids returns the registered service ids, sorted.
func (c *Catalog) ids(ctx context.Context) ([]string, error) {
	names, err := c.store.Names(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, servicePrefix)
	}
	return names, nil
}

// get returns the service registered under name: a NotFound error when
// there is none.
func (c *Catalog) get(ctx context.Context, name string) (*forgev1.Service, error) {
	s, _, err := c.store.Get(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	return s, storeErr(err)
}

// servicesForRepo returns the ids, sorted, of the services whose repo_url is
// https://github.com/<org>/<repo>, both compared case-insensitively.
func (c *Catalog) servicesForRepo(ctx context.Context, org, repo string) ([]string, error) {
	names, err := c.store.ForRepo(ctx, org, repo)
	if err != nil {
		return nil, storeErr(err)
	}
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, servicePrefix)
	}
	return names, nil
}
```

The `ListServices` tail, after `after` is decoded:

```go
	svcs, err := c.store.List(ctx, store.ListQuery{OwnerTeam: m.GetOwnerTeam(), After: after, Limit: size + 1})
	if err != nil {
		return nil, storeErr(err)
	}
	resp := &forgev1.ListServicesResponse{Services: svcs}
	if len(svcs) > size {
		resp.Services = svcs[:size]
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(svcs[size-1].GetName()))
	}
	return connect.NewResponse(resp), nil
```

`GetService` after `parseName`:

```go
	s, err := c.get(ctx, name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetServiceResponse{Service: s}), nil
```

`RegisterService` after `name := servicePrefix + m.GetServiceId()`:

```go
	var out *forgev1.Service
	err := c.store.Write(ctx, func(tx store.Tx) error {
		if key := m.GetRequestId(); key != "" {
			prev, err := tx.Recall(ctx, key)
			if err == nil {
				out = prev
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		switch _, _, err := tx.Get(ctx, name); {
		case err == nil:
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", name))
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if err := checkDependencies(ctx, tx, name, in); err != nil {
			return err
		}
		s := cloneSvc(in)
		s.Name = name
		now := timestamppb.New(c.now())
		s.CreateTime, s.UpdateTime = now, now
		s.Etag = etag(1)
		out = s
		if m.GetValidateOnly() {
			return nil
		}
		if err := tx.Put(ctx, s, 1); err != nil {
			return err
		}
		if key := m.GetRequestId(); key != "" {
			return tx.Remember(ctx, key, s)
		}
		return nil
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.RegisterServiceResponse{Service: out}), nil
```

`UpdateService`: everything after the mask check becomes:

```go
	var out *forgev1.Service
	err = c.store.Write(ctx, func(tx store.Tx) error {
		cur, rev, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
			return connect.NewError(connect.CodeAborted, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, in.GetEtag(), cur.GetEtag()))
		}
		next := cur // the store returned a copy
		for _, p := range paths {
			switch p {
			case "display_name":
				next.DisplayName = in.GetDisplayName()
			case "owner_team":
				next.OwnerTeam = in.GetOwnerTeam()
			case "owners":
				next.Owners = slices.Clone(in.GetOwners())
			case "lifecycle":
				next.Lifecycle = in.GetLifecycle()
			case "repo_url":
				next.RepoUrl = in.GetRepoUrl()
			case "dependencies":
				next.Dependencies = slices.Clone(in.GetDependencies())
			case "labels":
				next.Labels = in.GetLabels()
			}
		}
		if err := validateService(next); err != nil {
			return err
		}
		if err := checkDependencies(ctx, tx, name, next); err != nil {
			return err
		}
		next.UpdateTime = timestamppb.New(c.now())
		next.Etag = etag(rev + 1)
		out = next
		if req.Msg.GetValidateOnly() {
			return nil
		}
		return tx.Put(ctx, next, rev+1)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.UpdateServiceResponse{Service: out}), nil
```

`DeleteService` after `parseName`:

```go
	err = c.store.Write(ctx, func(tx store.Tx) error {
		cur, _, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if e := req.Msg.GetEtag(); e != "" && e != cur.GetEtag() {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, e, cur.GetEtag()))
		}
		deps, err := tx.Dependents(ctx, name)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a dependency of %s", name, deps[0]))
		}
		if req.Msg.GetValidateOnly() {
			return nil
		}
		return tx.Delete(ctx, name)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.DeleteServiceResponse{}), nil
```

`checkDependencies`:

```go
// checkDependencies requires every dependency of the service called name to be
// another registered service, so DeleteService's check is never undermined by
// dangling references.
func checkDependencies(ctx context.Context, tx store.Tx, name string, s *forgev1.Service) error {
	for _, d := range s.GetDependencies() {
		if d == name {
			return invalid(fmt.Sprintf("%s cannot depend on itself", name))
		}
		_, _, err := tx.Get(ctx, d)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("dependency %s is not registered", d))
		}
		if err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 3: Update the four callers**

`create.go` (in `create`):

```go
	exists, err := c.catalog.has(ctx, servicePrefix+id)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", servicePrefix, id))
	}
```

`source.go` (in `resolveSource`):

```go
	svc, err := cat.get(ctx, servicePrefix+sid)
	if err != nil {
		return s, err // NotFound "services/<id> not found", or Unavailable
	}
	if s.owner, s.repo, ok = repoOf(svc.GetRepoUrl(), org); !ok {
```

`ok` is the one from `strings.CutPrefix` above. Then delete the function's later `var err error` line, because `err` now exists; `s.commit, err = gh.ResolveRef(...)` stays as is.

`deploys.go` (in `ListDeployments`):

```go
	ids, err := d.o.Catalog.ids(ctx)
	if err != nil {
		return nil, err
	}
	for _, svc := range ids {
```

`ci_auth.go`:

```go
	ids, err := a.o.Catalog.servicesForRepo(ctx, a.o.Org, repo)
	if err != nil {
		return nil, err
	}
```

Use the context variable that function already has; if it is named differently, use that name.

In `create_test.go`, the three `e.catalog.has("services/payments")` calls become a helper defined in that file:

```go
func registered(t *testing.T, c *Catalog, name string) bool {
	t.Helper()
	ok, err := c.has(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
```

and the calls become `registered(t, e.catalog, "services/payments")`. Line 486's loop runs in the test goroutine, so `t.Fatal` is allowed there. Check with `grep -n "go func" internal/server/create_test.go` that none of the three is inside a goroutine; if one is, use `ok, _ := e.catalog.has(...)` there instead.

- [ ] **Step 4: Run the server tests**

Run: `go build ./... && go test -race -count=1 ./internal/server/ ./internal/cli/ ./cmd/...`. Expected: PASS. `TestCatalogOnPostgres` SKIPs without the database.

Run: `FORGE_TEST_DATABASE_URL=... go test -race -count=1 -run 'TestCatalogOnPostgres|TestCatalogStoreDown|TestGitHubOIDCStoreDown' ./internal/server/ -v`. Expected: all three PASS.

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./...`. Expected: clean.

```bash
git add internal/server
git commit -m "server: the catalog stores through store.Store; storage failures are Unavailable"
```

---

### Task 5: `forge-api --database-url`

**Files:**
- Create: `cmd/forge-api/store.go`, `cmd/forge-api/store_test.go`
- Modify: `cmd/forge-api/main.go` (header comment, flags, catalog construction)

**Interfaces:**
- Consumes: `store.NewMemory`, `store.OpenPostgres`, `(*Postgres).Migrate`, `store.Store` (Tasks 2 and 3); `server.NewCatalogWithStore` (Task 4); `pgtest.URL` (Task 3).
- Produces: `func openStore(ctx context.Context, url string, log *slog.Logger) (store.Store, error)`.

- [ ] **Step 1: Write the failing tests in `store_test.go`**

```go
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/internal/store/pgtest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestOpenStoreMemory(t *testing.T) {
	t.Parallel()
	s, err := openStore(context.Background(), "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*store.Memory); !ok {
		t.Fatalf("no URL: got %T, want *store.Memory", s)
	}
}

// catalogClient serves a catalog kept in s, as forge-api does, to a caller
// who may write.
func catalogClient(t *testing.T, s store.Store) forgev1connect.ServiceCatalogServiceClient {
	t.Helper()
	h, err := server.New(server.Options{
		Catalog:       server.NewCatalogWithStore(s, nil),
		Authenticator: server.StaticAuthenticator{"w": {Subject: "ana", Groups: []string{"platform"}}},
		Authorizer:    server.Authorizer{WriterGroups: []string{"platform"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
}

func authed[T any](msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer w")
	return r
}

func TestCatalogSurvivesRestart(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	ctx := context.Background()
	register := &forgev1.RegisterServiceRequest{
		ServiceId: "payments", RequestId: "req-1",
		Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	}

	first, err := openStore(ctx, url, quiet)
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalogClient(t, first).RegisterService(ctx, authed(register))
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := openStore(ctx, url, quiet) // a restart: connects and migrates again
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	client := catalogClient(t, second)
	got, err := client.GetService(ctx, authed(&forgev1.GetServiceRequest{Name: "services/payments"}))
	if err != nil || got.Msg.GetService().GetEtag() != created.Msg.GetService().GetEtag() {
		t.Fatalf("after restart: %v %v", got, err)
	}
	retry, err := client.RegisterService(ctx, authed(register))
	if err != nil || retry.Msg.GetService().GetName() != "services/payments" {
		t.Fatalf("retried request_id after restart: %v %v (want the stored service)", retry, err)
	}
}
```

Run: `go test ./cmd/forge-api/`. Expected: a build failure, `undefined: openStore`.

- [ ] **Step 2: Write `store.go`**

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/showmino/forge/internal/store"
)

// openStore returns the service catalog's store: PostgreSQL at url, migrated,
// or memory when url is empty. The database may start after forge-api (k3s),
// so connecting is retried for about 45 seconds. Never log url: it holds the
// password.
func openStore(ctx context.Context, url string, log *slog.Logger) (store.Store, error) {
	if url == "" {
		log.Warn("no --database-url: the service catalog is in memory and is lost when forge-api stops")
		return store.NewMemory(), nil
	}
	var (
		pg  *store.Postgres
		err error
	)
	for attempt := 1; ; attempt++ {
		pg, err = store.OpenPostgres(ctx, url)
		if err == nil || attempt == 10 || ctx.Err() != nil {
			break
		}
		log.Info("waiting for the database", "attempt", attempt, "error", err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if err != nil {
		return nil, err
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		return nil, fmt.Errorf("migrate the database: %w", err)
	}
	log.Info("service catalog in PostgreSQL")
	return pg, nil
}
```

- [ ] **Step 3: Wire it into `main.go`**

- **Header comment:** "the service catalog (in memory)" becomes "the service catalog (in PostgreSQL with --database-url, else in memory)".
- **The flag:** in the `var (...)` flag block, after `writers`:

  ```go
  		dbURL        = flag.String("database-url", "", "PostgreSQL URL for the service catalog, or FORGE_DATABASE_URL (empty = in memory, lost on restart)")
  ```

- **The environment:** directly after `flag.Parse()` and the `printVersion` check:

  ```go
  	if *dbURL == "" {
  		// Not the flag's default: --help prints defaults, and the URL holds a password.
  		*dbURL = os.Getenv("FORGE_DATABASE_URL")
  	}
  ```

- **The store:** replace `catalog, ops := server.NewCatalog(nil), server.NewOperations(nil)` with:

  ```go
  	st, err := openStore(ctx, *dbURL, log)
  	if err != nil {
  		return err
  	}
  	defer st.Close()
  	catalog, ops := server.NewCatalogWithStore(st, nil), server.NewOperations(nil)
  ```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./cmd/forge-api/` (expected: PASS, with `TestCatalogSurvivesRestart` SKIPped), then the same command with `FORGE_TEST_DATABASE_URL=...` set (expected: PASS, including that test).

Run: `B=$(mktemp -d) && go build -o "$B/forge-api" ./cmd/forge-api && FORGE_DATABASE_URL='postgres://u:topsecret@h/db' "$B/forge-api" --help 2>&1 | grep -c topsecret`. Expected: `0`.

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./... && go test -race ./...`. Expected: clean, and PASS.

```bash
git add cmd/forge-api
git commit -m "forge-api: --database-url keeps the catalog in PostgreSQL"
```

---

### Task 6: k3s GitHub login and writer groups

**Files:**
- Modify: `deploy/k3s/forge.yaml.tmpl` (Dex ConfigMap and Deployment, forge-api env), `deploy/k3s/render.sh`, `deploy/k3s/render_test.sh`
- Create: `deploy/k3s/dex-onepassword.yaml.tmpl`
- Regenerate: `deploy/k3s/testdata/lb.golden`, `lb-app.golden`, `previews.golden`

**Interfaces:** the render.sh variables and object names from Global Constraints.

- [ ] **Step 1: Write the failing render tests**

In `render_test.sh`, add after the existing `check "bad App id refused"` line:

```sh
check "Dex: no password login"          sh -c "! printf '%s' \"\$1\" | grep -q -e staticPasswords -e enablePasswordDB -e 'hash:'" _ "$with"
check "Dex: GitHub connector for the org" sh -c "printf '%s' \"\$1\" | grep -qF 'type: github' && printf '%s' \"\$1\" | grep -qF -- '- name: showmino' && printf '%s' \"\$1\" | grep -qF 'teamNameField: slug' && printf '%s' \"\$1\" | grep -qF 'redirectURI: http://dex.10.150.1.220.nip.io/dex/callback'" _ "$with"
check "Dex: OAuth credentials only from the Secret" sh -c "printf '%s' \"\$1\" | grep -qF 'clientSecret: \$GITHUB_CLIENT_SECRET' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: dex-github, key: client_id}' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: dex-github, key: client_secret}'" _ "$with"
check "writer groups default to platform-engineering" sh -c "printf '%s' \"\$1\" | grep -qF 'FORGE_WRITER_GROUPS, value: \"showmino:platform-engineering\"'" _ "$without"
check "writer groups can be set"        sh -c "FORGE_WRITER_GROUPS=showmino:a,showmino:b sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -qF 'FORGE_WRITER_GROUPS, value: \"showmino:a,showmino:b\"'"
check "empty writer groups refused"     sh -c "! FORGE_WRITER_GROUPS= sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "bad writer groups refused"       sh -c "! FORGE_WRITER_GROUPS='a;rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "Dex org follows FORGE_GITHUB_ORG" sh -c "out=\$(FORGE_GITHUB_ORG=acme sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221) && printf '%s' \"\$out\" | grep -qF -- '- name: acme' && printf '%s' \"\$out\" | grep -qF 'value: \"acme:platform-engineering\"'"
check "bad Dex org refused"             sh -c "! FORGE_DEX_GITHUB_ORG='a/b' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
check "no Dex 1Password item by default" sh -c "! printf '%s' \"\$1\" | grep -A3 'kind: OnePasswordItem' | grep -qF 'name: dex-github'" _ "$onepw"
check "Dex 1Password item syncs dex-github" sh -c "FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -A3 'kind: OnePasswordItem' | grep -qF 'name: dex-github' && FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 | grep -qF 'itemPath: vaults/Kube/items/forge-dex-github'"
check "bad Dex 1Password item refused"  sh -c "! FORGE_DEX_ONEPASSWORD_ITEM='x; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
```

Also change the existing `check "no App: no App settings"`: it greps `-e FORGE_GITHUB_APP -e forge-github-app`, which is still right; leave it as is.

Run: `sh deploy/k3s/render_test.sh`. Expected: the new checks FAIL, and so do the three "output unchanged" golden checks once the template changes.

- [ ] **Step 2: Change the templates**

In `forge.yaml.tmpl`, the Dex `config.yaml` replaces everything from `staticClients:` to the end of the `staticPasswords` block with:

```yaml
    staticClients:
      - id: forge-cli
        name: Forge CLI
        public: true
        redirectURIs: [/device/callback, http://127.0.0.1:8085/callback]
    # Only members of the org log in; groups are "<org>:<team-slug>", and
    # forge-api's FORGE_WRITER_GROUPS picks the teams that may write. Dex
    # expands $GITHUB_CLIENT_* from its environment (the Secret dex-github).
    connectors:
      - type: github
        id: github
        name: GitHub
        config:
          clientID: $GITHUB_CLIENT_ID
          clientSecret: $GITHUB_CLIENT_SECRET
          redirectURI: http://${DEX_HOST}/dex/callback
          orgs:
            - name: ${FORGE_DEX_GITHUB_ORG}
          teamNameField: slug
```

In the Dex container, after `args:`:

```yaml
          env:
            - name: GITHUB_CLIENT_ID
              valueFrom:
                secretKeyRef: {name: dex-github, key: client_id}
            - name: GITHUB_CLIENT_SECRET
              valueFrom:
                secretKeyRef: {name: dex-github, key: client_secret}
```

Then re-render and check that the output has `secretKeyRef: {name: dex-github, key: client_id}` on one line, as the test greps it.

In the forge-api `env:` list, after `FORGE_OIDC_CLIENT_ID`:

```yaml
            - {name: FORGE_WRITER_GROUPS, value: "${FORGE_WRITER_GROUPS}"}
```

In the header comment of `forge.yaml.tmpl`, add:

```yaml
#   ${FORGE_DEX_GITHUB_ORG}, ${FORGE_WRITER_GROUPS}: GitHub login; see render.sh.
```

Create `deploy/k3s/dex-onepassword.yaml.tmpl`:

```yaml
# Optional: the 1Password operator syncs Dex's GitHub OAuth App credentials
# into the Secret dex-github. render.sh adds this when FORGE_DEX_ONEPASSWORD_ITEM
# is set, e.g. vaults/Kube/items/forge-dex-github; the item needs the fields
# client_id and client_secret.
apiVersion: onepassword.com/v1
kind: OnePasswordItem
metadata:
  name: dex-github
  namespace: forge
spec:
  itemPath: ${FORGE_DEX_ONEPASSWORD_ITEM}
```

- [ ] **Step 3: Change render.sh**

- **Usage comment:** add after the 1Password paragraph:

  ```sh
  # Login is through GitHub (Dex's GitHub connector): only members of
  # FORGE_DEX_GITHUB_ORG (default FORGE_GITHUB_ORG) log in, and only the teams
  # in FORGE_WRITER_GROUPS (default <org>:platform-engineering; never empty) may
  # change anything. Dex reads the OAuth App's client_id and client_secret from
  # the Secret dex-github, which the 1Password operator can create:
  #   FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github ... | kubectl apply -f -
  ```

- **Settings:** after the `case $org$templates_repo` validation:

  ```sh
  dex_org=${FORGE_DEX_GITHUB_ORG:-$org}
  case $dex_org in
    ''|*[!A-Za-z0-9-]*) echo "invalid FORGE_DEX_GITHUB_ORG: $dex_org" >&2; exit 2 ;;
  esac
  # No colon: an explicitly empty FORGE_WRITER_GROUPS is refused, not defaulted.
  writer_groups=${FORGE_WRITER_GROUPS-$dex_org:platform-engineering}
  case $writer_groups in
    '') echo "FORGE_WRITER_GROUPS must not be empty: every login could deploy to production" >&2; exit 2 ;;
    *[!A-Za-z0-9:,_.-]*) echo "invalid FORGE_WRITER_GROUPS: $writer_groups" >&2; exit 2 ;;
  esac
  dex_onepassword_item=${FORGE_DEX_ONEPASSWORD_ITEM:-}
  case $dex_onepassword_item in
    *[!A-Za-z0-9./_-]*) echo "invalid FORGE_DEX_ONEPASSWORD_ITEM: $dex_onepassword_item" >&2; exit 2 ;;
  esac
  ```

- **The Dex item:** in the `{ ... }` block, after the `onepassword_item` section:

  ```sh
    if [ -n "$dex_onepassword_item" ]; then
      echo "---"
      cat "$dir/dex-onepassword.yaml.tmpl"
    fi
  ```

- **The `sed` line:** add `-e "s|\${FORGE_DEX_GITHUB_ORG}|$dex_org|g" -e "s|\${FORGE_WRITER_GROUPS}|$writer_groups|g" -e "s|\${FORGE_DEX_ONEPASSWORD_ITEM}|$dex_onepassword_item|g"`.

- [ ] **Step 4: Regenerate the goldens and read their diff**

```bash
sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 > deploy/k3s/testdata/lb.golden
FORGE_GITHUB_APP_ID=5239131 sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 > deploy/k3s/testdata/lb-app.golden
FORGE_GITHUB_APP_ID=5239131 FORGE_PREVIEW_IP=10.150.1.222 sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 > deploy/k3s/testdata/previews.golden
git diff --stat deploy/k3s/testdata && git diff deploy/k3s/testdata/lb.golden
```

Expected: the diff shows only the removed static password, the connector, Dex's env and `FORGE_WRITER_GROUPS`. `deploys.golden` is unchanged. In `render_test.sh`, the comment `# previews.golden is the render at 9bcbeff, before deploys existed.` becomes `# previews.golden is the previews render without deploys.`

- [ ] **Step 5: Run the render tests**

Run: `sh deploy/k3s/render_test.sh`. Expected: every line `ok`, including `valid YAML for kubectl`.

- [ ] **Step 6: Commit**

```bash
git add deploy/k3s
git commit -m "k3s: log in through GitHub; only platform-engineering may write"
```

---

### Task 7: k3s database: CNPG `forge-db`, Dex on Postgres, forge-api's URL

**Files:**
- Create: `deploy/k3s/database.yaml.tmpl`
- Modify: `deploy/k3s/forge.yaml.tmpl`, `deploy/k3s/render.sh` (always append the database), `deploy/k3s/render_test.sh`
- Regenerate: `deploy/k3s/testdata/lb.golden`, `lb-app.golden`, `previews.golden`

- [ ] **Step 1: Write the failing render tests**

Add to `render_test.sh`, after the Task 6 checks:

```sh
check "database: CNPG cluster forge-db" sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Cluster' && printf '%s' \"\$1\" | grep -qF 'name: forge-db' && printf '%s' \"\$1\" | grep -qF 'instances: 2' && printf '%s' \"\$1\" | grep -qF 'imageName: ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie' && printf '%s' \"\$1\" | grep -qF 'storageClass: longhorn-retain'" _ "$with"
check "database: dex database"          sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Database' && printf '%s' \"\$1\" | grep -qF 'name: forge-db-dex' && printf '%s' \"\$1\" | grep -qF 'name: dex'" _ "$with"
check "database: rendered without an App too" sh -c "printf '%s' \"\$1\" | grep -qF 'kind: Cluster'" _ "$without"
check "Dex: storage in Postgres"        sh -c "printf '%s' \"\$1\" | grep -qF 'type: postgres' && printf '%s' \"\$1\" | grep -qF 'host: forge-db-rw.forge.svc' && printf '%s' \"\$1\" | grep -qF 'password: \$DEX_DB_PASSWORD' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: forge-db-app, key: password}'" _ "$with"
check "Dex: no memory storage"          sh -c "! printf '%s' \"\$1\" | grep -qF 'type: memory'" _ "$with"
check "forge-api: database URL from the Secret" sh -c "printf '%s' \"\$1\" | grep -qF 'name: FORGE_DATABASE_URL' && printf '%s' \"\$1\" | grep -qF 'secretKeyRef: {name: forge-db-app, key: uri}'" _ "$with"
```

Run: `sh deploy/k3s/render_test.sh`. Expected: the new checks FAIL.

- [ ] **Step 2: Write `database.yaml.tmpl`**

```yaml
# Forge's database: the service catalog (forge-api) and Dex's logins.
# CloudNativePG creates the Secret forge-db-app (keys uri, password, ...)
# for the owner forge. No backups yet: the catalog can be re-registered from
# each repo's forge.yaml, and Dex's data is only sessions.
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: forge-db
  namespace: forge
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:18.4-system-trixie
  storage:
    size: 5Gi
    storageClass: longhorn-retain # the volumes outlive an accidental delete
  bootstrap:
    initdb:
      database: forge
      owner: forge
  resources:
    requests: {cpu: 50m, memory: 256Mi}
    limits: {memory: 512Mi}
---
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: forge-db-dex
  namespace: forge
spec:
  name: dex
  owner: forge
  cluster:
    name: forge-db
```

In `render.sh`, append it to every render. In the `{ ... }` block, after `cat "$exposure"`:

```sh
  echo "---"
  cat "$dir/database.yaml.tmpl"
```

- [ ] **Step 3: Point Dex and forge-api at it**

In `forge.yaml.tmpl`, the Dex `storage:` becomes:

```yaml
    storage:
      type: postgres
      config:
        host: forge-db-rw.forge.svc
        port: 5432
        database: dex
        user: forge
        password: $DEX_DB_PASSWORD # from the Secret forge-db-app
        ssl:
          mode: require
```

The Dex container's `env:` gains:

```yaml
            - name: DEX_DB_PASSWORD
              valueFrom:
                secretKeyRef: {name: forge-db-app, key: password}
```

The forge-api `env:` gains, after `FORGE_WRITER_GROUPS`:

```yaml
            - name: FORGE_DATABASE_URL
              valueFrom:
                secretKeyRef: {name: forge-db-app, key: uri}
```

The comment `replicas: 1 # the Phase 1 catalog is in memory; keep one replica` becomes `replicas: 1 # operations are in memory; keep one replica`.

- [ ] **Step 4: Regenerate the goldens and run the render tests**

Run the three golden commands from Task 6 Step 4, then read `git diff deploy/k3s/testdata/lb.golden`. Expected: only the database objects and the Dex and forge-api env and storage changes.

Run: `sh deploy/k3s/render_test.sh`. Expected: every line `ok`.

- [ ] **Step 5: Check the rendered Dex config against a real Postgres**

This proves that Dex v2.41.1 expands `$DEX_DB_PASSWORD` in its storage config and `$GITHUB_CLIENT_*` in its connector config. Use a scratch directory, `S=$(mktemp -d)`:

```bash
sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 |
  awk '/config\.yaml: [|]/ {on=1; next} on && /^---/ {exit} on {sub(/^    /, ""); print}' |
  sed -e 's/host: forge-db-rw.forge.svc/host: forge-dexcheck-pg/' -e 's/mode: require/mode: disable/' > "$S/config.yaml"
docker network create forge-dexcheck
docker run -d --name forge-dexcheck-pg --network forge-dexcheck \
  -e POSTGRES_USER=forge -e POSTGRES_PASSWORD=checkpw -e POSTGRES_DB=forge mirror.gcr.io/library/postgres:18
python3 -I -c 'import subprocess,time
for _ in range(30):
    if subprocess.run(["docker","exec","forge-dexcheck-pg","pg_isready","-U","forge"],capture_output=True).returncode==0: break
    time.sleep(1)'
docker exec forge-dexcheck-pg psql -U forge -c 'CREATE DATABASE dex'
docker run -d --name forge-dexcheck-dex --network forge-dexcheck -p 15556:5556 \
  -e GITHUB_CLIENT_ID=check-id -e GITHUB_CLIENT_SECRET=check-secret -e DEX_DB_PASSWORD=checkpw \
  -v "$S/config.yaml:/etc/dex/config.yaml:ro" ghcr.io/dexidp/dex:v2.41.1 dex serve /etc/dex/config.yaml
python3 -I -c 'import time,urllib.request
for _ in range(30):
    try:
        print(urllib.request.urlopen("http://127.0.0.1:15556/dex/healthz").status); break
    except Exception: time.sleep(1)'
docker exec forge-dexcheck-pg psql -U forge -d dex -c '\dt' | grep -c 'table'
docker logs forge-dexcheck-dex 2>&1 | tail -20
docker rm -f forge-dexcheck-dex forge-dexcheck-pg && docker network rm forge-dexcheck
```

Expected:
- the health check prints `200`;
- `\dt` lists Dex's tables, so the count is more than 0;
- the logs show the GitHub connector loaded and no storage error.

If Dex can't connect, `$DEX_DB_PASSWORD` was not expanded: stop and report BLOCKED with the log. The fallback, for the controller to rule on, is a Dex config Secret instead of the ConfigMap. The value `check-secret` is a dummy, not a secret.

- [ ] **Step 6: Commit**

```bash
git add deploy/k3s
git commit -m "k3s: CNPG cluster forge-db; Dex and the catalog in PostgreSQL"
```

---

### Task 8: Docs

**Files:**
- Modify: `README.md`, `docs/index.md`, `.claude/skills/forge/SKILL.md`, `docs/specs/2026-10-09-groundwork-auth-storage-design.md` (one line)

- [ ] **Step 1: README**

- **The layout line** `internal/server/    API handlers: catalog (in memory), auth, OIDC verification` becomes `internal/server/    API handlers: catalog, auth, OIDC verification`. Add a line after it: `internal/store/     catalog storage: memory or PostgreSQL (pgx, embedded migrations)`.
- **The local dev section:** after the line with `bin/forge auth login  # open the URL, log in as admin@example.com / password`, add a short paragraph:

  ```markdown
  The API keeps the catalog in memory unless it gets `--database-url` (or
  `FORGE_DATABASE_URL`). `make db` starts a local PostgreSQL on port 55432 for
  that and for the store tests:
  `FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable' make test`.
  ```

- **The k3s section:** replace the sentence "Change the static password in the ConfigMap before sharing the cluster." with:

  ```markdown
  On k3s you log in through GitHub: only members of `FORGE_DEX_GITHUB_ORG`
  (default the GitHub org) can log in, and only the teams in
  `FORGE_WRITER_GROUPS` (default `<org>:platform-engineering`) may register,
  create, preview or deploy. Create a GitHub OAuth App owned by the org with the
  callback `http://dex.<dex-ip>.nip.io/dex/callback`, and put its `client_id` and
  `client_secret` in the Secret `dex-github`, or in a 1Password item named by
  `FORGE_DEX_ONEPASSWORD_ITEM`. The render also creates the CloudNativePG cluster
  `forge-db` (2 instances, `longhorn-retain`), which holds the service catalog
  and Dex's logins; it needs the CloudNativePG operator. There are no backups yet.
  ```

- [ ] **Step 2: `docs/index.md`**

Line 228, "Phases 1 to 3 are done; Phase 4 is next.", becomes "Phases 1 to 4 are done; Phase 5 is next. Before it, the k3s install moved to GitHub login and PostgreSQL storage (`docs/specs/2026-10-09-groundwork-auth-storage-design.md`)."

- [ ] **Step 3: The spec line and the skill**

- **In the spec:** "`NewCatalog(store, now)` takes the store; a nil store means `store.NewMemory()`." becomes "`NewCatalog(now)` keeps the memory store, and `NewCatalogWithStore(s, now)` takes one."
- **In `.claude/skills/forge/SKILL.md`:** add to its status, in the style already there, that the groundwork (GitHub login, writer group `showmino:platform-engineering`, `forge-db` with the catalog and Dex) is done. Add "database backups" to the follow-up backlog.

- [ ] **Step 4: Commit**

```bash
git add README.md docs .claude/skills/forge/SKILL.md
git commit -m "docs: GitHub login, writer groups and the database"
```

---

### Task 9: Rollout on k3s (controller only, with the owner's OK)

No subagent runs this task. Run it after the final review, after the PR is merged to `main`, and after the owner says go.

- [ ] **R1, image.** From a clean checkout of merged `main`, build and push forge-api:
  - build with `--build-arg VERSION=$(git describe --tags --always) --build-arg COMMIT=$(git rev-parse HEAD)`;
  - tag `ghcr.io/showmino/forge-api:<sha7>` and `docker push` it.

  The chart is unchanged, so `FORGE_DEPLOY_CHART_REVISION` stays at `18b12326d3df2c1a11b9f664f378b579eb472aaf`. Read it live first, with the jsonpath command in render.sh.

- [ ] **R2, save the catalog.** Log in with the current static user and run `forge service list -o yaml > <scratchpad>/catalog-before.yaml`.

- [ ] **R3, rollback file.**
  - Save `kubectl -n forge get configmap dex-config deploy/dex deploy/forge-api -o yaml` to `<scratchpad>/rollback-before-groundwork.yaml`.
  - Note the render command in use today:

    ```sh
    FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=18b12326d3df2c1a11b9f664f378b579eb472aaf FORGE_PREVIEW_IP=10.150.1.222 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_GITHUB_APP_ID=5239131 FORGE_API_IMAGE=ghcr.io/showmino/forge-api:18b1232 deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221
    ```

- [ ] **R4, render, dry run, diff.**
  - Render with the same command plus `FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github` and the new `FORGE_API_IMAGE`, into `<scratchpad>/render-groundwork.yaml`.
  - Run `kubectl apply --dry-run=server -f`, then `kubectl diff -f`.
  - Read the diff. It should show only the new objects (Cluster, Database, OnePasswordItem `dex-github`), the Dex ConfigMap and Deployment changes, and forge-api's image and env.
  - Any change to Argo CD objects, the Gateway or RBAC is a stop.

- [ ] **R5, apply.**
  - Apply the render.
  - Wait for the Secret: `kubectl -n forge get secret dex-github` must exist, which the 1Password operator syncs. Check its key names only, never its values.
  - `kubectl -n forge wait cluster/forge-db --for=condition=Ready --timeout=10m`.
  - `kubectl -n forge rollout status deploy/dex deploy/forge-api --timeout=5m`.
  - In forge-api's logs, look for "service catalog in PostgreSQL".

- [ ] **R6, re-register.** Re-register every service in `catalog-before.yaml` with `forge service register`, using each entry's id, team and repo URL.

- [ ] **R7, checks.**
  1. `forge auth login` goes through GitHub. `forge auth status` shows the GitHub email and `showmino:platform-engineering`.
  2. Run `forge service register` for a test entry, then `kubectl -n forge rollout restart deploy/forge-api`. After the restart the entry is still listed; then delete it.
  3. Run `kubectl -n forge rollout restart deploy/dex`; after it, `forge service list` works without a new login, through a token refresh.
  4. CI: re-run the last `deploy` job of a registered service, or push to a test service's `main`, and watch it go green.
  5. `kubectl -n forge get cluster forge-db` shows 2 ready instances; `argocd app list` shows nothing new OutOfSync.

- [ ] **R8, rollback (only if R5 to R7 fail).** Apply `rollback-before-groundwork.yaml`. The Cluster, Database and OnePasswordItem stay in place, unused, until the owner OKs removing them.
