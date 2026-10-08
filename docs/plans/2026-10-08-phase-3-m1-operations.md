# Phase 3 Milestone 1: Operations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Long-running work in forge-api becomes an `Operation`. Callers can get, list, watch (replay then live) and cancel operations, and the CLI gains `forge op list / watch / cancel`.

**Architecture:**
- **Store:** an in-memory `Operations` store in `internal/server/operations.go` runs each operation's work in its own goroutine, detached from the request. It records the work's events and outcome, and fans live events out to watchers through buffered channels; a watcher that falls behind is dropped.
- **API:** `OperationService` (proto `forge.v1`) exposes the store over Connect.
- **CLI:** a shared renderer in `internal/cli/operation.go` streams events to stderr and reattaches after a drop without repeating lines. Milestone 2's `forge env create/destroy` reuse it.
- **No real operation kind yet:** milestone 1 tests start operations directly through `Options.Operations`.

**Tech Stack:** Go 1.27, ConnectRPC (`connectrpc.com/connect`), buf v1.47.2, Cobra, `google.golang.org/protobuf` (`proto.Clone`, `timestamppb`).

**Spec:** `docs/specs/2026-10-08-phase-3-environments-design.md`, section 1 (Operations) and the error table in section 7.

## Global Constraints

- Operations live in memory. Finished operations are kept for 24 hours, then pruned; at most 1000 are kept, oldest finished first. Running operations are never pruned.
- Work runs detached from the request (`context.WithoutCancel` plus its own cancel). A dropped stream never stops it; only `CancelOperation` does.
- Watcher channels are buffered. A watcher that falls behind is closed with `ResourceExhausted`; it can reattach and gets the full replay.
- `WatchOperation` sends every past event, then live events, then the final `Operation`, and closes.
- `CancelOperation` is idempotent. Cancelling a finished operation is a no-op that returns it.
- Operation names are `operations/op_<12 hex>`. The CLI accepts the id with or without `operations/`.
- CLI exit codes:
  - `op watch`: 0 on success, 7 when the operation failed or was cancelled, 8 when `--timeout` expires. On a timeout it prints `still running; resume with: forge op watch <id>`.
  - Unknown operation: 3.
  - `op cancel`: refuses without `--yes` and without a terminal, with exit 2.
- Permissions:
  - Reading operations needs `services.read`.
  - Cancelling needs `services.write`. Phase 3 has no per-environment RBAC; these are the existing two permissions.
- Commits end with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- Gate before each commit that touches Go: `make build test lint`, with `PATH=$HOME/.local/go/bin:$PATH`. CI uses golangci-lint v2.14.0.

## Review Focus

1. **Reattach after `ResourceExhausted`:** the CLI must print each event exactly once, neither missing nor repeating any. Other server errors must not reattach. Covered by `TestOpWatchReattachPrintsEachEventOnce` (Task 4).
2. **Watching an operation that already finished,** including long after: it replays every event, prints the result and exits with the operation's code (0 or 7). Covered by `TestOpWatchFinished` (Task 4).
3. **`--timeout` during a watch:** exit 8 with the resume hint, while the operation keeps running on the server. Covered by `TestOpWatchTimeout` (Task 4).
4. **Work that panics:** the operation fails with code `internal`, watchers get the final message, and forge-api stays up. Covered by `TestOperationPanicFails` (Task 2).
5. **Cancel of a finished operation (exit 0, says it already finished) and of an unknown one (exit 3):** covered by `TestOpCancel` (Task 5).

---

### Task 0: Branch

- [x] **Step 1:** The spec is committed on `phase-3`. Branch from it:

```bash
cd /home/mino/code/forge && git checkout phase-3 && git checkout -b phase-3-m1 && git branch --show-current
```

Expected: `phase-3-m1`.

---

### Task 1: `operation.proto` and the client

**Files:**
- Create: `proto/forge/v1/operation.proto`
- Generated: `gen/forge/v1/operation.pb.go`, `gen/forge/v1/forgev1connect/operation.connect.go` (by `make generate`)
- Modify: `pkg/client/client.go` (add `Operations`)

**Interfaces:**
- Produces:
  - Go types `forgev1.Operation`, `forgev1.OperationEvent`, `forgev1.OperationError`.
  - `forgev1.WatchOperationResponse` with oneof `Kind`, whose wrappers are `WatchOperationResponse_Event` and `WatchOperationResponse_Operation`, and getters `GetEvent()` and `GetOperation()`.
  - Request and response pairs for Get, List, Watch and Cancel.
  - `forgev1connect.OperationServiceHandler` and `forgev1connect.NewOperationServiceHandler`.
  - `client.Client.Operations forgev1connect.OperationServiceClient`.

- [x] **Step 1: Write the proto**

```protobuf
syntax = "proto3";

package forge.v1;

import "google/protobuf/timestamp.proto";

// OperationService reports on long-running work, such as creating an
// environment, that an RPC started and returned at once. Operations live in
// forge-api's memory: a restart forgets them, not the work's results.
// Resource names follow AIP style: "operations/{operation}".
service OperationService {
  // GetOperation returns one operation with all its events.
  rpc GetOperation(GetOperationRequest) returns (GetOperationResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // ListOperations returns operations newest first, without their events.
  rpc ListOperations(ListOperationsRequest) returns (ListOperationsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }

  // WatchOperation sends every past event, then live events, then the final
  // operation, and closes. A watcher that falls behind gets RESOURCE_EXHAUSTED
  // and may watch again to replay from the start.
  rpc WatchOperation(WatchOperationRequest) returns (stream WatchOperationResponse) {}

  // CancelOperation asks a running operation to stop and returns it. It is a
  // no-op for a finished operation.
  rpc CancelOperation(CancelOperationRequest) returns (CancelOperationResponse) {
    option idempotency_level = IDEMPOTENT;
  }
}

// Operation is long-running work and its progress.
message Operation {
  // "operations/op_9f2c4a1b0c3d".
  string name = 1;
  // The resource the work is about, for example "environments/pr-142".
  string target = 2;
  // What the work does, for example "create_environment".
  string kind = 3;
  // Subject of the caller that started it.
  string creator = 4;
  google.protobuf.Timestamp create_time = 5;
  // Unset while running.
  google.protobuf.Timestamp end_time = 6;
  bool done = 7;
  // Set when done and the work failed or was cancelled.
  OperationError error = 8;
  // Empty in ListOperations.
  repeated OperationEvent events = 9;
}

// OperationEvent is one progress message.
message OperationEvent {
  google.protobuf.Timestamp time = 1;
  // The step that sent it, for example "resolve".
  string step = 2;
  string message = 3;
}

// OperationError says why an operation did not succeed.
message OperationError {
  // Stable reason, for example "cancelled", "internal", "image_timeout".
  string code = 1;
  string message = 2;
}

message GetOperationRequest {
  // "operations/{operation}".
  string name = 1;
}

message GetOperationResponse {
  Operation operation = 1;
}

message ListOperationsRequest {
  // Only operations on this resource, for example "environments/pr-142".
  string target = 1;
  // Only operations that are still running.
  bool running = 2;
}

message ListOperationsResponse {
  repeated Operation operations = 1;
}

message WatchOperationRequest {
  // "operations/{operation}".
  string name = 1;
}

message WatchOperationResponse {
  oneof kind {
    OperationEvent event = 1;
    // The finished operation; always the last message.
    Operation operation = 2;
  }
}

message CancelOperationRequest {
  // "operations/{operation}".
  string name = 1;
}

message CancelOperationResponse {
  Operation operation = 1;
}
```

