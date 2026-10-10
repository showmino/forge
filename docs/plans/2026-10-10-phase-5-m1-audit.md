# Phase 5 Milestone 1: the audit log — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record every write call to the Forge API, refused calls included, with the outcomes of long-running work and forge-api's own actions, in an append-only `audit_events` table that `forge audit list/get` can read.

**Architecture:**
- **Capture:** an audit Connect interceptor, outermost after logging, builds one event per write procedure from a table of actions; the auth interceptor fills in the actor.
- **Outcomes:** the Operations runner appends an `outcome` event when work ends, and the preview reaper appends `system` events.
- **Storage:** events go to an `AuditStore` (memory or PostgreSQL, migration `0002`).
- **Reading:** a new `AuditService` (List/Get) and a CLI noun `audit` read them.

**Tech Stack:** Go 1.27, ConnectRPC, buf, pgx v5, Cobra, protojson.

**Spec:** `docs/specs/2026-10-10-phase-5-m1-audit-design.md`

## Global Constraints

- **Read access:**
  - Reading audit events needs `services.read`.
  - CI workloads can't read them: the read procedures are NOT added to `workloadProcedures`.
  - Reads are never audited.
- **Append-only:** no API call updates or deletes an audit event, and the Postgres trigger rejects `UPDATE`, `DELETE` and `TRUNCATE` on `audit_events`.
- **No caller text or secrets in events:**
  - `details` holds only identifiers and settings from the per-action allowlist in Task 3.
  - Never tokens, never free text from the caller, never request bodies.
- **Clipping:** `result.message` is at most 500 characters, cut on a UTF-8 boundary.
- **Exact names:**
  - actions: `service.register`, `service.update`, `service.delete`, `service.create`, `environment.create`, `environment.extend`, `environment.destroy`, `environment.expire`, `deploy.start`, `deploy.rollback`, `deploy.promote`, `operation.cancel`;
  - actor kinds: `user`, `workload`, `system`, `anonymous`;
  - event kinds: `request`, `outcome`;
  - result codes: `ok`, the Connect code's string (`permission_denied`, …), and for outcomes `ok`, `failed`, `cancelled`.
- **Paging:** `ListAuditEvents` returns newest first; default page size 50, max 500.
- **Append failure:** the call's own result is returned unchanged, and forge-api logs `audit event not stored` at ERROR with the event as JSON.
- **Tests and tooling:**
  - `go test ./...` passes with no database. Postgres tests run when `FORGE_TEST_DATABASE_URL` is set (local: `make db` → `postgres://postgres:forge@localhost:55432/postgres?sslmode=disable`).
  - Lint with golangci-lint v2.14.0 (`make lint`, which also runs `buf lint`), then run `make test`.
  - After proto changes, run `make generate` and commit `gen/`.
- **The cluster is production:**
  - Implementers may only use read-only `kubectl` or `--dry-run=client`.
  - Implementers never push or merge.
  - Task 8 is for the controller only, with the owner's OK.

## Review Focus

1. **Refused before authentication:** a write call with no token or a bad token is recorded with actor `anonymous` and result `unauthenticated`, never with the token. Pinned in `TestAuditRefusedCalls` (Task 3).
2. **Client disconnect:** an event is still appended when the client goes away mid-call, because the append uses a context that isn't cancelled. Pinned in `TestAuditAppendSurvivesCancel` (Task 3).
3. **Paging while new events arrive:** a page token taken before new writes still returns the next older events, with no duplicates and no gaps. Pinned in `runAuditSuite` "paging is stable" (Task 2) and `TestListAuditEventsPaging` (Task 5).
4. **Service on calls whose request has none:** destroy and extend of an environment, and cancelling an operation, still carry the service. Pinned in `TestAuditEnvironmentService` (Task 3) and `TestAuditCancelService` (Task 4).
5. **A new RPC added later:** it can't ship unaudited. Pinned in `TestAuditCoversEveryProcedure` (Task 3).

---

## File structure

| File | Responsibility |
| --- | --- |
| `proto/forge/v1/audit.proto`, `gen/forge/v1/...` | AuditService and AuditEvent |
| `pkg/client/client.go` | `Client.Audit` |
| `internal/store/audit.go` | `AuditStore`, `AuditQuery`, `Backend`, `AuditEventName`, `ParseAuditEventName` |
| `internal/store/audit_memory.go`, `audit_postgres.go` | the two implementations |
| `internal/store/migrations/0002_audit.sql` | table, indexes, append-only triggers |
| `internal/store/audit_suite_test.go`, `audit_test.go` | shared suite run on both, Postgres append-only test |
| `internal/server/audit.go` | event record in the context, actions table, interceptor, auditor |
| `internal/server/audit_test.go` | interceptor, refused calls, completeness, append failure |
| `internal/server/audit_service.go`, `audit_service_test.go` | List/Get handlers |
| `internal/server/operations.go`, `operation_service.go`, `environments.go`, `env_destroy.go`, `authn.go`, `server.go` | hooks |
| `cmd/forge-api/store.go`, `main.go` | `openStore` returns `store.Backend`; wire `Options.Audit` |
| `internal/cli/audit.go`, `audit_test.go`, `testdata/audit_*.golden` | `forge audit list/get` |
| `README.md`, `docs/index.md`, spec, `.claude/skills/forge/SKILL.md` | docs |

---

### Task 1: AuditService proto and client

**Files:**
- Create: `proto/forge/v1/audit.proto`
- Generate: `gen/forge/v1/audit.pb.go`, `gen/forge/v1/forgev1connect/audit.connect.go` (`make generate`)
- Modify: `pkg/client/client.go` (add `Audit`)

**Interfaces:**
- Produces:
  - Go types `forgev1.AuditEvent`, `AuditActor`, `AuditResult`, `AuditClient`, `ListAuditEventsRequest`, `ListAuditEventsResponse`, `GetAuditEventRequest`, `GetAuditEventResponse`;
  - `forgev1connect.AuditServiceHandler` and `AuditServiceClient`;
  - procedure constants `forgev1connect.AuditServiceListAuditEventsProcedure` and `AuditServiceGetAuditEventProcedure`;
  - `client.Client.Audit forgev1connect.AuditServiceClient`.

- [ ] **Step 1: Write `audit.proto`**

```proto
syntax = "proto3";

package forge.v1;

import "google/protobuf/timestamp.proto";

// AuditService reads the audit log: one event per write call to the Forge
// API, refused calls included, plus the outcome of long-running work and
// forge-api's own actions. Events are append-only. Resource names follow AIP
// style: "auditEvents/{id}", where a higher id is a later event.
service AuditService {
  // ListAuditEvents returns events newest first.
  rpc ListAuditEvents(ListAuditEventsRequest) returns (ListAuditEventsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // GetAuditEvent returns one event.
  rpc GetAuditEvent(GetAuditEventRequest) returns (GetAuditEventResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

// AuditEvent is one entry of the audit log.
message AuditEvent {
  // "auditEvents/1234".
  string name = 1;
  google.protobuf.Timestamp time = 2;
  // "request" (a write call) or "outcome" (how long-running work ended).
  string kind = 3;
  // For example "deploy.promote".
  string action = 4;
  AuditActor actor = 5;
  // "services/payments" when the action concerns a service.
  string service = 6;
  // The resource acted on, for example "deployments/payments/production".
  string target = 7;
  AuditResult result = 8;
  // "operations/op_..." when the call started long-running work.
  string operation = 9;
  // A few identifiers and settings of the request, per action.
  map<string, string> details = 10;
  AuditClient client = 11;
}

// AuditActor is who acted.
message AuditActor {
  // "user", "workload" (a CI job), "system" (forge-api) or "anonymous".
  string kind = 1;
  string subject = 2;
  // Set for users.
  string email = 3;
  // "<org>/<repo>", set for workloads.
  string repository = 4;
}

// AuditResult is how the action ended.
message AuditResult {
  // "ok", a Connect code such as "permission_denied", or for outcomes
  // "failed" or "cancelled".
  string code = 1;
  // The error message, at most 500 characters.
  string message = 2;
}

// AuditClient is what the caller's client sent.
message AuditClient {
  // The Forge-Client-Version header.
  string version = 1;
  // The X-Request-Id header.
  string request_id = 2;
}

message ListAuditEventsRequest {
  // Only events about this service ("services/payments" or "payments").
  string service = 1;
  // Only events by this actor: an email, a repository "<org>/<repo>", or an
  // actor kind such as "system".
  string actor = 2;
  // Only this action, for example "deploy.promote".
  string action = 3;
  // Only events whose result is not "ok".
  bool failed_only = 4;
  // Only events at or after since, and before until.
  google.protobuf.Timestamp since = 5;
  google.protobuf.Timestamp until = 6;
  // At most this many events; default 50, max 500.
  int32 page_size = 7;
  string page_token = 8;
}

message ListAuditEventsResponse {
  repeated AuditEvent events = 1;
  // Empty on the last page.
  string next_page_token = 2;
}

message GetAuditEventRequest {
  // "auditEvents/1234".
  string name = 1;
}

message GetAuditEventResponse {
  AuditEvent event = 1;
}
```

- [ ] **Step 2: Generate and lint**

Run: `make generate && buf lint && go build ./...`. Expected: clean; `gen/forge/v1/audit.pb.go` and `gen/forge/v1/forgev1connect/audit.connect.go` exist.

- [ ] **Step 3: Add the client**

In `pkg/client/client.go`, add the field `Audit forgev1connect.AuditServiceClient` to `Client` after `Deploys`, and set it in `New`: `Audit: forgev1connect.NewAuditServiceClient(hc, baseURL, opts...),`.

Run: `go build ./... && go test ./pkg/...`. Expected: ok.

- [ ] **Step 4: Commit**

```bash
git add proto/forge/v1/audit.proto gen pkg/client/client.go
git commit -m "proto: AuditService and AuditEvent"
```

---

### Task 2: The audit store (memory, PostgreSQL, migration 0002)

**Files:**
- Create: `internal/store/audit.go`, `internal/store/audit_memory.go`, `internal/store/audit_postgres.go`, `internal/store/migrations/0002_audit.sql`, `internal/store/audit_suite_test.go`, `internal/store/audit_test.go`
- Modify: `internal/store/memory.go` (two fields on `Memory`)

**Interfaces:**
- Consumes: `forgev1.AuditEvent` (Task 1); `Memory`, `Postgres`, `NewMemory`, `ErrNotFound`, `Store`, the `querier` interface in postgres.go, `pgtest.URL` (existing).
- Produces:

```go
type AuditQuery struct {
	Service    string    // "services/<id>"; "" = any
	Actor      string    // matches actor email, repository or kind; "" = any
	Action     string
	FailedOnly bool      // result code is not "ok"
	Since      time.Time // zero = no lower bound (inclusive)
	Until      time.Time // zero = no upper bound (exclusive)
	BeforeID   int64     // > 0: only ids below it (the page cursor)
	Limit      int       // > 0
}
type AuditStore interface {
	// AppendAuditEvent stores a copy of ev, assigning Name ("auditEvents/<id>")
	// and, when ev.Time is nil, the current time. It returns the stored event.
	AppendAuditEvent(ctx context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error)
	GetAuditEvent(ctx context.Context, id int64) (*forgev1.AuditEvent, error) // ErrNotFound
	ListAuditEvents(ctx context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error) // newest first
}
type Backend interface { Store; AuditStore }
func AuditEventName(id int64) string            // "auditEvents/<id>"
func ParseAuditEventName(name string) (int64, bool) // accepts "auditEvents/<id>" or "<id>"; id > 0
```
`*Memory` and `*Postgres` both implement `Backend`.

