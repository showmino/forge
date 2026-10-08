package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/pkg/client"
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