- [x] **Step 2: Generate and lint**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && make generate && buf lint && go build ./...`
Expected: no output from `buf lint`; `gen/forge/v1/operation.pb.go` and `gen/forge/v1/forgev1connect/operation.connect.go` exist.

- [x] **Step 3: Add the client**

In `pkg/client/client.go`, add the field to `Client` after `Templates`:

```go
	Operations forgev1connect.OperationServiceClient
```

and in `New`'s return, after `Templates: ...`:

```go
		Operations: forgev1connect.NewOperationServiceClient(hc, baseURL, opts...),
```

- [x] **Step 4: Build**

Run: `go build ./... && go vet ./pkg/client`
Expected: success.

- [x] **Step 5: Commit**

```bash
git add proto/forge/v1/operation.proto gen pkg/client/client.go
git commit -m "proto: OperationService

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: The operations store

**Files:**
- Create: `internal/server/operations.go`
- Test: `internal/server/operations_test.go`

**Interfaces:**
- Consumes: Task 1's `forgev1.Operation`, `OperationEvent`, `OperationError`, `WatchOperationResponse`.
- Produces (milestone 2 relies on these exact names):
  - `type Work func(ctx context.Context, emit func(step, message string)) error`
  - `type OpError struct{ Code, Message string }`, with `func (e *OpError) Error() string`.
  - `func NewOperations(now func() time.Time) *Operations`
  - `func (s *Operations) Start(ctx context.Context, kind, target, creator string, w Work) *forgev1.Operation`
  - `func (s *Operations) Get(name string) (*forgev1.Operation, bool)`
  - `func (s *Operations) List(target string, runningOnly bool) []*forgev1.Operation`
  - `func (s *Operations) Watch(ctx context.Context, name string, send func(*forgev1.WatchOperationResponse) error) error`
  - `func (s *Operations) Cancel(name string) (*forgev1.Operation, bool)`
  - `func (s *Operations) Wait(ctx context.Context, name string) (*forgev1.Operation, error)`
  - Error codes: `OpCodeCancelled = "cancelled"`, `OpCodeInternal = "internal"`.

- [x] **Step 1: Write the failing tests**

`internal/server/operations_test.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func wait(t *testing.T, s *Operations, name string) *forgev1.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	op, err := s.Wait(ctx, name)
	if err != nil {
		t.Fatalf("wait %s: %v", name, err)
	}
	return op
}

// collect watches name until the stream ends and returns what it got.
func collect(ctx context.Context, s *Operations, name string) ([]*forgev1.WatchOperationResponse, error) {
	var got []*forgev1.WatchOperationResponse
	err := s.Watch(ctx, name, func(m *forgev1.WatchOperationResponse) error {
		got = append(got, m)
		return nil
	})
	return got, err
}

func messages(ms []*forgev1.WatchOperationResponse) string {
	var parts []string
	for _, m := range ms {
		if ev := m.GetEvent(); ev != nil {
			parts = append(parts, ev.GetStep()+":"+ev.GetMessage())
		} else if op := m.GetOperation(); op != nil {
			parts = append(parts, fmt.Sprintf("done(%s)", op.GetError().GetCode()))
		}
	}
	return strings.Join(parts, " ")
}

func TestOperationSucceeds(t *testing.T) {
	t.Parallel()
	c := newClock()
	s := NewOperations(c.now)
	op := s.Start(context.Background(), "test", "things/a", "ana", func(_ context.Context, emit func(string, string)) error {
		emit("one", "first")
		emit("two", "second")
		return nil
	})
	if !strings.HasPrefix(op.GetName(), "operations/op_") || len(op.GetName()) != len("operations/op_")+12 {
		t.Errorf("name %q", op.GetName())
	}
	if op.GetKind() != "test" || op.GetTarget() != "things/a" || op.GetCreator() != "ana" || op.GetCreateTime() == nil {
		t.Errorf("start: %v", op)
	}
	done := wait(t, s, op.GetName())
	if !done.GetDone() || done.GetError() != nil || done.GetEndTime() == nil || len(done.GetEvents()) != 2 {
		t.Errorf("done: %v", done)
	}
	if got, ok := s.Get(op.GetName()); !ok || len(got.GetEvents()) != 2 {
		t.Errorf("get: %v %v", got, ok)
	}
	if _, ok := s.Get("operations/op_nope"); ok {
		t.Error("get of an unknown operation")
	}
}

func TestOperationErrors(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	coded := s.Start(context.Background(), "test", "", "ana", func(context.Context, func(string, string)) error {
		return &OpError{Code: "image_timeout", Message: "no image after 10m"}
	})
	plain := s.Start(context.Background(), "test", "", "ana", func(context.Context, func(string, string)) error {
		return errors.New("boom")
	})
	if e := wait(t, s, coded.GetName()).GetError(); e.GetCode() != "image_timeout" || e.GetMessage() != "no image after 10m" {
		t.Errorf("coded: %v", e)
	}
	if e := wait(t, s, plain.GetName()).GetError(); e.GetCode() != OpCodeInternal || e.GetMessage() != "boom" {
		t.Errorf("plain: %v", e)
	}
}

func TestOperationPanicFails(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	op := s.Start(context.Background(), "test", "", "ana", func(_ context.Context, emit func(string, string)) error {
		emit("one", "before")
		panic("kaboom")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := collect(ctx, s, op.GetName())
	if err != nil {
		t.Fatal(err)
	}
	if m := messages(got); m != "one:before done(internal)" {
		t.Errorf("watch: %s", m)
	}
	if e := wait(t, s, op.GetName()).GetError(); !strings.Contains(e.GetMessage(), "kaboom") {
		t.Errorf("error: %v", e)
	}
}

func TestWatchReplaysThenLive(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	op := s.Start(context.Background(), "test", "", "ana", func(_ context.Context, emit func(string, string)) error {
		emit("one", "a")
		<-gate
		emit("two", "b")
		return nil
	})
	// Wait until "a" is recorded, so the watcher must replay it.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if got, _ := s.Get(op.GetName()); len(got.GetEvents()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first event never recorded")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []*forgev1.WatchOperationResponse
	errc := make(chan error, 1)
	go func() {
		errc <- s.Watch(ctx, op.GetName(), func(m *forgev1.WatchOperationResponse) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, m)
			if len(got) == 1 {
				gate <- struct{}{} // release the work only after the replay arrived
			}
			return nil
		})
	}()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if m := messages(got); m != "one:a two:b done()" {
		t.Errorf("watch: %s", m)
	}
}

func TestWatchFinishedAndUnknown(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	op := s.Start(context.Background(), "test", "", "ana", func(_ context.Context, emit func(string, string)) error {
		emit("one", "a")
		return nil
	})
	wait(t, s, op.GetName())
	got, err := collect(context.Background(), s, op.GetName())
	if err != nil || messages(got) != "one:a done()" {
		t.Errorf("finished: %s %v", messages(got), err)
	}
	if _, err := collect(context.Background(), s, "operations/op_nope"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown: %v", err)
	}
}

func TestManyWatchers(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	gate := make(chan struct{})
	op := s.Start(context.Background(), "test", "", "ana", func(_ context.Context, emit func(string, string)) error {
		<-gate
		for i := range 5 {
			emit("step", fmt.Sprint(i))
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range results {
		wg.Go(func() {
			got, err := collect(ctx, s, op.GetName())
			if err != nil {
				results[i] = err.Error()
				return
			}
			results[i] = messages(got)
		})
	}
	// Let the watchers subscribe before the work emits; a watcher that
	// subscribes late still gets everything through the replay.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	for i, r := range results {
		if r != "step:0 step:1 step:2 step:3 step:4 done()" {
			t.Errorf("watcher %d: %s", i, r)
		}
	}
}

func TestSlowWatcherDoesNotBlockWork(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	gate := make(chan struct{})
	op := s.Start(context.Background(), "test", "", "ana", func(_ context.Context, emit func(string, string)) error {
		<-gate
		for i := range 3 * watchBuffer {
			emit("step", fmt.Sprint(i))
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unblock := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- s.Watch(ctx, op.GetName(), func(*forgev1.WatchOperationResponse) error {
			<-unblock // a client that stopped reading
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond) // subscribed
	close(gate)
	wait(t, s, op.GetName()) // finishes although the watcher is stuck
	close(unblock)
	if err := <-errc; connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("slow watcher: %v", err)
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	started := make(chan struct{})
	op := s.Start(context.Background(), "test", "", "ana", func(ctx context.Context, _ func(string, string)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	if _, ok := s.Cancel(op.GetName()); !ok {
		t.Fatal("cancel")
	}
	if e := wait(t, s, op.GetName()).GetError(); e.GetCode() != OpCodeCancelled {
		t.Errorf("error: %v", e)
	}
	// A second cancel, and a cancel of a finished operation, change nothing.
	again, ok := s.Cancel(op.GetName())
	if !ok || again.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("again: %v", again)
	}
	ok2 := s.Start(context.Background(), "test", "", "ana", func(context.Context, func(string, string)) error { return nil })
	wait(t, s, ok2.GetName())
	if got, _ := s.Cancel(ok2.GetName()); got.GetError() != nil || !got.GetDone() {
		t.Errorf("cancel after success: %v", got)
	}
	if _, ok := s.Cancel("operations/op_nope"); ok {
		t.Error("cancel of an unknown operation")
	}
}

func TestWorkOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	s := NewOperations(nil)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	gate := make(chan struct{})
	op := s.Start(reqCtx, "test", "", "ana", func(ctx context.Context, _ func(string, string)) error {
		<-gate
		return ctx.Err()
	})
	cancelReq() // the client went away
	close(gate)
	if e := wait(t, s, op.GetName()).GetError(); e != nil {
		t.Errorf("work was stopped by the request: %v", e)
	}
}

func TestListAndPrune(t *testing.T) {
	t.Parallel()
	c := newClock()
	s := NewOperations(c.now)
	s.max = 3
	quick := func(target string) string {
		op := s.Start(context.Background(), "test", target, "ana", func(_ context.Context, emit func(string, string)) error {
			emit("s", "m")
			return nil
		})
		wait(t, s, op.GetName())
		c.add(time.Second)
		return op.GetName()
	}
	a := quick("things/a")
	b := quick("things/b")
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	running := s.Start(context.Background(), "test", "things/a", "ana", func(context.Context, func(string, string)) error {
		<-gate
		return nil
	})
	c.add(time.Second)

	all := s.List("", false)
	if len(all) != 3 || all[0].GetName() != running.GetName() || all[2].GetName() != a {
		t.Fatalf("list newest first: %v", all)
	}
	for _, op := range all {
		if len(op.GetEvents()) != 0 {
			t.Errorf("list must omit events: %v", op)
		}
	}
	if got := s.List("things/a", false); len(got) != 2 {
		t.Errorf("by target: %v", got)
	}
	if got := s.List("", true); len(got) != 1 || got[0].GetName() != running.GetName() {
		t.Errorf("running: %v", got)
	}

	// Over max: the oldest finished operation goes, never a running one.
	d := quick("things/d")
	if _, ok := s.Get(a); ok {
		t.Error("oldest finished operation kept over max")
	}
	if _, ok := s.Get(running.GetName()); !ok {
		t.Error("running operation pruned")
	}
	// After 24h every finished operation goes.
	c.add(25 * time.Hour)
	quick("things/e")
	for _, n := range []string{b, d} {
		if _, ok := s.Get(n); ok {
			t.Errorf("%s kept after 24h", n)
		}
	}
	if _, ok := s.Get(running.GetName()); !ok {
		t.Error("running operation pruned after 24h")
	}
}
```

