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
