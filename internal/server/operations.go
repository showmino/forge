package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// opNameKey carries an operation's own name in its work's context.
type opNameKey struct{}

// noOutcomeKey marks a context whose operation appends no outcome event.
type noOutcomeKey struct{}

// withoutOutcome returns ctx marked so an operation started with it appends
// no outcome event: its owner appends one itself. Resource runs do, because
// a run can end after forge-api restarted and its operation is gone.
func withoutOutcome(ctx context.Context) context.Context {
	return context.WithValue(ctx, noOutcomeKey{}, true)
}

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

	audit atomic.Pointer[auditor]

	mu  sync.Mutex
	ops map[string]*operation
}

type operation struct {
	pb     *forgev1.Operation // guarded by Operations.mu
	cancel context.CancelFunc
	done   chan struct{}
	subs   map[*watcher]struct{} // guarded by Operations.mu

	auditEv   *forgev1.AuditEvent // the outcome event's template; set before the work starts
	noOutcome bool                // the owner appends the outcome event
}

// watcher receives live messages. The publisher closes ch when the operation
// ends, after the final message, or when the watcher fell behind.
type watcher struct {
	ch chan *forgev1.WatchOperationResponse
}

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
	ctx = context.WithValue(ctx, opNameKey{}, o.pb.Name)
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
	o.noOutcome = ctx.Value(noOutcomeKey{}) != nil
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
	e := opError(ctx, err)
	s.finish(o, e)
	if o.auditEv != nil && !o.noOutcome {
		ev := proto.Clone(o.auditEv).(*forgev1.AuditEvent)
		ev.Result = &forgev1.AuditResult{Code: "ok"}
		switch {
		case e == nil:
		case e.GetCode() == OpCodeCancelled:
			ev.Result = &forgev1.AuditResult{Code: "cancelled", Message: auditText(e.GetMessage(), auditMessageMax)}
		default:
			ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(e.GetCode()+": "+e.GetMessage(), auditMessageMax)}
		}
		s.appendAudit(ctx, ev)
	}
}

// setAuditor makes finished operations append their outcome to the audit log.
func (s *Operations) setAuditor(a *auditor) { s.audit.Store(a) }

// appendAudit appends ev when an auditor is set.
func (s *Operations) appendAudit(ctx context.Context, ev *forgev1.AuditEvent) {
	s.audit.Load().append(ctx, ev)
}

// auditService is the service an operation's audit events name, or "".
func (s *Operations) auditService(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.ops[name]; ok && o.auditEv != nil {
		return o.auditEv.GetService()
	}
	return ""
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
	// Oldest by creation time first; the spec does not say which order, so
	// creation order is the deliberate choice.
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