- [ ] **Step 1: Write `audit.go`**

```go
package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// AuditQuery selects audit events for ListAuditEvents.
type AuditQuery struct {
	Service    string    // "services/<id>"; "" means any
	Actor      string    // an actor's email, repository or kind; "" means any
	Action     string    // "" means any
	FailedOnly bool      // only events whose result code is not "ok"
	Since      time.Time // zero means no lower bound; inclusive
	Until      time.Time // zero means no upper bound; exclusive
	BeforeID   int64     // > 0: only events with a lower id (the page cursor)
	Limit      int       // at most this many; must be positive
}

// AuditStore keeps the audit log. Events are append-only: nothing changes or
// deletes one.
type AuditStore interface {
	// AppendAuditEvent stores a copy of ev, assigning its Name
	// ("auditEvents/<id>", ids increasing) and, when ev.Time is nil, the
	// current time. It returns the stored event.
	AppendAuditEvent(ctx context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error)
	// GetAuditEvent returns the event with id, or ErrNotFound.
	GetAuditEvent(ctx context.Context, id int64) (*forgev1.AuditEvent, error)
	// ListAuditEvents returns up to q.Limit matching events, newest first.
	ListAuditEvents(ctx context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error)
}

// Backend is everything forge-api keeps: the catalog and the audit log.
type Backend interface {
	Store
	AuditStore
}

const auditPrefix = "auditEvents/"

// AuditEventName is the resource name of the event with id.
func AuditEventName(id int64) string { return auditPrefix + strconv.FormatInt(id, 10) }

// ParseAuditEventName accepts "auditEvents/<id>" or "<id>" and returns a
// positive id.
func ParseAuditEventName(name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(name, auditPrefix), 10, 64)
	return id, err == nil && id > 0
}
```

- [ ] **Step 2: Write the shared suite `audit_suite_test.go`**

```go
package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runAuditSuite checks an AuditStore. open returns an empty store per subtest.
func runAuditSuite(t *testing.T, open func(t *testing.T) AuditStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	ev := func(min int, action, service, email, code string) *forgev1.AuditEvent {
		return &forgev1.AuditEvent{
			Time: timestamppb.New(at.Add(time.Duration(min) * time.Minute)), Kind: "request", Action: action,
			Actor:   &forgev1.AuditActor{Kind: "user", Subject: "sub-" + email, Email: email},
			Service: service, Target: service,
			Result:  &forgev1.AuditResult{Code: code},
			Details: map[string]string{"env": "staging"},
			Client:  &forgev1.AuditClient{Version: "0.3.0", RequestId: "req-1"},
		}
	}
	appendAll := func(t *testing.T, s AuditStore, evs ...*forgev1.AuditEvent) []*forgev1.AuditEvent {
		t.Helper()
		var out []*forgev1.AuditEvent
		for _, e := range evs {
			got, err := s.AppendAuditEvent(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, got)
		}
		return out
	}
	actions := func(evs []*forgev1.AuditEvent) []string {
		var out []string
		for _, e := range evs {
			out = append(out, e.GetAction())
		}
		return out
	}

	t.Run("append assigns increasing names and keeps fields", func(t *testing.T) {
		s := open(t)
		in := ev(0, "deploy.start", "services/payments", "ana@example.com", "ok")
		got := appendAll(t, s, in, ev(1, "deploy.promote", "services/payments", "ana@example.com", "ok"))
		a, okA := ParseAuditEventName(got[0].GetName())
		b, okB := ParseAuditEventName(got[1].GetName())
		if !okA || !okB || b <= a {
			t.Fatalf("names %q, %q: want increasing auditEvents/<id>", got[0].GetName(), got[1].GetName())
		}
		back, err := s.GetAuditEvent(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		in.Name = got[0].GetName()
		if !proto.Equal(back, in) {
			t.Fatalf("round trip:\n got %v\nwant %v", back, in)
		}
		if in.GetName() == "" || back == in {
			t.Fatal("the store must copy events in and out")
		}
	})

	t.Run("append sets a missing time", func(t *testing.T) {
		s := open(t)
		e := ev(0, "x", "", "", "ok")
		e.Time = nil
		got := appendAll(t, s, e)
		if got[0].GetTime() == nil || time.Since(got[0].GetTime().AsTime()) > time.Minute {
			t.Fatalf("time %v", got[0].GetTime())
		}
	})

	t.Run("get missing", func(t *testing.T) {
		s := open(t)
		if _, err := s.GetAuditEvent(ctx, 999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("list filters, newest first", func(t *testing.T) {
		s := open(t)
		appendAll(t, s,
			ev(0, "service.register", "services/a", "ana@example.com", "ok"),
			ev(1, "deploy.start", "services/a", "bo@example.com", "permission_denied"),
			ev(2, "deploy.start", "services/b", "ana@example.com", "ok"),
			&forgev1.AuditEvent{Time: timestamppb.New(at.Add(3 * time.Minute)), Kind: "request", Action: "deploy.start",
				Actor: &forgev1.AuditActor{Kind: "workload", Subject: "github:showmino/a", Repository: "showmino/a"},
				Service: "services/a", Result: &forgev1.AuditResult{Code: "ok"}},
			&forgev1.AuditEvent{Time: timestamppb.New(at.Add(4 * time.Minute)), Kind: "request", Action: "environment.expire",
				Actor: &forgev1.AuditActor{Kind: "system"}, Service: "services/b", Result: &forgev1.AuditResult{Code: "ok"}},
		)
		for _, tc := range []struct {
			name string
			q    AuditQuery
			want []string
		}{
			{"all", AuditQuery{Limit: 10}, []string{"environment.expire", "deploy.start", "deploy.start", "deploy.start", "service.register"}},
			{"limit", AuditQuery{Limit: 2}, []string{"environment.expire", "deploy.start"}},
			{"service", AuditQuery{Service: "services/b", Limit: 10}, []string{"environment.expire", "deploy.start"}},
			{"actor email", AuditQuery{Actor: "ana@example.com", Limit: 10}, []string{"deploy.start", "service.register"}},
			{"actor repository", AuditQuery{Actor: "showmino/a", Limit: 10}, []string{"deploy.start"}},
			{"actor kind", AuditQuery{Actor: "system", Limit: 10}, []string{"environment.expire"}},
			{"action", AuditQuery{Action: "service.register", Limit: 10}, []string{"service.register"}},
			{"failed only", AuditQuery{FailedOnly: true, Limit: 10}, []string{"deploy.start"}},
			{"since inclusive", AuditQuery{Since: at.Add(3 * time.Minute), Limit: 10}, []string{"environment.expire", "deploy.start"}},
			{"until exclusive", AuditQuery{Until: at.Add(1 * time.Minute), Limit: 10}, []string{"service.register"}},
		} {
			got, err := s.ListAuditEvents(ctx, tc.q)
			if err != nil || !slices.Equal(actions(got), tc.want) {
				t.Errorf("%s: %v %v, want %v", tc.name, actions(got), err, tc.want)
			}
		}
	})

	t.Run("paging is stable while events arrive", func(t *testing.T) {
		s := open(t)
		got := appendAll(t, s, ev(0, "a1", "", "", "ok"), ev(1, "a2", "", "", "ok"), ev(2, "a3", "", "", "ok"))
		first, _ := s.ListAuditEvents(ctx, AuditQuery{Limit: 2})
		if !slices.Equal(actions(first), []string{"a3", "a2"}) {
			t.Fatalf("first page %v", actions(first))
		}
		appendAll(t, s, ev(3, "a4", "", "", "ok")) // a new event before the next page
		last, _ := ParseAuditEventName(first[len(first)-1].GetName())
		next, _ := s.ListAuditEvents(ctx, AuditQuery{BeforeID: last, Limit: 2})
		if !slices.Equal(actions(next), []string{"a1"}) {
			t.Fatalf("next page %v, want [a1]", actions(next))
		}
		_ = got
	})
}
```

`audit_test.go`:

```go
package store

import (
	"context"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryAudit(t *testing.T) {
	t.Parallel()
	runAuditSuite(t, func(*testing.T) AuditStore { return NewMemory() })
}

func TestPostgresAudit(t *testing.T) {
	t.Parallel()
	runAuditSuite(t, func(t *testing.T) AuditStore {
		p := openTestPostgres(t, pgtest.URL(t))
		if err := p.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p
	})
}

func TestPostgresAuditAppendOnly(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	ctx := context.Background()
	if err := p.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AppendAuditEvent(ctx, &forgev1.AuditEvent{Kind: "request", Action: "x",
		Actor: &forgev1.AuditActor{Kind: "system"}, Result: &forgev1.AuditResult{Code: "ok"}}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"UPDATE audit_events SET action = 'y'",
		"DELETE FROM audit_events",
		"TRUNCATE audit_events",
	} {
		_, err := p.pool.Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want the append-only error", sql, err)
		}
	}
}

func TestParseAuditEventName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"auditEvents/12": 12, "12": 12, "auditEvents/0": 0, "auditEvents/-1": 0, "x": 0, "": 0} {
		got, ok := ParseAuditEventName(in)
		if (want > 0) != ok || (ok && got != want) {
			t.Errorf("ParseAuditEventName(%q) = %d %v, want %d", in, got, ok, want)
		}
	}
	if AuditEventName(7) != "auditEvents/7" {
		t.Error(AuditEventName(7))
	}
}
```

Add the import `forgev1 "github.com/showmino/forge/gen/forge/v1"` to `audit_test.go`.

Run: `go test ./internal/store/`. Expected: build failure (`AppendAuditEvent` undefined).

- [ ] **Step 3: Write the migration `0002_audit.sql`**

```sql
-- The audit log: one row per write call, outcome of long-running work, or
-- forge-api action. Append-only: the triggers below reject every change.
CREATE TABLE audit_events (
    id               bigserial PRIMARY KEY,
    time             timestamptz NOT NULL,
    kind             text NOT NULL,
    action           text NOT NULL,
    actor_kind       text NOT NULL,
    actor_subject    text NOT NULL DEFAULT '',
    actor_email      text NOT NULL DEFAULT '',
    actor_repository text NOT NULL DEFAULT '',
    service          text NOT NULL DEFAULT '',
    target           text NOT NULL DEFAULT '',
    result_code      text NOT NULL,
    result_message   text NOT NULL DEFAULT '',
    operation        text NOT NULL DEFAULT '',
    details          jsonb NOT NULL DEFAULT '{}',
    client_version   text NOT NULL DEFAULT '',
    request_id       text NOT NULL DEFAULT ''
);
CREATE INDEX audit_events_service ON audit_events (service, id);
CREATE INDEX audit_events_actor_email ON audit_events (actor_email, id);
CREATE INDEX audit_events_actor_repository ON audit_events (actor_repository, id);
CREATE INDEX audit_events_time ON audit_events (time);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END
$$;
CREATE TRIGGER audit_events_no_change BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
```

- [ ] **Step 4: Write the memory store `audit_memory.go`**

In `memory.go`, add two fields to `Memory`: `auditMu sync.Mutex` and `audit []*forgev1.AuditEvent` (index i holds id i+1). `NewMemory` needs no change: a nil slice is empty.