- [x] **Step 2: Run them to see them fail**

Run: `go test ./internal/server -run 'TestOperation|TestWatch|TestManyWatchers|TestSlowWatcher|TestCancel|TestWorkOutlives|TestListAndPrune'`
Expected: build failure, `undefined: NewOperations`.

- [x] **Step 3: Implement the store**

`internal/server/operations.go`:

```go
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// Operation error codes every kind of work may end with.
const (
	OpCodeCancelled = "cancelled"
	OpCodeInternal  = "internal"
)

// watchBuffer is how many messages a watcher may fall behind before it is dropped.
const watchBuffer = 64

// Work is the body of an operation. It reports progress with emit and returns
// nil on success. An *OpError keeps its code; any other error becomes
// "internal", or "cancelled" when the operation was cancelled.
type Work func(ctx context.Context, emit func(step, message string)) error

// OpError is a failure with a stable code for the operation's error.
type OpError struct{ Code, Message string }

func (e *OpError) Error() string { return e.Message }

// Operations runs long work in the background and keeps, in memory, each
// operation's events and outcome for the watchers that stream them.
type Operations struct {
	now  func() time.Time
	keep time.Duration // finished operations older than this are pruned
	max  int           // most operations kept; running ones are never pruned

	mu  sync.Mutex
	ops map[string]*operation
}

type operation struct {
	pb     *forgev1.Operation // guarded by Operations.mu
	cancel context.CancelFunc
	done   chan struct{}
	subs   map[*watcher]struct{} // guarded by Operations.mu
}

// watcher receives live messages. The publisher closes ch when the operation
// ends, after the final message, or when the watcher fell behind.
type watcher struct{ ch chan *forgev1.WatchOperationResponse }

// NewOperations returns an empty store. now defaults to time.Now.
func NewOperations(now func() time.Time) *Operations {
	if now == nil {
		now = time.Now
	}
	return &Operations{now: now, keep: 24 * time.Hour, max: 1000, ops: map[string]*operation{}}
}

// Start runs w in its own goroutine and returns the new operation. The work
// keeps ctx's values but not its cancellation: only Cancel stops it.
func (s *Operations) Start(ctx context.Context, kind, target, creator string, w Work) *forgev1.Operation {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o := &operation{
		pb: &forgev1.Operation{
			Name: "operations/" + newOpID(), Target: target, Kind: kind, Creator: creator,
			CreateTime: timestamppb.New(s.now()),
		},
		cancel: cancel,
		done:   make(chan struct{}),
		subs:   map[*watcher]struct{}{},
	}
	s.mu.Lock()
	s.ops[o.pb.Name] = o
	s.prune()
	snap := proto.Clone(o.pb).(*forgev1.Operation)
	s.mu.Unlock()
	go s.run(ctx, o, w)
	return snap
}

func newOpID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "op_" + hex.EncodeToString(b)
}

func (s *Operations) run(ctx context.Context, o *operation, w Work) {
	defer o.cancel()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return w(ctx, func(step, message string) { s.emit(o, step, message) })
	}()
	s.finish(o, opError(ctx, err))
}

// opError turns the work's result into the operation's error.
func opError(ctx context.Context, err error) *forgev1.OperationError {
	if err == nil {
		return nil
	}
	var oe *OpError
	if errors.As(err, &oe) {
		return &forgev1.OperationError{Code: oe.Code, Message: oe.Message}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return &forgev1.OperationError{Code: OpCodeCancelled, Message: "the operation was cancelled"}
	}
	return &forgev1.OperationError{Code: OpCodeInternal, Message: err.Error()}
}

func (s *Operations) emit(o *operation, step, message string) {
	ev := &forgev1.OperationEvent{Time: timestamppb.New(s.now()), Step: step, Message: message}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.pb.Done {
		return
	}
	o.pb.Events = append(o.pb.Events, ev)
	msg := &forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Event{Event: ev}}
	for w := range o.subs {
		select {
		case w.ch <- msg:
		default: // fell behind: drop it; it can watch again and replay
			delete(o.subs, w)
			close(w.ch)
		}
	}
}

func (s *Operations) finish(o *operation, e *forgev1.OperationError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o.pb.Done = true
	o.pb.EndTime = timestamppb.New(s.now())
	o.pb.Error = e
	msg := &forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Operation{
		Operation: proto.Clone(o.pb).(*forgev1.Operation),
	}}
	for w := range o.subs {
		select {
		case w.ch <- msg:
		default:
		}
		delete(o.subs, w)
		close(w.ch)
	}
	close(o.done)
}

// prune drops finished operations older than keep, then the oldest finished
// ones while there are more than max. s.mu must be held.
func (s *Operations) prune() {
	cutoff := s.now().Add(-s.keep)
	var finished []*operation
	for name, o := range s.ops {
		if !o.pb.Done {
			continue
		}
		if o.pb.EndTime.AsTime().Before(cutoff) {
			delete(s.ops, name)
			continue
		}
		finished = append(finished, o)
	}
	if len(s.ops) <= s.max {
		return
	}
	slices.SortFunc(finished, func(a, b *operation) int {
		return a.pb.CreateTime.AsTime().Compare(b.pb.CreateTime.AsTime())
	})
	for _, o := range finished {
		if len(s.ops) <= s.max {
			return
		}
		delete(s.ops, o.pb.Name)
	}
}

// Get returns a copy of the operation with its events.
func (s *Operations) Get(name string) (*forgev1.Operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.ops[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(o.pb).(*forgev1.Operation), true
}

// List returns operations newest first, without events, optionally only those
// on target or only running ones.
func (s *Operations) List(target string, runningOnly bool) []*forgev1.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	var out []*forgev1.Operation
	for _, o := range s.ops {
		if (target != "" && o.pb.Target != target) || (runningOnly && o.pb.Done) {
			continue
		}
		c := proto.Clone(o.pb).(*forgev1.Operation)
		c.Events = nil
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *forgev1.Operation) int {
		if c := b.CreateTime.AsTime().Compare(a.CreateTime.AsTime()); c != 0 {
			return c
		}
		return strings.Compare(b.GetName(), a.GetName()) // stable for equal times
	})
	return out
}
```

