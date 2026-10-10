package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

// auditAPI serves a catalog with an audit store; tokens: "writer" (ana, may
// write), "reader" (bo).
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
		auditCode(errors.New("plain")) != "unknown" ||
		auditCode(context.Canceled) != "canceled" || auditCode(fmt.Errorf("wait: %w", context.DeadlineExceeded)) != "deadline_exceeded" ||
		auditCode(connect.NewError(connect.CodeInternal, context.Canceled)) != "internal" {
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
}

func TestAuditText(t *testing.T) {
	t.Parallel()
	if got := auditText("a\x00b\xffc", 10); got != "ab\uFFFDc" {
		t.Errorf("got %q", got)
	}
	if got := auditText(strings.Repeat("é", 300), 256); len([]rune(got)) != 256 {
		t.Errorf("clipped to %d runes", len([]rune(got)))
	}
	if got := auditText("a\x1b[2Jb\ac\nd\u202ee\u2066f", 50); got != "a\uFFFD[2Jb\uFFFDc\uFFFDd\uFFFDe\uFFFDf" {
		t.Errorf("control and bidi: %q", got)
	}
	if auditText("ok", 256) != "ok" {
		t.Error("clean text changed")
	}
}

func TestAuditCleansCallerText(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	req := as("writer", &forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"}})
	req.Header().Set("X-Request-Id", "\xff")
	req.Header().Set("Forge-Client-Version", "a\x00b") // the HTTP client may refuse this one
	_, err := c.RegisterService(context.Background(), req)
	if err != nil {
		req.Header().Set("Forge-Client-Version", "ab")
		if _, err = c.RegisterService(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	evs := allEvents(t, audit)
	if len(evs) == 0 {
		t.Fatal("no event")
	}
	for _, e := range evs {
		if _, err := protojson.Marshal(e); err != nil {
			t.Errorf("event does not marshal: %v", err)
		}
		for _, s := range []string{e.GetClient().GetVersion(), e.GetClient().GetRequestId(), e.GetService(), e.GetTarget()} {
			if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				t.Errorf("unclean text %q", s)
			}
		}
	}
	// A NUL in a header, which a Go client will not send, still comes out clean.
	h := http.Header{}
	h.Set("Forge-Client-Version", "a\x00b")
	ev := newAuditRecord("x").finish(h, nil)
	if ev.GetClient().GetVersion() != "ab" {
		t.Errorf("version %q", ev.GetClient().GetVersion())
	}
}

func TestAuditBoundsLongIdentifiers(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	_, _ = c.RegisterService(context.Background(), as("writer", &forgev1.RegisterServiceRequest{
		ServiceId: strings.Repeat("a", 10000), Service: &forgev1.Service{OwnerTeam: "t"},
	}))
	evs := allEvents(t, audit)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if n := len([]rune(evs[0].GetService())); n == 0 || n > 256 {
		t.Errorf("service has %d runes", n)
	}
	if n := len([]rune(evs[0].GetTarget())); n == 0 || n > 256 {
		t.Errorf("target has %d runes", n)
	}
}

func TestAuditUnauthenticatedControlCharacters(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	c := auditAPI(t, audit, nil)
	_, _ = c.RegisterService(context.Background(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "x\u001b[2J\u202eevil\nline2", Service: &forgev1.Service{OwnerTeam: "billing"},
	}))
	evs := allEvents(t, audit)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	for _, s := range []string{evs[0].GetService(), evs[0].GetTarget()} {
		if s == "" {
			t.Error("empty field")
		}
		for _, r := range s {
			if unicode.IsControl(r) || r == '\u202e' {
				t.Errorf("%q holds %U", s, r)
			}
		}
	}
}

// panicStore panics on every write, like a bug in a handler.
type panicStore struct{ brokenStore }

func (panicStore) Write(context.Context, func(store.Tx) error) error { panic("kaboom\x1b[2J") }

func TestAuditHandlerPanicIsRecorded(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	h, err := New(Options{
		Authenticator: StaticAuthenticator{"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}}},
		Authorizer:    Authorizer{WriterGroups: []string{"platform"}},
		Catalog:       NewCatalogWithStore(panicStore{}, nil),
		Audit:         audit,
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	var propagated any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { propagated = recover() }() // what net/http would do
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	_, _ = c.RegisterService(context.Background(), as("writer", &forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing"},
	}))
	if s, _ := propagated.(string); !strings.HasPrefix(s, "kaboom") {
		t.Fatalf("panic did not propagate unchanged: %v", propagated)
	}
	evs := allEvents(t, audit)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	r := evs[0].GetResult()
	if evs[0].GetAction() != "service.register" || evs[0].GetService() != "services/payments" || r.GetCode() != "internal" ||
		!strings.HasPrefix(r.GetMessage(), "panic: kaboom") || strings.ContainsRune(r.GetMessage(), 0x1b) {
		t.Fatalf("event %v", evs[0])
	}
}

func TestAuditDescribePanicKeepsAction(t *testing.T) {
	t.Parallel()
	rec := newAuditRecord("x")
	rec.safeDescribe(auditAction{name: "x", describe: func(any) (string, string, map[string]string) { panic("bad") }}, "msg")
	ev := rec.snapshot()
	if ev.GetAction() != "x" || ev.GetService() != "" || ev.GetTarget() != "" {
		t.Fatalf("event %v", ev)
	}
}

func TestAuditAnonymousLimit(t *testing.T) {
	t.Parallel()
	audit := store.NewMemory()
	var logs bytes.Buffer
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	a := &auditor{store: audit, log: slog.New(slog.NewJSONHandler(&logs, nil)), now: func() time.Time { return now }}
	event := func(kind string) *forgev1.AuditEvent {
		return &forgev1.AuditEvent{Kind: "request", Action: "x", Actor: &forgev1.AuditActor{Kind: kind}, Result: &forgev1.AuditResult{Code: "ok"}}
	}
	for range 60 {
		a.append(context.Background(), event(actorAnonymous))
	}
	a.append(context.Background(), event(actorAnonymous))
	a.append(context.Background(), event(actorAnonymous))
	if n := len(allEvents(t, audit)); n != 60 {
		t.Fatalf("stored %d anonymous events, want 60", n)
	}
	a.append(context.Background(), event(actorUser))
	a.append(context.Background(), event(actorWorkload))
	a.append(context.Background(), event(actorSystem))
	if n := len(allEvents(t, audit)); n != 63 {
		t.Fatalf("stored %d events, want 63: other actors are not limited", n)
	}
	if n := strings.Count(logs.String(), "over the limit"); n != 1 || !strings.Contains(logs.String(), `"dropped_total":1`) ||
		!strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("want one warning in the window, got %d: %s", n, logs.String())
	}
	now = now.Add(anonymousWindow)
	a.append(context.Background(), event(actorAnonymous))
	if n := len(allEvents(t, audit)); n != 64 {
		t.Fatalf("stored %d events after the window, want 64", n)
	}
}