```go
package store

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func (m *Memory) AppendAuditEvent(_ context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error) {
	c := proto.Clone(ev).(*forgev1.AuditEvent)
	if c.Time == nil {
		c.Time = timestamppb.Now()
	}
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	c.Name = AuditEventName(int64(len(m.audit) + 1))
	m.audit = append(m.audit, c)
	return proto.Clone(c).(*forgev1.AuditEvent), nil
}

func (m *Memory) GetAuditEvent(_ context.Context, id int64) (*forgev1.AuditEvent, error) {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	if id < 1 || id > int64(len(m.audit)) {
		return nil, ErrNotFound
	}
	return proto.Clone(m.audit[id-1]).(*forgev1.AuditEvent), nil
}

func (m *Memory) ListAuditEvents(_ context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error) {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	var out []*forgev1.AuditEvent
	for i := len(m.audit) - 1; i >= 0 && len(out) < q.Limit; i-- {
		if q.BeforeID > 0 && int64(i+1) >= q.BeforeID {
			continue
		}
		if e := m.audit[i]; auditMatches(e, q) {
			out = append(out, proto.Clone(e).(*forgev1.AuditEvent))
		}
	}
	return out, nil
}

// auditMatches applies q's filters, other than paging, to e.
func auditMatches(e *forgev1.AuditEvent, q AuditQuery) bool {
	a := e.GetActor()
	t := e.GetTime().AsTime()
	switch {
	case q.Service != "" && e.GetService() != q.Service,
		q.Actor != "" && a.GetEmail() != q.Actor && a.GetRepository() != q.Actor && a.GetKind() != q.Actor,
		q.Action != "" && e.GetAction() != q.Action,
		q.FailedOnly && e.GetResult().GetCode() == "ok",
		!q.Since.IsZero() && t.Before(q.Since),
		!q.Until.IsZero() && !t.Before(q.Until):
		return false
	}
	return true
}

var _ = time.Time{} // keep the import if unused after edits; remove if lint flags it
```

Drop the `var _ = time.Time{}` line and the `time` import if nothing uses them. They are only a guard while editing.

- [ ] **Step 5: Write the Postgres store `audit_postgres.go`**

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

const auditColumns = `id, time, kind, action, actor_kind, actor_subject, actor_email, actor_repository,
	service, target, result_code, result_message, operation, details, client_version, request_id`

func (p *Postgres) AppendAuditEvent(ctx context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error) {
	t := time.Now()
	if ev.GetTime() != nil {
		t = ev.GetTime().AsTime()
	}
	details := ev.GetDetails()
	if details == nil {
		details = map[string]string{}
	}
	d, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	a, r, c := ev.GetActor(), ev.GetResult(), ev.GetClient()
	row := p.pool.QueryRow(ctx, `INSERT INTO audit_events (time, kind, action, actor_kind, actor_subject, actor_email,
			actor_repository, service, target, result_code, result_message, operation, details, client_version, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING `+auditColumns,
		t, ev.GetKind(), ev.GetAction(), a.GetKind(), a.GetSubject(), a.GetEmail(), a.GetRepository(),
		ev.GetService(), ev.GetTarget(), r.GetCode(), r.GetMessage(), ev.GetOperation(), d, c.GetVersion(), c.GetRequestId())
	return scanAuditEvent(row)
}

func (p *Postgres) GetAuditEvent(ctx context.Context, id int64) (*forgev1.AuditEvent, error) {
	e, err := scanAuditEvent(p.pool.QueryRow(ctx, "SELECT "+auditColumns+" FROM audit_events WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (p *Postgres) ListAuditEvents(ctx context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Service != "" {
		add("service = $%d", q.Service)
	}
	if q.Actor != "" {
		add("(actor_email = $%[1]d OR actor_repository = $%[1]d OR actor_kind = $%[1]d)", q.Actor)
	}
	if q.Action != "" {
		add("action = $%d", q.Action)
	}
	if q.FailedOnly {
		where = append(where, "result_code <> 'ok'")
	}
	if !q.Since.IsZero() {
		add("time >= $%d", q.Since)
	}
	if !q.Until.IsZero() {
		add("time < $%d", q.Until)
	}
	if q.BeforeID > 0 {
		add("id < $%d", q.BeforeID)
	}
	args = append(args, q.Limit)
	rows, err := p.pool.Query(ctx, fmt.Sprintf("SELECT %s FROM audit_events WHERE %s ORDER BY id DESC LIMIT $%d",
		auditColumns, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*forgev1.AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanAuditEvent(row pgx.Row) (*forgev1.AuditEvent, error) {
	var (
		id                                    int64
		t                                     time.Time
		kind, action, akind, asub, aemail     string
		arepo, service, target, code, message string
		operation, version, requestID         string
		details                               []byte
	)
	if err := row.Scan(&id, &t, &kind, &action, &akind, &asub, &aemail, &arepo,
		&service, &target, &code, &message, &operation, &details, &version, &requestID); err != nil {
		return nil, err
	}
	e := &forgev1.AuditEvent{
		Name: AuditEventName(id), Time: timestamppb.New(t), Kind: kind, Action: action,
		Actor:   &forgev1.AuditActor{Kind: akind, Subject: asub, Email: aemail, Repository: arepo},
		Service: service, Target: target,
		Result:    &forgev1.AuditResult{Code: code, Message: message},
		Operation: operation,
		Client:    &forgev1.AuditClient{Version: version, RequestId: requestID},
	}
	if err := json.Unmarshal(details, &e.Details); err != nil {
		return nil, fmt.Errorf("decode audit details: %w", err)
	}
	if len(e.Details) == 0 {
		e.Details = nil
	}
	return e, nil
}
```

The suite's round trip compares with `proto.Equal`, which treats a nil map and an empty map as equal. A missing actor, result or client becomes an empty message on read. The suite always sets them; if lint or the round trip disagrees, keep the code and adjust only the test's input.

Postgres `timestamptz` keeps microseconds. The suite's times are whole minutes, so the round trip is exact.

- [ ] **Step 6: Run the tests with and without the database**

Run: `go test -race -count=1 ./internal/store/`. Expected: PASS; the Postgres tests SKIP.

Run: `FORGE_TEST_DATABASE_URL=... go test -race -count=1 ./internal/store/... -run 'Audit' -v 2>&1 | tail -25`. Expected: all subtests of `TestMemoryAudit` and `TestPostgresAudit` PASS, plus `TestPostgresAuditAppendOnly` and `TestParseAuditEventName`.

Also re-run the whole store package with the database, to show that migration 0002 doesn't break `TestPostgresMigrate`. It counts rows in `forge_migrations`: update its expected count from 1 to 2, and its failure message.

- [ ] **Step 7: Lint and commit**

Run: `golangci-lint run ./... && go test -race ./...`. Expected: clean, PASS.

```bash
git add internal/store
git commit -m "store: append-only audit log (memory and PostgreSQL)"
```

---

### Task 3: Capture: the audit interceptor, actions, actors, completeness

**Files:**
- Create: `internal/server/audit.go`, `internal/server/audit_test.go`
- Modify:
  - `internal/server/server.go`: `Options.Audit`, the auditor, the interceptor order;
  - `internal/server/authn.go`: fill in the actor;
  - `internal/server/environments.go`: note the service in Extend and Destroy;
  - `internal/server/environments_test.go`: the fixture passes an audit store.

**Interfaces:**
- Consumes: `store.AuditStore`, `store.NewMemory` (Task 2); `forgev1.AuditEvent` and its parts, plus the `forgev1connect` procedure constants (Task 1); `Principal`, `Workload`, `servicePrefix`, `envNamePrefix`, `deployNamePrefix`, `labelService` (existing).
- Produces, in package `server`:

```go
// Options gains:
	// Audit stores the audit log; nil keeps it in memory.
	Audit store.AuditStore

type auditRecord struct{ ... }                       // the event a write call builds
func auditFrom(ctx context.Context) *auditRecord      // nil outside a write call
func withAuditRecord(ctx context.Context, r *auditRecord) context.Context
func newAuditRecord(action string) *auditRecord      // kind "request", no actor yet
func (r *auditRecord) setActor(p *Principal)
func (r *auditRecord) setSystem()                     // actor kind "system"
func noteAuditService(ctx context.Context, service string)   // sets service if empty and service != ""
func noteAuditOperation(ctx context.Context, name string)
func (r *auditRecord) snapshot() *forgev1.AuditEvent  // a copy of the event so far
func (r *auditRecord) describe(act auditAction, msg any) // service, target, details from the request; called before the handler
func (r *auditRecord) finish(h interface{ Get(string) string }, err error) *forgev1.AuditEvent
type auditor struct{ store store.AuditStore; log *slog.Logger; now func() time.Time }
func (a *auditor) append(ctx context.Context, ev *forgev1.AuditEvent) // nil-safe; logs on failure
var auditActions map[string]auditAction               // procedure -> action
var auditReads []string                               // procedures that are reads
func auditClip(s string) string                       // at most 500 characters on a rune boundary
func auditCode(err error) string                      // "ok" or the Connect code string
```

- [ ] **Step 1: Write the failing tests `audit_test.go`**

```go
package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

// auditAPI serves a catalog with an audit store; tokens: "writer" (ana, may
// write), "reader" (bo), "ci" (a workload of showmino/payments).
func auditAPI(t *testing.T, audit store.AuditStore, log *slog.Logger) forgev1connect.ServiceCatalogServiceClient {
	t.Helper()
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Email: "bo@example.com", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Audit:      audit,
		Logger:     log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
}

// allEvents returns the store's events, oldest first.
func allEvents(t *testing.T, s store.AuditStore) []*forgev1.AuditEvent {
	t.Helper()
	evs, err := s.ListAuditEvents(context.Background(), store.AuditQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

func TestAuditWriteCall(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	req := as("writer", &forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"}})
	req.Header().Set("Forge-Client-Version", "0.3.0")
	req.Header().Set("X-Request-Id", "req-42")
	if _, err := c.RegisterService(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListServices(context.Background(), as("writer", &forgev1.ListServicesRequest{})); err != nil {
		t.Fatal(err)
	}
	evs := allEvents(t, audit)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (reads are not audited): %v", len(evs), evs)
	}
	e := evs[0]
	if e.GetKind() != "request" || e.GetAction() != "service.register" || e.GetService() != "services/payments" ||
		e.GetTarget() != "services/payments" || e.GetResult().GetCode() != "ok" ||
		e.GetActor().GetKind() != "user" || e.GetActor().GetEmail() != "ana@example.com" || e.GetActor().GetSubject() != "ana" ||
		e.GetClient().GetVersion() != "0.3.0" || e.GetClient().GetRequestId() != "req-42" || e.GetTime() == nil {
		t.Fatalf("event: %v", e)
	}
}

func TestAuditRefusedCalls(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	ctx := context.Background()
	reg := &forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"}}
	_, _ = c.RegisterService(ctx, as("reader", reg))
	_, _ = c.RegisterService(ctx, connect.NewRequest(reg))         // no token
	_, _ = c.RegisterService(ctx, as("not-a-real-token-xyz", reg)) // bad token
	_, _ = c.RegisterService(ctx, as("writer", &forgev1.RegisterServiceRequest{ServiceId: "Bad Id", Service: &forgev1.Service{OwnerTeam: "t"}}))
	evs := allEvents(t, audit)
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	want := []struct{ actor, email, code string }{
		{"user", "bo@example.com", "permission_denied"},
		{"anonymous", "", "unauthenticated"},
		{"anonymous", "", "unauthenticated"},
		{"user", "ana@example.com", "invalid_argument"},
	}
	for i, w := range want {
		e := evs[i]
		if e.GetActor().GetKind() != w.actor || e.GetActor().GetEmail() != w.email || e.GetResult().GetCode() != w.code || e.GetResult().GetMessage() == "" {
			t.Errorf("event %d: %v, want actor %s %s code %s", i, e, w.actor, w.email, w.code)
		}
		if strings.Contains(e.String(), "not-a-real-token-xyz") {
			t.Errorf("event %d contains the token", i)
		}
	}
}

func TestAuditDetailsAllowlist(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	ctx := context.Background()
	_, _ = c.RegisterService(ctx, as("writer", &forgev1.RegisterServiceRequest{ServiceId: "payments", ValidateOnly: true,
		Service: &forgev1.Service{OwnerTeam: "billing", DisplayName: "secret-ish free text"}}))
	_, _ = c.UpdateService(ctx, as("writer", &forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "payments", DisplayName: "more free text"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "labels"}},
	}))
	evs := allEvents(t, audit)
	if got := evs[0].GetDetails(); len(got) != 1 || got["validate_only"] != "true" {
		t.Errorf("register details %v", got)
	}
	if got := evs[1].GetDetails(); len(got) != 1 || got["update_mask"] != "display_name,labels" || evs[1].GetService() != "services/payments" {
		t.Errorf("update: service %q details %v", evs[1].GetService(), got)
	}
	for _, e := range evs {
		if strings.Contains(e.String(), "free text") {
			t.Errorf("free text from the caller in %v", e)
		}
	}
}

func TestAuditStreamingCreate(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	stream, err := c.CreateService(context.Background(), as("writer", &forgev1.CreateServiceRequest{
		ServiceId: "payments", Template: "go-grpc", TemplateVersion: "v1.3.1", OwnerTeam: "billing",
		Parameters: map[string]string{"description": "free text"},
	}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	evs := allEvents(t, audit)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.GetAction() != "service.create" || e.GetService() != "services/payments" ||
		e.GetDetails()["template"] != "go-grpc" || e.GetDetails()["template_version"] != "v1.3.1" ||
		e.GetResult().GetCode() != connect.CodeOf(err).String() || strings.Contains(e.String(), "free text") {
		t.Fatalf("event %v (stream error %v)", e, err)
	}
}

// failingAudit refuses every append.
type failingAudit struct{ store.AuditStore }

func (failingAudit) AppendAuditEvent(context.Context, *forgev1.AuditEvent) (*forgev1.AuditEvent, error) {
	return nil, errors.New("database is down")
}

func TestAuditAppendFailureKeepsResult(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	c := auditAPI(t, failingAudit{store.NewMemory()}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if _, err := c.RegisterService(context.Background(), as("writer", &forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"},
	})); err != nil {
		t.Fatalf("the call must keep its own result: %v", err)
	}
	if !strings.Contains(logs.String(), "audit event not stored") || !strings.Contains(logs.String(), "service.register") ||
		!strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("logs: %s", logs.String())
	}
}

func TestAuditAppendSurvivesCancel(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	a := &auditor{store: audit, log: slog.New(slog.DiscardHandler), now: nil}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.append(ctx, &forgev1.AuditEvent{Kind: "request", Action: "x", Actor: &forgev1.AuditActor{Kind: "system"}, Result: &forgev1.AuditResult{Code: "ok"}})
	if n := len(allEvents(t, audit)); n != 1 {
		t.Fatalf("got %d events after a cancelled context, want 1", n)
	}
	var nilAuditor *auditor
	nilAuditor.append(context.Background(), &forgev1.AuditEvent{}) // must not panic
}

func TestAuditClip(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 600)
	got := auditClip(long)
	if n := len([]rune(got)); n != 500 || !strings.HasPrefix(long, got) {
		t.Fatalf("clipped to %d runes", n)
	}
	if auditClip("short") != "short" {
		t.Error("short text changed")
	}
}

func TestAuditCode(t *testing.T) {
	t.Parallel()
	if auditCode(nil) != "ok" || auditCode(connect.NewError(connect.CodePermissionDenied, errors.New("x"))) != "permission_denied" ||
		auditCode(errors.New("plain")) != "unknown" {
		t.Fatal("auditCode")
	}
}

// Every forge.v1 method is either an audited write or a listed read, not both.
func TestAuditCoversEveryProcedure(t *testing.T) {
	t.Parallel()
	reads := map[string]bool{}
	for _, p := range auditReads {
		reads[p] = true
	}
	seen := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("forge.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			sd := fd.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				proc := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(j).Name())
				_, write := auditActions[proc]
				switch {
				case write && reads[proc]:
					t.Errorf("%s is both an audited write and a read", proc)
				case !write && !reads[proc]:
					t.Errorf("%s is neither in auditActions nor in auditReads: classify it", proc)
				}
				seen++
			}
		}
		return true
	})
	if seen != len(auditActions)+len(auditReads) {
		t.Errorf("saw %d procedures, the tables list %d: a listed procedure does not exist", seen, len(auditActions)+len(auditReads))
	}
	_ = http.MethodGet
}
```

Remove the stray `_ = http.MethodGet` line and the `net/http` import if lint flags them; they're there in case a helper needs `http`.

In `environments_test.go`, extend `envFixture`:
- add a field `audit *store.Memory`;
- set `f.audit = store.NewMemory()` before `New(...)`;
- pass `Audit: f.audit` in the `New(Options{...})` call.

Then add to `environments_more_test.go`:

```go
func TestAuditEnvironmentService(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-7", Ref: "main", Ttl: durationpb.New(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, resp.GetOperation())
	ctx := context.Background()
	if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/pr-7", Ttl: durationpb.New(4 * time.Hour)})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/pr-7"})); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range allEvents(t, f.audit) {
		if e.GetKind() == "request" {
			got = append(got, e.GetAction()+" "+e.GetService()+" "+e.GetTarget()+" "+e.GetDetails()["ttl"])
		}
	}
	want := []string{
		"environment.create services/payments environments/pr-7 2h0m0s",
		"environment.extend services/payments environments/pr-7 4h0m0s",
		"environment.destroy services/payments environments/pr-7 ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
```

Check `f.create`'s existing signature: it returns `(*forgev1.CreateEnvironmentResponse, error)` and fills `Service` with `services/payments`. Use `resp.GetOperation()` as it returns it, and add imports (`durationpb`, `strings`, `time`) as needed. If the destroy's `ns` lookup fails in the fixture, use whichever path that fixture's destroy test already uses.

Run: `go test ./internal/server/`. Expected: build failure (`undefined: auditor`, `Options.Audit`).

- [ ] **Step 2: Write `audit.go`**

```go
package server

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/pkg/client"
)