Add `"strings"` to the imports. Continue in the same file:

```go
// Watch sends the operation's past events, then live ones, then the final
// operation. A watcher that falls behind ends with RESOURCE_EXHAUSTED.
func (s *Operations) Watch(ctx context.Context, name string, send func(*forgev1.WatchOperationResponse) error) error {
	s.mu.Lock()
	o, ok := s.ops[name]
	if !ok {
		s.mu.Unlock()
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
	}
	past := slices.Clone(o.pb.Events)
	var w *watcher
	var final *forgev1.Operation
	if o.pb.Done {
		final = proto.Clone(o.pb).(*forgev1.Operation)
	} else {
		w = &watcher{ch: make(chan *forgev1.WatchOperationResponse, watchBuffer)}
		o.subs[w] = struct{}{}
	}
	s.mu.Unlock()
	if w != nil {
		defer s.unsubscribe(o, w)
	}

	for _, ev := range past {
		if err := send(&forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Event{Event: ev}}); err != nil {
			return err
		}
	}
	if final != nil {
		return send(&forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Operation{Operation: final}})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-w.ch:
			if !ok {
				return connect.NewError(connect.CodeResourceExhausted,
					fmt.Errorf("the watch of %s fell behind; watch again to replay it", name))
			}
			if err := send(m); err != nil {
				return err
			}
			if m.GetOperation() != nil {
				return nil
			}
		}
	}
}

func (s *Operations) unsubscribe(o *operation, w *watcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(o.subs, w) // the publisher closes ch only while w is subscribed
}

// Cancel asks a running operation to stop and returns its current state. It
// changes nothing for a finished operation.
func (s *Operations) Cancel(name string) (*forgev1.Operation, bool) {
	s.mu.Lock()
	o, ok := s.ops[name]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	o.cancel()
	return s.Get(name)
}

// Wait blocks until the operation is done or ctx ends, and returns it.
func (s *Operations) Wait(ctx context.Context, name string) (*forgev1.Operation, error) {
	s.mu.Lock()
	o, ok := s.ops[name]
	s.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
	}
	select {
	case <-o.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	op, ok := s.Get(name)
	if !ok { // pruned between done and now
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
	}
	return op, nil
}
```

**Two invariants to keep:**
- **Snapshot and subscribe happen under one lock.** In `Watch`, copying `past` and adding `w` to `o.subs` must happen under the same `s.mu` hold. Then every event is either in `past` or delivered on `w.ch`, never both and never neither.
- **Only the publisher closes `w.ch`.** `emit` and `finish` close a watcher's channel only while removing it from `o.subs` under `s.mu`, and `unsubscribe` only removes it. So a channel is never closed twice.

- [x] **Step 4: Run the tests**

Run: `go test -race -count=3 ./internal/server -run 'TestOperation|TestWatch|TestManyWatchers|TestSlowWatcher|TestCancel|TestWorkOutlives|TestListAndPrune'`
Expected: PASS three times, no race reports.

- [x] **Step 5: Lint and commit**

```bash
make build test lint
git add internal/server/operations.go internal/server/operations_test.go
git commit -m "server: in-memory operations with replayed watches and cancel

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: `OperationService` handler and wiring

**Files:**
- Create: `internal/server/operation_service.go`
- Modify: `internal/server/server.go` (`Options.Operations`, handler registration)
- Test: `internal/server/operation_service_test.go`

**Interfaces:**
- Consumes: Task 2's `*Operations` (`Get`, `List`, `Watch`, `Cancel`), and `require`, `PermServicesRead` and `PermServicesWrite` from `authn.go`.
- Produces:
  - `Options.Operations *Operations`. Nil means `New` creates one with `NewOperations(nil)`. Milestone 2 passes the same store to the environment service, and CLI tests pass their own to start operations.
  - `func validOpName(name string) error`.

- [x] **Step 1: Write the failing test**

`internal/server/operation_service_test.go`:

```go
package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
)

func opServer(t *testing.T) (*Operations, forgev1connect.OperationServiceClient) {
	t.Helper()
	ops := NewOperations(nil)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Operations: ops,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return ops, forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
}

func as[T any](token string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+token)
	return r
}

