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