// Audit event kinds and actor kinds.
const (
	auditRequest = "request"
	auditOutcome = "outcome"

	actorUser      = "user"
	actorWorkload  = "workload"
	actorSystem    = "system"
	actorAnonymous = "anonymous"
)

// auditMessageMax is the most characters of an error message an event keeps.
const auditMessageMax = 500

// auditRecord is the audit event a write call builds while it runs: the
// interceptor creates it, the auth interceptor adds the actor, handlers and
// Operations.Start add the service and operation.
type auditRecord struct {
	mu sync.Mutex
	ev *forgev1.AuditEvent
}

type auditKey struct{}

func newAuditRecord(action string) *auditRecord {
	return &auditRecord{ev: &forgev1.AuditEvent{Kind: auditRequest, Action: action, Actor: &forgev1.AuditActor{Kind: actorAnonymous}}}
}

func withAuditRecord(ctx context.Context, r *auditRecord) context.Context {
	return context.WithValue(ctx, auditKey{}, r)
}

// auditFrom returns the call's record, or nil outside an audited call.
func auditFrom(ctx context.Context) *auditRecord {
	r, _ := ctx.Value(auditKey{}).(*auditRecord)
	return r
}

func (r *auditRecord) setActor(p *Principal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := p.Workload; w != nil {
		r.ev.Actor = &forgev1.AuditActor{Kind: actorWorkload, Subject: p.Subject, Repository: w.Repository}
		return
	}
	r.ev.Actor = &forgev1.AuditActor{Kind: actorUser, Subject: p.Subject, Email: p.Email}
}

func (r *auditRecord) setSystem() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Actor = &forgev1.AuditActor{Kind: actorSystem}
}

// snapshot returns a copy of the event so far.
func (r *auditRecord) snapshot() *forgev1.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return proto.Clone(r.ev).(*forgev1.AuditEvent)
}

// noteAuditService records the service of the call, when the request did not
// name it (destroying an environment, cancelling an operation).
func noteAuditService(ctx context.Context, service string) {
	if r := auditFrom(ctx); r != nil && service != "" && service != servicePrefix {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ev.Service == "" {
			r.ev.Service = service
		}
	}
}

// noteAuditOperation records the operation the call started.
func noteAuditOperation(ctx context.Context, name string) {
	if r := auditFrom(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ev.Operation = name
	}
}

// describe fills service, target and details from the request message. The
// interceptor calls it before the handler runs, so an operation the handler
// starts copies them into its outcome template.
func (r *auditRecord) describe(act auditAction, msg any) {
	if msg == nil {
		return
	}
	service, target, det := act.describe(msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if service != "" {
		r.ev.Service = service
	}
	r.ev.Target, r.ev.Details = target, det
}

// finish sets the result and client and returns a copy of the event.
func (r *auditRecord) finish(h interface{ Get(string) string }, err error) *forgev1.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Result = &forgev1.AuditResult{Code: auditCode(err), Message: auditClip(errMessage(err))}
	if h != nil {
		r.ev.Client = &forgev1.AuditClient{Version: h.Get(client.HeaderClientVersion), RequestId: h.Get(client.HeaderRequestID)}
	}
	return proto.Clone(r.ev).(*forgev1.AuditEvent)
}