func TestOperationServiceRPCs(t *testing.T) {
	t.Parallel()
	ops, c := opServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gate := make(chan struct{})
	op := ops.Start(ctx, "test", "things/a", "ana", func(ctx context.Context, emit func(string, string)) error {
		emit("one", "a")
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	got, err := c.GetOperation(ctx, as("reader", &forgev1.GetOperationRequest{Name: op.GetName()}))
	if err != nil || got.Msg.GetOperation().GetKind() != "test" {
		t.Fatalf("get: %v %v", got, err)
	}
	list, err := c.ListOperations(ctx, as("reader", &forgev1.ListOperationsRequest{Running: true}))
	if err != nil || len(list.Msg.GetOperations()) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"operations/op_000000000000", connect.CodeNotFound},
		{"op_123", connect.CodeInvalidArgument},
		{"", connect.CodeInvalidArgument},
	} {
		_, err := c.GetOperation(ctx, as("reader", &forgev1.GetOperationRequest{Name: tc.name}))
		if connect.CodeOf(err) != tc.code {
			t.Errorf("get %q: %v, want %v", tc.name, err, tc.code)
		}
	}

	if _, err := c.CancelOperation(ctx, as("reader", &forgev1.CancelOperationRequest{Name: op.GetName()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader cancel: %v", err)
	}
	if _, err := c.GetOperation(ctx, connect.NewRequest(&forgev1.GetOperationRequest{Name: op.GetName()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}

	stream, err := c.WatchOperation(ctx, as("reader", &forgev1.WatchOperationRequest{Name: op.GetName()}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetEvent().GetMessage() != "a" {
		t.Fatalf("first watch message: %v %v", stream.Msg(), stream.Err())
	}
	if _, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: op.GetName()})); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var final *forgev1.Operation
	for stream.Receive() {
		final = stream.Msg().GetOperation()
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if final.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("final: %v", final)
	}
	again, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: op.GetName()}))
	if err != nil || again.Msg.GetOperation().GetError().GetCode() != OpCodeCancelled {
		t.Errorf("cancel again: %v %v", again, err)
	}
	if _, err := c.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: "operations/op_000000000000"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("cancel unknown: %v", err)
	}
}
```

- [x] **Step 2: Run it to see it fail**

Run: `go test ./internal/server -run TestOperationServiceRPCs`
Expected: build failure, `unknown field Operations in struct literal of type Options`.

- [x] **Step 3: Implement the handler**

`internal/server/operation_service.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

var opNameRE = regexp.MustCompile(`^operations/op_[0-9a-f]{12}$`)

// validOpName checks an "operations/op_<12 hex>" name.
func validOpName(name string) error {
	if !opNameRE.MatchString(name) {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not an operation name (operations/op_<id>)", name))
	}
	return nil
}

func opNotFound(name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
}

type operationService struct{ ops *Operations }

func (s *operationService) GetOperation(ctx context.Context, req *connect.Request[forgev1.GetOperationRequest]) (*connect.Response[forgev1.GetOperationResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return nil, err
	}
	op, ok := s.ops.Get(name)
	if !ok {
		return nil, opNotFound(name)
	}
	return connect.NewResponse(&forgev1.GetOperationResponse{Operation: op}), nil
}

func (s *operationService) ListOperations(ctx context.Context, req *connect.Request[forgev1.ListOperationsRequest]) (*connect.Response[forgev1.ListOperationsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListOperationsResponse{
		Operations: s.ops.List(req.Msg.GetTarget(), req.Msg.GetRunning()),
	}), nil
}

func (s *operationService) WatchOperation(ctx context.Context, req *connect.Request[forgev1.WatchOperationRequest], stream *connect.ServerStream[forgev1.WatchOperationResponse]) error {
	if err := require(ctx, PermServicesRead); err != nil {
		return err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return err
	}
	err := s.ops.Watch(ctx, name, stream.Send)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // the client went away; nothing to report to it
	}
	return err
}