// auditCode is "ok" or the Connect code of err ("permission_denied", ...).
func auditCode(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

func errMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// auditClip keeps at most auditMessageMax characters, cut on a rune boundary.
func auditClip(s string) string {
	if utf8.RuneCountInString(s) <= auditMessageMax {
		return s
	}
	n := 0
	for i := range s {
		if n == auditMessageMax {
			return s[:i]
		}
		n++
	}
	return s
}

// auditor appends events to the store, falling back to the log.
type auditor struct {
	store store.AuditStore
	log   *slog.Logger
	now   func() time.Time
}

// append stores ev. It does not depend on ctx's cancellation, so a caller
// that went away is still audited; a failed append is logged in full.
func (a *auditor) append(ctx context.Context, ev *forgev1.AuditEvent) {
	if a == nil {
		return
	}
	if ev.Time == nil {
		now := time.Now
		if a.now != nil {
			now = a.now
		}
		ev.Time = timestamppb.New(now())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.store.AppendAuditEvent(ctx, ev); err != nil {
		b, _ := protojson.Marshal(ev)
		a.log.Error("audit event not stored", "error", err, "event", string(b))
	}
}

// auditAction names a write procedure and reads its request.
type auditAction struct {
	name     string
	describe func(msg any) (service, target string, details map[string]string)
}

// serviceName turns "payments" or "services/payments" into "services/payments".
func serviceName(s string) string {
	if s == "" || strings.HasPrefix(s, servicePrefix) {
		return s
	}
	return servicePrefix + s
}

// deploymentService is the service of "deployments/<service>/<env>".
func deploymentService(name string) string {
	svc, _, ok := strings.Cut(strings.TrimPrefix(name, deployNamePrefix), "/")
	if !ok || svc == "" {
		return ""
	}
	return servicePrefix + svc
}

// details keeps the non-empty values of kv (key, value, key, value, ...).
func details(kv ...string) map[string]string {
	var m map[string]string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			if m == nil {
				m = map[string]string{}
			}
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func flag(b bool) string {
	if b {
		return strconv.FormatBool(b)
	}
	return ""
}

func ttl(d interface{ AsDuration() time.Duration; IsValid() bool }) string {
	if d == nil || !d.IsValid() {
		return ""
	}
	return d.AsDuration().String()
}

// auditActions maps every write procedure to its action. The allowlisted
// details are identifiers and settings only, never free text from the caller.
var auditActions = map[string]auditAction{
	forgev1connect.ServiceCatalogServiceRegisterServiceProcedure: {"service.register", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.RegisterServiceRequest)
		s := serviceName(r.GetServiceId())
		return s, s, details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceUpdateServiceProcedure: {"service.update", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.UpdateServiceRequest)
		s := serviceName(r.GetService().GetName())
		return s, s, details("update_mask", strings.Join(r.GetUpdateMask().GetPaths(), ","), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceDeleteServiceProcedure: {"service.delete", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.DeleteServiceRequest)
		s := serviceName(r.GetName())
		return s, s, details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceCreateServiceProcedure: {"service.create", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.CreateServiceRequest)
		s := serviceName(r.GetServiceId())
		return s, s, details("template", r.GetTemplate(), "template_version", r.GetTemplateVersion(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.EnvironmentServiceCreateEnvironmentProcedure: {"environment.create", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.CreateEnvironmentRequest)
		return serviceName(r.GetService()), envNamePrefix + r.GetEnvironmentId(),
			details("ref", r.GetRef(), "ttl", ttl(r.GetTtl()), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.EnvironmentServiceExtendEnvironmentProcedure: {"environment.extend", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.ExtendEnvironmentRequest)
		return "", r.GetName(), details("ttl", ttl(r.GetTtl()))
	}},
	forgev1connect.EnvironmentServiceDestroyEnvironmentProcedure: {"environment.destroy", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.DestroyEnvironmentRequest)
		return "", r.GetName(), details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServiceStartDeployProcedure: {"deploy.start", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.StartDeployRequest)
		s := serviceName(r.GetService())
		return s, deployNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + r.GetEnv(),
			details("env", r.GetEnv(), "ref", r.GetRef(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServiceRollbackDeployProcedure: {"deploy.rollback", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.RollbackDeployRequest)
		return deploymentService(r.GetName()), r.GetName(), details("to", r.GetTo(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServicePromoteDeployProcedure: {"deploy.promote", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.PromoteDeployRequest)
		s := serviceName(r.GetService())
		to := r.GetTo()
		if to == "" {
			to = "production"
		}
		return s, deployNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + to,
			details("from", r.GetFrom(), "to", r.GetTo(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.OperationServiceCancelOperationProcedure: {"operation.cancel", func(m any) (string, string, map[string]string) {
		return "", m.(*forgev1.CancelOperationRequest).GetName(), nil
	}},
}

// auditReads lists every read procedure. A procedure in neither table fails
// TestAuditCoversEveryProcedure, so new RPCs are classified on purpose.
var auditReads = []string{
	forgev1connect.ServiceCatalogServiceListServicesProcedure,
	forgev1connect.ServiceCatalogServiceGetServiceProcedure,
	forgev1connect.AuthServiceGetServerInfoProcedure,
	forgev1connect.AuthServiceGetStatusProcedure,
	forgev1connect.TemplateServiceListTemplatesProcedure,
	forgev1connect.TemplateServiceGetTemplateProcedure,
	forgev1connect.OperationServiceGetOperationProcedure,
	forgev1connect.OperationServiceListOperationsProcedure,
	forgev1connect.OperationServiceWatchOperationProcedure,
	forgev1connect.EnvironmentServiceGetEnvironmentProcedure,
	forgev1connect.EnvironmentServiceListEnvironmentsProcedure,
	forgev1connect.DeployServiceGetDeploymentProcedure,
	forgev1connect.DeployServiceListDeploymentsProcedure,
	forgev1connect.DeployServiceListDeployHistoryProcedure,
	forgev1connect.AuditServiceListAuditEventsProcedure,
	forgev1connect.AuditServiceGetAuditEventProcedure,
}

// auditInterceptor records every write call; it runs outside authentication
// so refused calls are recorded too.
type auditInterceptor struct{ a *auditor }

func (i *auditInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		act, ok := auditActions[req.Spec().Procedure]
		if !ok {
			return next(ctx, req)
		}
		rec := newAuditRecord(act.name)
		rec.describe(act, req.Any())
		resp, err := next(withAuditRecord(ctx, rec), req)
		i.a.append(ctx, rec.finish(req.Header(), err))
		return resp, err
	}
}

func (i *auditInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *auditInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		act, ok := auditActions[conn.Spec().Procedure]
		if !ok {
			return next(ctx, conn)
		}
		rec := newAuditRecord(act.name)
		sc := &auditConn{StreamingHandlerConn: conn, rec: rec, act: act}
		err := next(withAuditRecord(ctx, rec), sc)
		i.a.append(ctx, rec.finish(conn.RequestHeader(), err))
		return err
	}
}

// auditConn describes the event from the first request message of a stream.
type auditConn struct {
	connect.StreamingHandlerConn
	rec  *auditRecord
	act  auditAction
	seen bool
}

func (c *auditConn) Receive(m any) error {
	err := c.StreamingHandlerConn.Receive(m)
	if err == nil && !c.seen {
		c.seen = true
		c.rec.describe(c.act, m)
	}
	return err
}
```

`ttl`'s parameter is an interface, so that `*durationpb.Duration` fits. If lint objects to the inline interface, import `google.golang.org/protobuf/types/known/durationpb` and take `*durationpb.Duration` instead. `durationpb.Duration` has `IsValid()`, and a nil pointer must return "". `flag` shadows no package here; if lint (predeclared or importas) complains, rename it `boolDetail`.

- [ ] **Step 3: Wire it in**

`server.go`:
- add to `Options`:

  ```go
  	// Audit stores the audit log; nil keeps it in memory.
  	Audit store.AuditStore
  ```
- in `New`, after the defaults, `if o.Audit == nil { o.Audit = store.NewMemory() }`;
- build `aud := &auditor{store: o.Audit, log: o.Logger}`;
- the interceptors become, in this order:

  ```go
  	opts := connect.WithInterceptors(
  		&logInterceptor{log: o.Logger},
  		&auditInterceptor{a: aud},
  		&authInterceptor{authn: o.Authenticator, authz: o.Authorizer},
  	)
  ```
- import `github.com/showmino/forge/internal/store`;
- keep `aud` in a variable: Task 4 passes it to Operations and Task 5 to AuditService.

`authn.go`, in `authInterceptor.authenticate`, right after `p` is known to be valid and before the workload/human branch:

```go
	if r := auditFrom(ctx); r != nil {
		r.setActor(p)
	}
```

`environments.go`:
- in `ExtendEnvironment`, right after the first successful `ns, err := e.o.Cluster.Env(ctx, id)`: `noteAuditService(ctx, servicePrefix+ns.Labels[labelService])`;
- in `DestroyEnvironment`, after the `switch` that sets `env`: `noteAuditService(ctx, env.GetService())`.

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/server/ -run 'Audit' -v 2>&1 | tail -30`. Expected: all PASS.

Then run `go test -race ./...`. Expected: PASS. Existing tests run with the default memory audit store and are unaffected.

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./...`. Expected: clean.

```bash
git add internal/server
git commit -m "server: audit every write call, refused ones included"
```

---

### Task 4: Outcomes of long-running work, system actions, cancel's service

**Files:**
- Modify:
  - `internal/server/operations.go`: an auditor, the audit template on each operation, the outcome event, `auditService`;
  - `internal/server/server.go`: give Operations the auditor;
  - `internal/server/operation_service.go`: the service on cancel;
  - `internal/server/env_destroy.go`: the reaper's system events.
- Test: `internal/server/operations_test.go`, `internal/server/environments_more_test.go`

**Interfaces:**
- Consumes (Task 3): `auditor`, `auditRecord`, `auditFrom`, `withAuditRecord`, `newAuditRecord`, `noteAuditOperation`, `noteAuditService`, `(*auditRecord).snapshot`, `setSystem`, `auditClip`, `auditOutcome`, `actorSystem`; the `envFixture.audit` field.
- Produces:

```go
func (s *Operations) setAuditor(a *auditor)            // called by server.New
func (s *Operations) appendAudit(ctx context.Context, ev *forgev1.AuditEvent)
func (s *Operations) auditService(name string) string  // the service of an operation, or ""
```

- [ ] **Step 1: Write the failing tests**

In `operations_test.go`:

```go
func TestOperationOutcomeAudited(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	ops := NewOperations(nil)
	ops.setAuditor(&auditor{store: audit, log: slog.New(slog.DiscardHandler)})

	rec := newAuditRecord("deploy.promote")
	rec.setActor(&Principal{Subject: "ana", Email: "ana@example.com"})
	ctx := withAuditRecord(context.Background(), rec)
	noteAuditService(ctx, "services/payments")
	ok := ops.Start(ctx, "promote", "deployments/payments/production", "ana", func(context.Context, func(string, string)) error { return nil })
	if rec.snapshot().GetOperation() != ok.GetName() {
		t.Fatalf("Start must name its operation in the call's event: %v", rec.snapshot())
	}
	failed := ops.Start(ctx, "promote", "deployments/payments/production", "ana", func(context.Context, func(string, string)) error {
		return &OpError{Code: "sync_timeout", Message: "argo cd did not sync"}
	})
	bare := ops.Start(context.Background(), "test_kind", "things/a", "", func(context.Context, func(string, string)) error { return nil })
	for _, op := range []*forgev1.Operation{ok, failed, bare} {
		if _, err := ops.Wait(context.Background(), op.GetName()); err != nil {
			t.Fatal(err)
		}
	}
	evs := waitEvents(t, audit, 3)
	byOp := map[string]*forgev1.AuditEvent{}
	for _, e := range evs {
		byOp[e.GetOperation()] = e
	}
	if e := byOp[ok.GetName()]; e.GetKind() != "outcome" || e.GetAction() != "deploy.promote" || e.GetService() != "services/payments" ||
		e.GetTarget() != "deployments/payments/production" || e.GetActor().GetEmail() != "ana@example.com" || e.GetResult().GetCode() != "ok" {
		t.Errorf("ok outcome: %v", e)
	}
	if e := byOp[failed.GetName()]; e.GetResult().GetCode() != "failed" || !strings.Contains(e.GetResult().GetMessage(), "argo cd did not sync") {
		t.Errorf("failed outcome: %v", e)
	}
	if e := byOp[bare.GetName()]; e.GetActor().GetKind() != "system" || e.GetAction() != "test_kind" {
		t.Errorf("an operation without an audit context: %v", e)
	}
}

// waitEvents polls until the store holds n events (outcomes are appended just
// after an operation is marked done) and returns them oldest first.
func waitEvents(t *testing.T, s store.AuditStore, n int) []*forgev1.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := allEvents(t, s)
		if len(evs) >= n || time.Now().After(deadline) {
			if len(evs) != n {
				t.Fatalf("got %d events, want %d: %v", len(evs), n, evs)
			}
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

`time.Sleep` in a test poll loop is fine. Add imports as needed: `log/slog`, `strings`, `time`, and `internal/store`.

Also in `operations_test.go`:

```go
func TestAuditCancelService(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	ops := NewOperations(nil)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}}},
		Authorizer:    Authorizer{WriterGroups: []string{"platform"}},
		Operations:    ops, Audit: audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rec := newAuditRecord("environment.create")
	ctx := withAuditRecord(context.Background(), rec)
	noteAuditService(ctx, "services/payments")
	block := make(chan struct{})
	op := ops.Start(ctx, "create_environment", "environments/pr-1", "ana", func(ctx context.Context, _ func(string, string)) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-block:
			return nil
		}
	})
	c := forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
	if _, err := c.CancelOperation(context.Background(), as("writer", &forgev1.CancelOperationRequest{Name: op.GetName()})); err != nil {
		t.Fatal(err)
	}
	evs := waitEvents(t, audit, 2) // the cancel request, then the operation's outcome
	var cancel, outcome *forgev1.AuditEvent
	for _, e := range evs {
		if e.GetAction() == "operation.cancel" {
			cancel = e
		} else {
			outcome = e
		}
	}
	if cancel.GetService() != "services/payments" || cancel.GetTarget() != op.GetName() {
		t.Errorf("cancel event: %v", cancel)
	}
	if outcome.GetResult().GetCode() != "cancelled" {
		t.Errorf("outcome: %v", outcome)
	}
	close(block)
}
```

In `environments_more_test.go` (the fixture already passes `f.audit` since Task 3):

```go
func TestReaperAuditsAsSystem(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-8", Ttl: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, resp.GetOperation())
	f.clock.add(2 * time.Hour)
	if err := f.envs.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	var expire *forgev1.AuditEvent
	deadline := time.Now().Add(5 * time.Second)
	for expire == nil && time.Now().Before(deadline) {
		for _, e := range allEvents(t, f.audit) {
			if e.GetAction() == "environment.expire" && e.GetKind() == "request" {
				expire = e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if expire == nil || expire.GetActor().GetKind() != "system" || expire.GetService() != "services/payments" ||
		expire.GetTarget() != "environments/pr-8" || expire.GetOperation() == "" || expire.GetResult().GetCode() != "ok" {
		t.Fatalf("expire event: %v", expire)
	}
	// Its destroy operation's outcome follows, also as the system.
	evs := waitOutcome(t, f.audit, expire.GetOperation())
	if evs.GetActor().GetKind() != "system" || evs.GetAction() != "environment.expire" {
		t.Fatalf("outcome: %v", evs)
	}
}

func waitOutcome(t *testing.T, s store.AuditStore, op string) *forgev1.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range allEvents(t, s) {
			if e.GetKind() == "outcome" && e.GetOperation() == op {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no outcome for %s", op)
	return nil
}
```

The env fixture uses a settable `clock` (`f.clock`, with `add`) as the `Now` of its options. If the method name differs, use the existing one. Add `"time"`, `durationpb` and the `store` import as needed.

Run: `go test ./internal/server/`. Expected: build failure (`ops.setAuditor` undefined).

- [ ] **Step 2: Implement in `operations.go`**

- Add the import `"sync/atomic"`. Add to `Operations` the field `audit atomic.Pointer[auditor]`, and to `operation` the field `auditEv *forgev1.AuditEvent` (the outcome template).
- Add:

```go
// setAuditor makes finished operations append their outcome to the audit log.
func (s *Operations) setAuditor(a *auditor) { s.audit.Store(a) }

// appendAudit appends ev when an auditor is set.
func (s *Operations) appendAudit(ctx context.Context, ev *forgev1.AuditEvent) { s.audit.Load().append(ctx, ev) }

// auditService is the service an operation's audit events name, or "".
func (s *Operations) auditService(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.ops[name]; ok && o.auditEv != nil {
		return o.auditEv.GetService()
	}
	return ""
}
```

- In `Start`, after building `o` and before locking, take the template from the call's audit record:

```go
	if r := auditFrom(ctx); r != nil {
		noteAuditOperation(ctx, o.pb.Name)
		o.auditEv = r.snapshot()
	} else {
		o.auditEv = &forgev1.AuditEvent{Action: kind, Actor: &forgev1.AuditActor{Kind: actorSystem}}
	}
	o.auditEv.Kind, o.auditEv.Operation = auditOutcome, o.pb.Name
	if o.auditEv.Target == "" {
		o.auditEv.Target = target
	}
	o.auditEv.Details, o.auditEv.Client, o.auditEv.Result, o.auditEv.Name, o.auditEv.Time = nil, nil, nil, "", nil
```

  Take the snapshot after `noteAuditOperation`: the request's record then holds the operation too, and the template is a separate copy.

- In `run`, after `s.finish(o, e)` (compute `e := opError(ctx, err)` first, and pass it), append the outcome outside the lock:

```go
	e := opError(ctx, err)
	s.finish(o, e)
	if o.auditEv != nil {
		ev := proto.Clone(o.auditEv).(*forgev1.AuditEvent)
		ev.Result = &forgev1.AuditResult{Code: "ok"}
		switch {
		case e == nil:
		case e.GetCode() == OpCodeCancelled:
			ev.Result = &forgev1.AuditResult{Code: "cancelled", Message: auditClip(e.GetMessage())}
		default:
			ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditClip(e.GetCode() + ": " + e.GetMessage())}
		}
		s.appendAudit(ctx, ev)
	}
```

  `ctx` here is the work's context, possibly cancelled; `append` detaches from cancellation. Watchers have their final message by then, so a test that waits for the operation and then reads the store must poll, as `waitEvents` does.

- In `server.go` `New`: after the defaults and the creation of `aud`, call `o.Operations.setAuditor(aud)`.

- [ ] **Step 3: Cancel's service and the reaper**

In `operation_service.go` `CancelOperation`, after `validOpName` succeeds and before `s.ops.Cancel(name)`:

```go
	noteAuditService(ctx, s.ops.auditService(name))
```

In `env_destroy.go` `Reap`, replace the body of the loop after `id := ns.Labels[labelEnvironment]` with:

```go
		rec := newAuditRecord("environment.expire")
		rec.setSystem()
		actx := withAuditRecord(context.Background(), rec)
		noteAuditService(actx, servicePrefix+ns.Labels[labelService])
		op := e.startDestroy(actx, id, "forge-reaper")
		if op == nil {
			continue
		}
		ev := rec.snapshot()
		ev.Target, ev.Operation = envNamePrefix+id, op.GetName()
		ev.Result = &forgev1.AuditResult{Code: "ok"}
		e.o.Operations.appendAudit(actx, ev)
		e.o.Log.Info("environment expired", "environment", id, "operation", op.GetName())
```

`startDestroy` may return a destroy that is already running, started by someone else. Then `Start` didn't run with this record, and `ev.Operation` names that destroy; that is accurate. The template only exists when `Start` ran with `actx`.

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/server/ -run 'Outcome|Cancel|Reaper|Audit' -v 2>&1 | tail -30`. Expected: PASS.

Then `go test -race -count=3 ./internal/server/`. Expected: PASS (`-count=3` shakes out timing).

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./... && go test -race ./...`. Expected: clean, PASS.

```bash
git add internal/server
git commit -m "server: audit outcomes of long-running work and the reaper's actions"
```

---

### Task 5: AuditService handlers and forge-api wiring

**Files:**
- Create: `internal/server/audit_service.go`, `internal/server/audit_service_test.go`
- Modify:
  - `internal/server/server.go`: register the handler;
  - `cmd/forge-api/store.go`: `openStore` returns `store.Backend`;
  - `cmd/forge-api/store_test.go`: the type assertion;
  - `cmd/forge-api/main.go`: `Audit: st`.

**Interfaces:**
- Consumes: `store.AuditStore`, `store.AuditQuery`, `store.ParseAuditEventName`, `store.ErrNotFound`, `store.Backend` (Task 2); `forgev1connect.NewAuditServiceHandler` (Task 1); `storeErr`, `require`, `PermServicesRead`, `invalid`, `serviceName` (existing or Task 3).
- Produces: `type auditService struct{ store store.AuditStore }`, implementing `forgev1connect.AuditServiceHandler`; `func openStore(ctx, url string, log *slog.Logger) (store.Backend, error)`.

- [ ] **Step 1: Write the failing tests `audit_service_test.go`**

```go
package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

func auditReader(t *testing.T, audit store.AuditStore) forgev1connect.AuditServiceClient {
	t.Helper()
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"reader": {Subject: "bo", Email: "bo@example.com", Groups: []string{"dev"}},
			"ci": {Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
				Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Audit:      audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return forgev1connect.NewAuditServiceClient(srv.Client(), srv.URL)
}

func seedAudit(t *testing.T, s store.AuditStore, n int) {
	t.Helper()
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for i := range n {
		svc := "services/a"
		if i%2 == 1 {
			svc = "services/b"
		}
		if _, err := s.AppendAuditEvent(context.Background(), &forgev1.AuditEvent{
			Time: timestamppb.New(at.Add(time.Duration(i) * time.Minute)), Kind: "request", Action: "deploy.start",
			Actor: &forgev1.AuditActor{Kind: "user", Email: "ana@example.com"}, Service: svc,
			Result: &forgev1.AuditResult{Code: "ok"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAuditEvents(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	seedAudit(t, audit, 5)
	c := auditReader(t, audit)
	ctx := context.Background()
	resp, err := c.ListAuditEvents(ctx, as("reader", &forgev1.ListAuditEventsRequest{Service: "b"}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.Msg.GetEvents()); n != 2 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("service b: %d events, token %q", n, resp.Msg.GetNextPageToken())
	}
	if _, err := c.ListAuditEvents(ctx, as("ci", &forgev1.ListAuditEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a CI workload must not read the audit log: %v", err)
	}
	if _, err := c.ListAuditEvents(ctx, as("reader", &forgev1.ListAuditEventsRequest{PageSize: -1})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("negative page size: %v", err)
	}
	if _, err := c.ListAuditEvents(ctx, as("reader", &forgev1.ListAuditEventsRequest{PageToken: "!!"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad token: %v", err)
	}
}

func TestListAuditEventsPaging(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	seedAudit(t, audit, 5)
	c := auditReader(t, audit)
	ctx := context.Background()
	first, err := c.ListAuditEvents(ctx, as("reader", &forgev1.ListAuditEventsRequest{PageSize: 2}))
	if err != nil || len(first.Msg.GetEvents()) != 2 || first.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page: %v %v", first, err)
	}
	seedAudit(t, audit, 1) // a new event arrives between pages
	var names []string
	for _, e := range first.Msg.GetEvents() {
		names = append(names, e.GetName())
	}
	token := first.Msg.GetNextPageToken()
	for token != "" {
		page, err := c.ListAuditEvents(ctx, as("reader", &forgev1.ListAuditEventsRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Msg.GetEvents() {
			names = append(names, e.GetName())
		}
		token = page.Msg.GetNextPageToken()
	}
	want := "auditEvents/5,auditEvents/4,auditEvents/3,auditEvents/2,auditEvents/1"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("pages: %s, want %s (no duplicates, no gaps)", got, want)
	}
}

func TestGetAuditEvent(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	seedAudit(t, audit, 1)
	c := auditReader(t, audit)
	ctx := context.Background()
	for _, name := range []string{"auditEvents/1", "1"} {
		got, err := c.GetAuditEvent(ctx, as("reader", &forgev1.GetAuditEventRequest{Name: name}))
		if err != nil || got.Msg.GetEvent().GetName() != "auditEvents/1" {
			t.Errorf("get %s: %v %v", name, got, err)
		}
	}
	if _, err := c.GetAuditEvent(ctx, as("reader", &forgev1.GetAuditEventRequest{Name: "auditEvents/99"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := c.GetAuditEvent(ctx, as("reader", &forgev1.GetAuditEventRequest{Name: "auditEvents/x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad name: %v", err)
	}
}
```

`envSHA` is a constant in the package's tests (deploys tests). If it isn't visible here, use any 40-hex string.

Run: `go test ./internal/server/`. Expected: build failure (`auditService` undefined), or Unimplemented failures.

- [ ] **Step 2: Write `audit_service.go`**

```go
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// auditService reads the audit log for every member (services.read).
type auditService struct{ store store.AuditStore }

func (s *auditService) ListAuditEvents(ctx context.Context, req *connect.Request[forgev1.ListAuditEventsRequest]) (*connect.Response[forgev1.ListAuditEventsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	var before int64
	if t := m.GetPageToken(); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err == nil {
			before, err = strconv.ParseInt(string(b), 10, 64)
		}
		if err != nil || before < 1 {
			return nil, invalid("page_token is malformed")
		}
	}
	q := store.AuditQuery{
		Service: serviceName(m.GetService()), Actor: m.GetActor(), Action: m.GetAction(),
		FailedOnly: m.GetFailedOnly(), BeforeID: before, Limit: size + 1,
	}
	if m.GetSince() != nil {
		q.Since = m.GetSince().AsTime()
	}
	if m.GetUntil() != nil {
		q.Until = m.GetUntil().AsTime()
	}
	evs, err := s.store.ListAuditEvents(ctx, q)
	if err != nil {
		return nil, storeErr(err)
	}
	resp := &forgev1.ListAuditEventsResponse{Events: evs}
	if len(evs) > size {
		resp.Events = evs[:size]
		id, _ := store.ParseAuditEventName(evs[size-1].GetName())
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
	}
	return connect.NewResponse(resp), nil
}

func (s *auditService) GetAuditEvent(ctx context.Context, req *connect.Request[forgev1.GetAuditEventRequest]) (*connect.Response[forgev1.GetAuditEventResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	id, ok := store.ParseAuditEventName(req.Msg.GetName())
	if !ok {
		return nil, invalid(fmt.Sprintf("name %q must look like auditEvents/{id}", req.Msg.GetName()))
	}
	ev, err := s.store.GetAuditEvent(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", store.AuditEventName(id)))
	}
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.GetAuditEventResponse{Event: ev}), nil
}
```

In `server.go`, register it next to the others: `mux.Handle(forgev1connect.NewAuditServiceHandler(&auditService{store: o.Audit}, opts))`.

- [ ] **Step 3: forge-api wiring**

In `cmd/forge-api/store.go`, `openStore` returns `(store.Backend, error)`. `store.NewMemory()` and `*store.Postgres` already satisfy it, so only the signature and doc comment change.

In `cmd/forge-api/store_test.go`, `TestOpenStoreMemory`'s assertion `s.(*store.Memory)` keeps working. `catalogClient(t, s store.Store)` accepts a Backend unchanged.

In `cmd/forge-api/main.go`, add `Audit: st,` to the `server.Options` literal.

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/server/ ./cmd/forge-api/`. Expected: PASS.

Run: `FORGE_TEST_DATABASE_URL=... go test -race -count=1 ./...`. Expected: PASS.

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./...`. Expected: clean.

```bash
git add internal/server cmd/forge-api
git commit -m "server: AuditService lists and gets audit events; forge-api stores them in PostgreSQL"
```

---

### Task 6: `forge audit list` and `forge audit get`

**Files:**
- Create: `internal/cli/audit.go`, `internal/cli/audit_test.go`, `internal/cli/testdata/audit_list_table.golden`, `internal/cli/testdata/audit_get_table.golden`
- Modify:
  - `internal/cli/root.go`: register `newAuditCmd(f)` in the command list;
  - `internal/cli/cli_test.go`: the harness gains an audit store.

**Interfaces:**
- Consumes: `client.Client.Audit` (Task 1); `store.NewMemory`, `store.AuditStore` (Task 2); `server.Options.Audit` (Task 3). From the CLI package: `group`, `noArgs`, `f.Format`, `f.Client`, `f.withTimeout`, `f.infof`, `f.Now`, `output.Print`, `output.View`, `exitcode.Usagef`, `golden`.
- Produces: the `audit` noun (`list`, `get`).

- [ ] **Step 1: Give the harness an audit store**

In `cli_test.go`:
- add a field `audit *store.Memory` to `harness`;
- in `newHarness`, set `audit: store.NewMemory()` in the struct literal;
- in `startAPI`, pass `Audit: h.audit`;
- import `github.com/showmino/forge/internal/store`.

- [ ] **Step 2: Write the failing tests `audit_test.go`**

```go
package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// seedAudit appends fixed events to the harness's audit store.
func (h *harness) seedAudit() {
	h.t.Helper()
	at := time.Date(2026, 10, 10, 9, 14, 3, 0, time.UTC)
	for _, e := range []*forgev1.AuditEvent{
		{Time: timestamppb.New(at), Kind: "request", Action: "deploy.start", Service: "services/payments",
			Target: "deployments/payments/staging", Operation: "operations/op_0123456789ab",
			Actor:  &forgev1.AuditActor{Kind: "workload", Subject: "github:showmino/payments", Repository: "showmino/payments"},
			Result: &forgev1.AuditResult{Code: "ok"}, Details: map[string]string{"env": "staging", "ref": "main"},
			Client: &forgev1.AuditClient{Version: "0.3.0", RequestId: "req-1"}},
		{Time: timestamppb.New(at.Add(time.Minute)), Kind: "outcome", Action: "deploy.start", Service: "services/payments",
			Target: "deployments/payments/staging", Operation: "operations/op_0123456789ab",
			Actor:  &forgev1.AuditActor{Kind: "workload", Subject: "github:showmino/payments", Repository: "showmino/payments"},
			Result: &forgev1.AuditResult{Code: "ok"}},
		{Time: timestamppb.New(at.Add(2 * time.Minute)), Kind: "request", Action: "deploy.promote", Service: "services/payments",
			Target: "deployments/payments/production",
			Actor:  &forgev1.AuditActor{Kind: "user", Subject: "bo", Email: "bo@example.com"},
			Result: &forgev1.AuditResult{Code: "permission_denied", Message: "bo lacks permission services.write"}},
		{Time: timestamppb.New(at.Add(3 * time.Minute)), Kind: "request", Action: "environment.expire", Service: "services/ledger",
			Target: "environments/pr-9", Actor: &forgev1.AuditActor{Kind: "system"}, Result: &forgev1.AuditResult{Code: "ok"}},
	} {
		if _, err := h.audit.AppendAuditEvent(context.Background(), e); err != nil {
			h.t.Fatal(err)
		}
	}
}

func TestAuditList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedAudit()
	r := h.ok("audit", "list")
	golden(t, "audit_list_table", r.out)

	if r := h.ok("audit", "list", "--service", "payments", "-o", "name"); r.out != "auditEvents/3\nauditEvents/2\nauditEvents/1\n" {
		t.Errorf("--service: %q", r.out)
	}
	if r := h.ok("audit", "list", "--failed", "-o", "name"); r.out != "auditEvents/3\n" {
		t.Errorf("--failed: %q", r.out)
	}
	if r := h.ok("audit", "list", "--actor", "showmino/payments", "--limit", "1", "-o", "name"); r.out != "auditEvents/2\n" {
		t.Errorf("--actor --limit: %q", r.out)
	}
	if r := h.ok("audit", "list", "--action", "environment.expire", "-o", "json"); !strings.Contains(r.out, `"kind": "system"`) {
		t.Errorf("json: %s", r.out)
	}
	if r := h.ok("audit", "list", "--since", "2026-10-10T09:16:00Z", "-o", "name"); r.out != "auditEvents/4\nauditEvents/3\n" {
		t.Errorf("--since RFC 3339: %q", r.out)
	}
	h.fails(2, "audit", "list", "--since", "yesterday")
	h.fails(2, "audit", "list", "--limit", "0")
}

func TestAuditListSinceDuration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedAudit() // all in 2026-10-10 09:14–09:17 UTC
	// 1000 days back covers them; 1h back from now does not (now is the real clock).
	if r := h.ok("audit", "list", "--since", "1000d", "-o", "name"); strings.Count(r.out, "\n") != 4 {
		t.Errorf("--since 1000d: %q", r.out)
	}
	if r := h.ok("audit", "list", "--since", "1h", "-o", "name"); strings.Contains(r.out, "auditEvents/") {
		t.Errorf("--since 1h: %q", r.out)
	}
}

func TestAuditListPagesUpToLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 3 {
		h.seedAudit() // 12 events
	}
	if r := h.ok("audit", "list", "--limit", "10", "-o", "name"); strings.Count(r.out, "\n") != 10 {
		t.Errorf("--limit 10: %q", r.out)
	}
}

func TestAuditListEmpty(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.ok("audit", "list")
	if r.out != "" || !strings.Contains(r.err, "No audit events.") {
		t.Errorf("empty: %v", r)
	}
	if r := h.ok("audit", "list", "-o", "json"); !strings.Contains(r.out, `"events": []`) {
		t.Errorf("empty json: %s", r.out)
	}
}

func TestAuditGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedAudit()
	r := h.ok("audit", "get", "1")
	golden(t, "audit_get_table", r.out)
	h.ok("audit", "get", "auditEvents/1")
	h.fails(2, "audit", "get", "abc")
	h.fails(3, "audit", "get", "99") // NotFound is exit 3
}
```

The listing tests also see events the harness's own writes produce (none here: these tests do no writes).

Run: `go test ./internal/cli/ -run Audit`. Expected: FAIL (unknown command "audit").

- [ ] **Step 3: Write `audit.go`**

```go
package cli

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
)

func newAuditCmd(f *Factory) *cobra.Command {
	cmd := group("audit", "Read the audit log", "audits")
	cmd.Long = `The audit log has one event per write call to the Forge API, refused calls
included, plus the outcome of long-running work (rows marked ↳) and forge-api's
own actions (actor "system"). Every member can read it; nothing can change it.`
	cmd.AddCommand(newAuditListCmd(f), newAuditGetCmd(f))
	return cmd
}

func newAuditListCmd(f *Factory) *cobra.Command {
	var service, actor, action, since, until string
	var failed bool
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List audit events, newest first",
		Example: `  forge audit list --service payments --since 7d
  forge audit list --actor ana@example.com --failed
  forge audit list --action deploy.promote -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			if limit < 1 {
				return exitcode.Usagef("--limit must be at least 1")
			}
			req := &forgev1.ListAuditEventsRequest{Service: service, Actor: actor, Action: action, FailedOnly: failed}
			now := f.Now()
			if req.Since, err = auditTime(since, now, "--since"); err != nil {
				return err
			}
			if req.Until, err = auditTime(until, now, "--until"); err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			var events []*forgev1.AuditEvent
			for len(events) < limit {
				req.PageSize = int32(min(limit-len(events), 500))
				resp, err := c.Audit.ListAuditEvents(ctx, connect.NewRequest(req))
				if err != nil {
					return err
				}
				events = append(events, resp.Msg.GetEvents()...)
				if req.PageToken = resp.Msg.GetNextPageToken(); req.PageToken == "" {
					break
				}
			}
			if len(events) == 0 && format == output.Table {
				f.infof("No audit events.")
				return nil
			}
			v := output.View{
				Headers: []string{"id", "time", "actor", "action", "service", "target", "result"},
				Data:    &forgev1.ListAuditEventsResponse{Events: events},
			}
			if len(events) == 0 {
				v.Data = map[string]any{"events": []any{}} // keep '.events[]' working in jq
			}
			for _, e := range events {
				act := e.GetAction()
				if e.GetKind() == "outcome" {
					act = "↳ " + act
				}
				v.Rows = append(v.Rows, []string{
					auditID(e.GetName()), e.GetTime().AsTime().UTC().Format("2006-01-02 15:04:05"), auditActor(e.GetActor()),
					act, strings.TrimPrefix(e.GetService(), "services/"), e.GetTarget(), e.GetResult().GetCode(),
				})
				v.Names = append(v.Names, e.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&service, "service", "", "only events about this service")
	fl.StringVar(&actor, "actor", "", "only events by this actor: an email, a repository <org>/<repo>, system or anonymous")
	fl.StringVar(&action, "action", "", "only this action, for example deploy.promote")
	fl.BoolVar(&failed, "failed", false, "only events whose result is not ok")
	fl.StringVar(&since, "since", "", "only events at or after this: a duration back from now (24h, 7d) or an RFC 3339 time")
	fl.StringVar(&until, "until", "", "only events before this: a duration back from now or an RFC 3339 time")
	fl.IntVar(&limit, "limit", 50, "most events to show")
	return cmd
}

func newAuditGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "get <id>",
		Short:   "Show one audit event",
		Example: "  forge audit get 1234\n  forge audit get auditEvents/1234 -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			id := strings.TrimPrefix(args[0], "auditEvents/")
			if n, err := strconv.ParseInt(id, 10, 64); err != nil || n < 1 {
				return exitcode.Usagef("%q is not an audit event id", args[0]).WithHint("see 'forge audit list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Audit.GetAuditEvent(ctx, connect.NewRequest(&forgev1.GetAuditEventRequest{Name: "auditEvents/" + id}))
			if err != nil {
				return err
			}
			e := resp.Msg.GetEvent()
			a := e.GetActor()
			var det []string
			for _, k := range slices.Sorted(maps.Keys(e.GetDetails())) {
				det = append(det, k+"="+e.GetDetails()[k])
			}
			rows := [][]string{
				{"name", e.GetName()},
				{"time", e.GetTime().AsTime().UTC().Format(time.RFC3339)},
				{"kind", e.GetKind()},
				{"action", e.GetAction()},
				{"actor", auditActor(a)},
				{"actor kind", a.GetKind()},
				{"subject", a.GetSubject()},
				{"service", e.GetService()},
				{"target", e.GetTarget()},
				{"result", e.GetResult().GetCode()},
				{"message", e.GetResult().GetMessage()},
				{"operation", e.GetOperation()},
				{"details", strings.Join(det, ", ")},
				{"client", e.GetClient().GetVersion()},
				{"request id", e.GetClient().GetRequestId()},
			}
			return output.Print(f.IO.Out, format, output.View{Headers: []string{"field", "value"}, Rows: rows, Names: []string{e.GetName()}, Data: e})
		},
	}
}

// auditTime reads a --since/--until value: "" (none), a duration back from
// now ("24h", "7d"), or an RFC 3339 time.
func auditTime(s string, now time.Time, flag string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return timestamppb.New(now.AddDate(0, 0, -n)), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return timestamppb.New(now.Add(-d)), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t), nil
	}
	return nil, exitcode.Usagef("%s %q must be a duration such as 24h or 7d, or an RFC 3339 time", flag, s)
}

// auditID is the number of "auditEvents/<id>".
func auditID(name string) string { return strings.TrimPrefix(name, "auditEvents/") }

// auditActor is who acted, as one word: an email, a repository, or the kind.
func auditActor(a *forgev1.AuditActor) string {
	switch {
	case a.GetEmail() != "":
		return a.GetEmail()
	case a.GetRepository() != "":
		return a.GetRepository()
	default:
		return a.GetKind()
	}
}

var _ = fmt.Sprint
```

- **Empty fields:** the table printer already shows `-` for empty cells, as `forge service get` does, so the get view passes raw values.
- **Leftovers:** remove `var _ = fmt.Sprint` and the `fmt` import if unused.
- **Group signature:** `group(name, short string, aliases ...string)` matches how `newOpCmd` calls it. Check that signature in `root.go` or wherever `group` lives, and follow it.
- **Register the noun:** in `root.go`, add `newAuditCmd(f)` to the slice that holds `newOpCmd(f)`.

- [ ] **Step 4: Create the goldens and run the tests**

Run: `go test ./internal/cli/ -run 'TestAuditList$|TestAuditGet' -update`, then `cat internal/cli/testdata/audit_list_table.golden internal/cli/testdata/audit_get_table.golden`. Check them by eye:
- the list is newest first;
- the outcome row shows `↳ deploy.start`;
- the actors read `showmino/payments`, `bo@example.com` and `system`;
- the get view shows details `env=staging, ref=main`.

Run: `go test -race -count=1 ./internal/cli/`. Expected: PASS.

- [ ] **Step 5: Lint and commit**

Run: `golangci-lint run ./... && go test -race ./...`. Expected: clean, PASS.

```bash
git add internal/cli
git commit -m "cli: forge audit list and get"
```

---

### Task 7: Docs

**Files:**
- Modify: `README.md`, `docs/index.md`, `docs/specs/2026-10-10-phase-5-m1-audit-design.md`, `.claude/skills/forge/SKILL.md`

- [ ] **Step 1: README**

After the deploy section of the CLI usage part (find the `forge deploy` paragraph), add:

```markdown
### Audit log

Every write call to the Forge API is recorded, refused calls included, with
who made it (a user's email, a CI repository, `system` for forge-api itself,
or `anonymous`), the service, the target and the result. Long-running work
adds a second row with its outcome. Every member can read the log; nothing
can change it.

    forge audit list --service payments --since 7d
    forge audit list --actor ana@example.com --failed
    forge audit get 1234

Events live in PostgreSQL (`forge-db`) when forge-api has `--database-url`;
otherwise in memory.
```

In the command table under "Using the CLI", add a row: `| `audit` | `list`, `get` |`.

- [ ] **Step 2: `docs/index.md` and the spec**

- **`docs/index.md`:** the line "Phases 1 to 4 are done; Phase 5 is next." becomes "Phases 1 to 4 are done. Phase 5 is in progress: milestone 1, the audit log, is done (`docs/specs/2026-10-10-phase-5-m1-audit-design.md`); milestone 2 brings resources and policy." Keep the sentence about the groundwork after it.
- **In the spec, section 4, exit gate step 4:**
  - "an `outcome` row for the create, the deploys and the environment operations" becomes "an `outcome` row for the deploys and the environment operations".
  - Then add one sentence: "`CreateService` streams its result instead of starting an operation, so its request row carries its final result."

- [ ] **Step 3: The skill**

In `.claude/skills/forge/SKILL.md`:
- **Roadmap:** in the "Later:" line, Phase 5 becomes "Phase 5: milestone 1, the audit log, done (spec docs/specs/2026-10-10-phase-5-m1-audit-design.md); milestone 2: resources (OpenTofu jobs) and policy".
- **Conventions table:** add a row: `| Every write RPC is in auditActions, every read in auditReads (TestAuditCoversEveryProcedure) | internal/server/audit.go |`.
- **"Adding an RPC" sentence:** append "; classify it in `auditActions` (a write, with its allowlisted details) or `auditReads`".

- [ ] **Step 4: Commit**

```bash
git add README.md docs .claude/skills/forge/SKILL.md
git commit -m "docs: the audit log"
```

---

### Task 8: Rollout and the exit gate on k3s (controller only, with the owner's OK)

No subagent runs this task. It runs after the final review, the merge to `main`, and the owner's go.

- [ ] **R1, image.** From a clean checkout of merged `main`: `docker build --build-arg VERSION=$(git describe --tags --always) --build-arg COMMIT=$(git rev-parse HEAD) -t ghcr.io/showmino/forge-api:<sha7> .`, then push it as a separate command.
- [ ] **R2, rollback file.** Save `kubectl -n forge get deploy/forge-api -o yaml` to the scratchpad.
- [ ] **R3, render, dry run, diff.**
  - Render with today's command:

    ```sh
    FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=18b12326d3df2c1a11b9f664f378b579eb472aaf FORGE_PREVIEW_IP=10.150.1.222 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_GITHUB_APP_ID=5239131 FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github FORGE_API_IMAGE=ghcr.io/showmino/forge-api:<sha7> deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221
    ```
  - Run `kubectl apply --dry-run=server`, then `kubectl diff`. The diff must show only forge-api's image.
- [ ] **R4, apply.**
  - Apply the render, then run `kubectl -n forge rollout status deploy/forge-api`.
  - In the logs: "service catalog in PostgreSQL", and no migration error.
  - Check that `kubectl -n forge exec forge-db-1 -c postgres -- psql -U postgres -d forge -tAc "select max(version) from forge_migrations"` prints 2.
- [ ] **R5, exit gate.**
  1. `forge create audit-gate-<mmddHHMM> --template go-grpc --team platform-engineering` (the owner is logged in through GitHub). Wait for its CI: test → image → deploy.
  2. Run:
     - `forge env create gate-pr --service audit-gate-<…>`, then `forge env destroy gate-pr --yes`;
     - `forge deploy promote audit-gate-<…> --yes`, then `forge deploy rollback audit-gate-<…> --env production --yes`, if a previous revision exists (otherwise a second staging deploy first);
     - `forge service update audit-gate-<…> --display-name "Audit gate"`.
  3. An unauthenticated write, sent as a plain HTTP POST with `python3 -I` urllib to `http://forge.10.150.1.221.nip.io/forge.v1.ServiceCatalogService/RegisterService`, with JSON `{"serviceId":"audit-gate-<…>"}` and no Authorization header. It returns 401.
  4. `forge audit list --service audit-gate-<…> --limit 200` must show:
     - `service.create` by the owner's email;
     - `deploy.start` by `showmino/audit-gate-<…>`, with its `↳` outcome;
     - `environment.create` and `environment.destroy`, each with its outcome;
     - `deploy.promote` and `deploy.rollback`, each with its outcome;
     - `service.update`;
     - `service.register` by `anonymous` with `unauthenticated`.

     `forge audit get <promote id>` shows details and the operation.
  5. Clean up as in the Phase 4 gate: the deploy-repo directory, the Applications, the namespaces, the catalog entry and the repo, with the owner's OK for deleting the repo.
- [ ] **R6, rollback (only if R4 or R5 fails).** Apply the saved forge-api Deployment. The `audit_events` table stays.