func (s *operationService) CancelOperation(ctx context.Context, req *connect.Request[forgev1.CancelOperationRequest]) (*connect.Response[forgev1.CancelOperationResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := validOpName(name); err != nil {
		return nil, err
	}
	op, ok := s.ops.Cancel(name)
	if !ok {
		return nil, opNotFound(name)
	}
	return connect.NewResponse(&forgev1.CancelOperationResponse{Operation: op}), nil
}
```

In `internal/server/server.go`:
- Add to `Options`, after `GitHubOrg`:

```go
	// Operations holds long-running work; nil creates an empty store.
	Operations *Operations
```

- In `New`, after the `o.Catalog` default:

```go
	if o.Operations == nil {
		o.Operations = NewOperations(nil)
	}
```

- After the `TemplateService` handler line:

```go
	mux.Handle(forgev1connect.NewOperationServiceHandler(&operationService{ops: o.Operations}, opts))
```

- Update the package comment's first sentence to: `Package server is the Forge API: the service catalog, auth discovery, templates and operations, over Connect, gRPC and gRPC-Web from the same handlers.`

- [x] **Step 4: Run the tests**

Run: `go test -race ./internal/server`
Expected: PASS (the new test and every existing one).

- [x] **Step 5: Lint and commit**

```bash
make build test lint
git add internal/server/operation_service.go internal/server/operation_service_test.go internal/server/server.go
git commit -m "server: OperationService

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: The shared renderer and `forge op watch`

**Files:**
- Create: `internal/cli/operation.go`
- Modify: `internal/cli/root.go` (register `op` in the core group), `internal/cli/cli_test.go` (harness gets `ops *server.Operations`)
- Test: `internal/cli/operation_test.go`

**Interfaces:**
- Consumes: `client.Client.Operations` (Task 1); `server.Operations` with `Start`, `Wait` and `Work` (Task 2); `Options.Operations` (Task 3).
- Produces (milestone 2's `env create/destroy` call these):
  - `func opName(s string) string`: accepts `op_x` or `operations/op_x` and returns `operations/op_x`.
  - `func opID(name string) string`: `operations/op_x` → `op_x`.
  - `func (f *Factory) watchOperation(ctx context.Context, c *client.Client, name string) (*forgev1.Operation, error)`: streams events to stderr and returns the finished operation. When `ctx` times out it returns an exit-8 error with the resume hint.
  - `func opFailure(op *forgev1.Operation) error`: nil on success; otherwise an exit-7 `*exitcode.Error`.
  - `func opState(op *forgev1.Operation) string`: `running`, `succeeded`, `failed` or `cancelled`.

Event lines on stderr are `  <step padded to 10> <message>`, for example `  resolve    main is 1a2b3c4`. Unlike the spec's "✓ step" wording, events are progress messages, not finished steps. A check mark on "waiting for image" would be wrong, so the renderer prints step and message.

- [x] **Step 1: Give the harness an operations store**

In `internal/cli/cli_test.go`:
- Add the field `ops *server.Operations` to `harness`, next to `repos`.
- In `newHarness`, set `h := &harness{t: t, dir: t.TempDir(), store: &auth.MemoryStore{}, ops: server.NewOperations(nil)}`.
- In `startAPI`'s `server.Options`, add `Operations: h.ops,`.

- [x] **Step 2: Write the failing tests**

`internal/cli/operation_test.go`:

```go
package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/server"
)

// startOp starts work on the harness API's store and returns its id ("op_...").
func (h *harness) startOp(target string, w server.Work) string {
	h.t.Helper()
	op := h.ops.Start(context.Background(), "test_kind", target, "ana", w)
	return strings.TrimPrefix(op.GetName(), "operations/")
}

func (h *harness) waitOp(id string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.ops.Wait(ctx, "operations/"+id); err != nil {
		h.t.Fatal(err)
	}
}

func steps(emit func(string, string)) {
	emit("resolve", "main is 1a2b3c4")
	emit("image", "found ghcr.io/showmino/x:1a2b3c4")
}

func TestOpWatchFinished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ok := h.startOp("things/a", func(_ context.Context, emit func(string, string)) error { steps(emit); return nil })
	h.waitOp(ok)
	r := h.ok("op", "watch", ok)
	if r.err != "  resolve    main is 1a2b3c4\n  image      found ghcr.io/showmino/x:1a2b3c4\n" {
		t.Errorf("events:\n%q", r.err)
	}
	if r.out != "operations/"+ok+" succeeded\n" {
		t.Errorf("result: %q", r.out)
	}
	h.ok("op", "watch", "operations/"+ok) // full name works too
	if r := h.ok("op", "watch", ok, "-o", "json"); !strings.Contains(r.out, `"kind": "test_kind"`) {
		t.Errorf("json: %s", r.out)
	}

	bad := h.startOp("things/a", func(_ context.Context, emit func(string, string)) error {
		emit("image", "waiting")
		return &server.OpError{Code: "image_timeout", Message: "no image after 10m"}
	})
	h.waitOp(bad)
	r = h.fails(7, "op", "watch", bad)
	if !strings.Contains(r.err, "error: operations/"+bad+" failed: no image after 10m (image_timeout)") {
		t.Errorf("failure: %s", r.err)
	}
	h.fails(3, "op", "watch", "op_000000000000")
	h.fails(2, "op", "watch", "nope")
}

func TestOpWatchLive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	gate := make(chan struct{})
	id := h.startOp("things/a", func(_ context.Context, emit func(string, string)) error {
		emit("resolve", "main is 1a2b3c4")
		<-gate
		emit("ready", "http://x")
		return nil
	})
	go func() { time.Sleep(50 * time.Millisecond); close(gate) }()
	r := h.ok("op", "watch", id)
	if !strings.Contains(r.err, "resolve    main is 1a2b3c4") || !strings.Contains(r.err, "ready      http://x") {
		t.Errorf("events: %q", r.err)
	}
}

func TestOpWatchTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	id := h.startOp("things/a", func(_ context.Context, emit func(string, string)) error {
		emit("image", "waiting")
		<-gate
		return nil
	})
	r := h.fails(8, "op", "watch", id, "--timeout", "200ms")
	if !strings.Contains(r.err, "error: operations/"+id+" is still running") || !strings.Contains(r.err, "hint: resume with: forge op watch "+id) {
		t.Errorf("timeout: %s", r.err)
	}
	if op, _ := h.ops.Get("operations/" + id); op.GetDone() {
		t.Error("the watch timeout stopped the operation")
	}
}

// flakyOps is an OperationService whose first watch sends two events and
// then fails the way a watcher that fell behind does; the second sends all
// three events and the result.
type flakyOps struct {
	forgev1connect.UnimplementedOperationServiceHandler
	calls  atomic.Int32
	failOn connect.Code
}

func (o *flakyOps) WatchOperation(_ context.Context, req *connect.Request[forgev1.WatchOperationRequest], s *connect.ServerStream[forgev1.WatchOperationResponse]) error {
	n := o.calls.Add(1)
	events := []string{"a", "b", "c"}
	if n == 1 {
		events = events[:2]
	}
	for _, m := range events {
		if err := s.Send(&forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Event{
			Event: &forgev1.OperationEvent{Step: "step", Message: m},
		}}); err != nil {
			return err
		}
	}
	if n == 1 {
		return connect.NewError(o.failOn, errors.New("fell behind"))
	}
	return s.Send(&forgev1.WatchOperationResponse{Kind: &forgev1.WatchOperationResponse_Operation{
		Operation: &forgev1.Operation{Name: req.Msg.GetName(), Done: true},
	}})
}

func TestOpWatchReattachPrintsEachEventOnce(t *testing.T) {
	t.Parallel()
	for _, code := range []connect.Code{connect.CodeResourceExhausted, connect.CodeInvalidArgument} {
		ops := &flakyOps{failOn: code}
		mux := http.NewServeMux()
		mux.Handle(forgev1connect.NewOperationServiceHandler(ops))
		srv := httptest.NewServer(mux)
		c, err := client.New(srv.URL, client.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var errOut strings.Builder
		f := newFactory(Deps{IO: IOStreams{ErrOut: &errOut, Out: io.Discard}})
		op, err := f.watchOperation(context.Background(), c, "operations/op_000000000001")
		srv.Close()
		if code == connect.CodeInvalidArgument {
			// Only a dropped stream or RESOURCE_EXHAUSTED reattaches; other errors end the watch.
			if connect.CodeOf(err) != code || ops.calls.Load() != 1 {
				t.Errorf("%v: err %v after %d watches", code, err, ops.calls.Load())
			}
			continue
		}
		if err != nil || !op.GetDone() || ops.calls.Load() != 2 {
			t.Fatalf("%v: %v %v after %d watches", code, op, err, ops.calls.Load())
		}
		if want := "  step       a\n  step       b\n  step       c\n"; errOut.String() != want {
			t.Errorf("printed:\n%q\nwant\n%q", errOut.String(), want)
		}
	}
}
```

Add the imports this test needs to `operation_test.go`: `errors`, `io`, `net/http`, `net/http/httptest`, `sync/atomic`, `connectrpc.com/connect`, `forgev1 "github.com/showmino/forge/gen/forge/v1"`, `"github.com/showmino/forge/gen/forge/v1/forgev1connect"` and `"github.com/showmino/forge/pkg/client"`.

A real HTTP connection can't force a server-side watcher to fall behind, because socket buffers absorb the events. The store's side of that is covered by `TestSlowWatcherDoesNotBlockWork` (Task 2). This test covers the CLI's side with a fake service: two watches, and every line printed once.

- [x] **Step 3: Run them to see them fail**

Run: `go test ./internal/cli -run 'TestOpWatch'`
Expected: build failure, `undefined: opName`.

- [x] **Step 4: Implement**

`internal/cli/operation.go`:

```go
package cli

import (
	"context"
	"errors"
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

func newOpCmd(f *Factory) *cobra.Command {
	cmd := group("op", "Watch and cancel long-running operations", "ops", "operation", "operations")
	cmd.Long = `Long-running work, such as creating an environment, runs as an operation on
the Forge API. Commands that start one watch it until it ends; with
--wait=false they print its id instead, and 'forge op watch <id>' picks it up
again. Operations live in the API's memory for 24 hours.`
	cmd.AddCommand(newOpWatchCmd(f))
	return cmd
}

func newOpWatchCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "watch <id>",
		Short: "Stream an operation's progress until it ends",
		Long: `Stream an operation's progress until it ends: first everything that already
happened, then new events. Exits 0 when it succeeded and 7 when it failed or
was cancelled. With --timeout, stops watching (exit 8) but leaves the
operation running.`,
		Example: `  forge op watch op_9f2c4a1b0c3d
  forge op watch op_9f2c4a1b0c3d --timeout 5m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name := opName(args[0])
			if !validOpID(name) {
				return exitcode.Usagef("%q is not an operation id (op_<id>)", args[0]).WithHint("see 'forge op list'")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			op, err := f.watchOperation(ctx, c, name)
			if err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			return f.printOperation(format, op)
		},
	}
}

// opName accepts "op_x" or "operations/op_x" and returns "operations/op_x".
func opName(s string) string { return "operations/" + strings.TrimPrefix(s, "operations/") }

// opID returns the id of "operations/op_x": "op_x".
func opID(name string) string { return strings.TrimPrefix(name, "operations/") }

// validOpID checks the shape the API accepts, to fail fast with exit 2.
func validOpID(name string) bool {
	id := opID(name)
	if len(id) != len("op_")+12 || !strings.HasPrefix(id, "op_") {
		return false
	}
	return strings.Trim(id[3:], "0123456789abcdef") == ""
}

// opState reads running, succeeded, failed or cancelled.
func opState(op *forgev1.Operation) string {
	switch {
	case !op.GetDone():
		return "running"
	case op.GetError() == nil:
		return "succeeded"
	case op.GetError().GetCode() == "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

// opFailure is nil for a successful operation and exit 7 otherwise.
func opFailure(op *forgev1.Operation) error {
	e := op.GetError()
	if e == nil {
		return nil
	}
	if e.GetCode() == "cancelled" {
		return exitcode.New(exitcode.OperationFailed, "%s was cancelled", op.GetName())
	}
	return exitcode.New(exitcode.OperationFailed, "%s failed: %s (%s)", op.GetName(), e.GetMessage(), e.GetCode())
}

// printOperation prints a finished operation's result.
func (f *Factory) printOperation(format output.Format, op *forgev1.Operation) error {
	if format == output.Table {
		_, err := fmt.Fprintf(f.IO.Out, "%s %s\n", op.GetName(), opState(op))
		return err
	}
	return output.Print(f.IO.Out, format, output.View{Names: []string{op.GetName()}, Data: op})
}

// event prints one progress line on stderr.
func (f *Factory) event(ev *forgev1.OperationEvent) {
	f.infof("  %-10s %s", ev.GetStep(), ev.GetMessage())
}

// watchOperation streams name's events to stderr and returns the finished
// operation. A dropped stream, or a watch the API ended because it fell
// behind, is watched again; the replay's events already printed are skipped.
func (f *Factory) watchOperation(ctx context.Context, c *client.Client, name string) (*forgev1.Operation, error) {
	printed := 0
	for attempt := 0; ; attempt++ {
		stream, err := c.Operations.WatchOperation(ctx, connect.NewRequest(&forgev1.WatchOperationRequest{Name: name}))
		if err != nil {
			return nil, f.watchErr(ctx, name, err)
		}
		seen := 0
		var final *forgev1.Operation
		for stream.Receive() {
			m := stream.Msg()
			if ev := m.GetEvent(); ev != nil {
				if seen >= printed {
					f.event(ev)
					printed++
				}
				seen++
				continue
			}
			final = m.GetOperation()
		}
		err = stream.Err()
		_ = stream.Close()
		if final != nil {
			return final, nil
		}
		again := err == nil || !connect.IsWireError(err) || connect.CodeOf(err) == connect.CodeResourceExhausted
		if again && attempt < 5 && ctx.Err() == nil {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			}
			continue
		}
		if err == nil {
			err = errors.New("the Forge API ended the watch without a result")
		}
		return nil, f.watchErr(ctx, name, err)
	}
}

// watchErr turns the end of --timeout into exit 8 with the resume hint.
func (f *Factory) watchErr(ctx context.Context, name string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded {
		return exitcode.New(exitcode.Timeout, "%s is still running", name).
			WithHint("resume with: forge op watch " + opID(name))
	}
	return err
}
```

In `internal/cli/root.go`, add `newOpCmd(f)` to the core group list after `newCreateCmd(f)`:

```go
	for _, c := range []*cobra.Command{newServiceCmd(f), newTemplateCmd(f), newCreateCmd(f), newOpCmd(f)} {
```

- [x] **Step 5: Run the tests**

Run: `go test -race -count=3 ./internal/cli -run 'TestOpWatch'`
Expected: PASS three times.

- [x] **Step 6: Lint and commit**

```bash
make build test lint
git add internal/cli/operation.go internal/cli/operation_test.go internal/cli/root.go internal/cli/cli_test.go
git commit -m "cli: forge op watch and the shared operation renderer

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `forge op list` and `forge op cancel`

**Files:**
- Modify: `internal/cli/operation.go`
- Test: `internal/cli/operation_test.go`, plus goldens `internal/cli/testdata/op_list_table.golden` and `internal/cli/testdata/op_list_json.golden`

**Interfaces:**
- Consumes: Task 4's `opName`, `opID`, `validOpID`, `opState`, `printOperation`, `watchOperation` and `opFailure`; `f.confirmDestructive(action, name string, yes bool) error` from `prompt.go`.
- Produces: `func age(d time.Duration) string`, which reads `<1m`, `5m`, `3h` or `2d`.

`op list` table columns: `ID KIND TARGET STATE AGE`. JSON prints the `ListOperationsResponse`; when there are none, `{"operations": []}`, as `template list` does.

- [x] **Step 1: Write the failing tests**

Append to `internal/cli/operation_test.go`:

```go
func TestOpList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.ok("op", "list"); r.out != "" || !strings.Contains(r.err, "No operations.") {
		t.Errorf("empty: %v", r)
	}
	if r := h.ok("op", "list", "-o", "json"); !strings.Contains(r.out, `"operations": []`) {
		t.Errorf("empty json: %q", r.out)
	}
	done := h.startOp("environments/a", func(context.Context, func(string, string)) error { return nil })
	h.waitOp(done)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	running := h.startOp("environments/b", func(context.Context, func(string, string)) error { <-gate; return nil })

	r := h.ok("op", "list")
	// Ids are random: replace them before comparing with the golden file.
	out := strings.NewReplacer(done, "op_done", running, "op_running").Replace(r.out)
	golden(t, "op_list_table", out)
	js := strings.NewReplacer(done, "op_done", running, "op_running").Replace(h.ok("op", "list", "-o", "json").out)
	if !strings.Contains(js, `"name": "operations/op_running"`) || strings.Contains(js, `"events"`) {
		t.Errorf("json: %s", js)
	}
	if r := h.ok("op", "list", "--running", "-o", "name"); r.out != "operations/"+running+"\n" {
		t.Errorf("running: %q", r.out)
	}
	if r := h.ok("op", "list", "--target", "environments/a", "-o", "name"); r.out != "operations/"+done+"\n" {
		t.Errorf("target: %q", r.out)
	}
}

func TestOpCancel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.startOp("environments/a", func(ctx context.Context, emit func(string, string)) error {
		emit("image", "waiting")
		<-ctx.Done()
		return ctx.Err()
	})
	h.fails(2, "op", "cancel", id) // no terminal, no --yes
	r := h.ok("op", "cancel", id, "--yes")
	if r.out != "operations/"+id+" cancelling\n" {
		t.Errorf("cancel: %q", r.out)
	}
	h.waitOp(id)
	r = h.ok("op", "cancel", id, "--yes")
	if r.out != "operations/"+id+" already finished (cancelled)\n" {
		t.Errorf("again: %q", r.out)
	}
	h.fails(3, "op", "cancel", "op_000000000000", "--yes")

	waited := h.startOp("environments/a", func(ctx context.Context, _ func(string, string)) error {
		<-ctx.Done()
		return ctx.Err()
	})
	r = h.fails(7, "op", "cancel", waited, "--yes", "--wait")
	if !strings.Contains(r.err, "operations/"+waited+" was cancelled") {
		t.Errorf("--wait: %v", r)
	}

	// A reader may not cancel.
	h.env["FORGE_TOKEN"] = readerToken
	other := h.startOp("environments/a", func(ctx context.Context, _ func(string, string)) error { <-ctx.Done(); return nil })
	h.fails(5, "op", "cancel", other, "--yes")
	h.ops.Cancel("operations/" + other)
}

func TestAge(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		0: "<1m", 59 * time.Second: "<1m", 5 * time.Minute: "5m", 3*time.Hour + time.Minute: "3h", 50 * time.Hour: "2d",
	} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %q, want %q", d, got, want)
		}
	}
}
```

Write `internal/cli/testdata/op_list_table.golden` by hand. Both operations are under a minute old, the running one is newest, and the ids are replaced:

```
ID           KIND        TARGET           STATE       AGE
op_running   test_kind   environments/b   running     <1m
op_done      test_kind   environments/a   succeeded   <1m
```

The column widths come from `output.Print`'s table writer. If the golden differs only in spacing, check it with `go test ./internal/cli -run TestOpList -update` and review the diff before keeping it.

- [x] **Step 2: Run them to see them fail**

Run: `go test ./internal/cli -run 'TestOpList|TestOpCancel|TestAge'`
Expected: FAIL. `op list` and `op cancel` are unknown commands (exit 2), and `age` is undefined.

- [x] **Step 3: Implement**

In `internal/cli/operation.go`, change `cmd.AddCommand(newOpWatchCmd(f))` to:

```go
	cmd.AddCommand(newOpListCmd(f), newOpWatchCmd(f), newOpCancelCmd(f))
```

and add:

```go
func newOpListCmd(f *Factory) *cobra.Command {
	var target string
	var running bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List operations, newest first",
		Example: `  forge op list
  forge op list --running
  forge op list --target environments/pr-142 -o json`,
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
			resp, err := c.Operations.ListOperations(ctx, connect.NewRequest(&forgev1.ListOperationsRequest{Target: target, Running: running}))
			if err != nil {
				return err
			}
			ops := resp.Msg.GetOperations()
			if len(ops) == 0 && format == output.Table {
				f.infof("No operations.")
				return nil
			}
			v := output.View{Headers: []string{"id", "kind", "target", "state", "age"}, Data: resp.Msg}
			if len(ops) == 0 {
				v.Data = map[string]any{"operations": []any{}} // keep '.operations[]' working in jq
			}
			now := f.Now()
			for _, op := range ops {
				v.Rows = append(v.Rows, []string{opID(op.GetName()), op.GetKind(), op.GetTarget(), opState(op), age(now.Sub(op.GetCreateTime().AsTime()))})
				v.Names = append(v.Names, op.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "only operations on this resource, for example environments/pr-142")
	cmd.Flags().BoolVar(&running, "running", false, "only operations that are still running")
	return cmd
}

func newOpCancelCmd(f *Factory) *cobra.Command {
	var yes, wait bool
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Stop a running operation",
		Long: `Ask a running operation to stop. Work it already did is undone where the
operation's kind allows it (a cancelled environment create removes the
environment). Cancelling a finished operation changes nothing.`,
		Example: `  forge op cancel op_9f2c4a1b0c3d --yes
  forge op cancel op_9f2c4a1b0c3d --yes --wait`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name := opName(args[0])
			if !validOpID(name) {
				return exitcode.Usagef("%q is not an operation id (op_<id>)", args[0]).WithHint("see 'forge op list'")
			}
			if err := f.confirmDestructive("cancel", name, yes); err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			resp, err := c.Operations.CancelOperation(ctx, connect.NewRequest(&forgev1.CancelOperationRequest{Name: name}))
			if err != nil {
				return err
			}
			op := resp.Msg.GetOperation()
			if op.GetDone() {
				if format != output.Table {
					return f.printOperation(format, op)
				}
				_, err := fmt.Fprintf(f.IO.Out, "%s already finished (%s)\n", name, opState(op))
				return err
			}
			if !wait {
				if format != output.Table {
					return f.printOperation(format, op)
				}
				_, err := fmt.Fprintf(f.IO.Out, "%s cancelling\n", name)
				return err
			}
			if op, err = f.watchOperation(ctx, c, name); err != nil {
				return err
			}
			if err := opFailure(op); err != nil {
				return err
			}
			return f.printOperation(format, op)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&wait, "wait", false, "watch the operation until it has stopped")
	return cmd
}

// age reads a duration coarsely: <1m, 5m, 3h, 2d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
```

`TestAge` expects `50h` → `2d`, which falls in the `default` branch (50h ≥ 48h → 2d), and `3h1m` → `3h`.

`confirmDestructive`'s prompt reads "This will cancel operations/op_x. Type its name to confirm:" and accepts `op_x`, because `shortName` keeps what follows the last `/`.

- [x] **Step 4: Run the tests**

Run: `go test -race ./internal/cli`
Expected: PASS, every existing CLI test included.

- [x] **Step 5: Lint and commit**

```bash
make build test lint
git add internal/cli/operation.go internal/cli/operation_test.go internal/cli/testdata/op_list_table.golden
git commit -m "cli: forge op list and forge op cancel

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

(`op_list_json.golden` is not used: the JSON check in `TestOpList` matches on substrings, because JSON carries timestamps.)

---

### Task 6: Smoke cases, docs, PR

**Files:**
- Modify: `.claude/skills/forge/scripts/cli-smoke.sh`, `.claude/skills/forge/SKILL.md`, `README.md`, `docs/plans/2026-10-08-phase-3-m1-operations.md` (tick the boxes)

- [x] **Step 1: Smoke cases.** In `cli-smoke.sh`, before `echo; echo "#### output and global flags"`, add:

```bash
echo; echo "#### operations"
t 0 "op list" -- f op list
t 0 "op list json" -- f op list -o json
t 3 "op watch missing" -- f op watch op_000000000000
t 2 "op watch bad id" -- f op watch nope
t 2 "op cancel without --yes (no tty)" -- f op cancel op_000000000000
t 3 "op cancel missing" -- f op cancel op_000000000000 --yes
```

The full script needs a Dex login, so it runs on k3s in milestone 3. Here, check the new cases against a local API started with `make api-dev` (token `dev`) in the background:

```bash
export FORGE_TOKEN=dev
bin/forge --api-url http://localhost:8080 op list; echo "exit $?"                          # 0, "No operations."
bin/forge --api-url http://localhost:8080 op watch op_000000000000; echo "exit $?"         # 3
bin/forge --api-url http://localhost:8080 op watch nope; echo "exit $?"                    # 2
bin/forge --api-url http://localhost:8080 op cancel op_000000000000 --yes; echo "exit $?"  # 3
```

Stop the API afterwards by its PID (`kill <pid>`), never with `pkill -f`.

- [x] **Step 2: Docs.**
  - **`SKILL.md` "Status and roadmap":** replace the Phase 2 "follow-up backlog" bullet's position by adding, after the Phase 2 bullet: `- **Phase 3 (in progress):** spec docs/specs/2026-10-08-phase-3-environments-design.md. Milestone 1: operations (OperationService, in-memory store, forge op list/watch/cancel) per docs/plans/2026-10-08-phase-3-m1-operations.md. Then milestone 2 (environments) and 3 (k3s and the 10-minute exit gate).`
  - **README:** after the paragraph that starts "`forge create <name> --template <template> --team <team>` asks forge-api to", add: "`forge op list`, `forge op watch <id>` and `forge op cancel <id>` show and stop long-running work on the API (operations). Phase 3's environments are the first to use them."

- [ ] **Step 3: Gate, PR, merge**

```bash
make build test lint
git add -A .claude README.md docs
git commit -m "docs: Phase 3 milestone 1 (operations)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push -u origin phase-3-m1
gh pr create --base main --title "Phase 3 milestone 1: operations" --body-file <prepared body>
```

The PR also carries the Phase 3 spec commit from `phase-3`. Wait for CI, then merge (the owner's standing rule allows it).
