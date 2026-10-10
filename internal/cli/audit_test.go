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
	at := time.Date(2025, 10, 10, 9, 14, 3, 0, time.UTC)
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
	if r := h.ok("audit", "list", "--since", "2025-10-10T09:16:00Z", "-o", "name"); r.out != "auditEvents/4\nauditEvents/3\n" {
		t.Errorf("--since RFC 3339: %q", r.out)
	}
	h.fails(2, "audit", "list", "--since", "yesterday")
	h.fails(2, "audit", "list", "--limit", "0")
}

func TestAuditListMarksDryRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, code := range []string{"ok", "invalid_argument"} {
		if _, err := h.audit.AppendAuditEvent(context.Background(), &forgev1.AuditEvent{
			Time: timestamppb.Now(), Kind: "request", Action: "deploy.start", Service: "services/payments", Target: "deployments/payments/staging",
			Actor:  &forgev1.AuditActor{Kind: "user", Subject: "bo"},
			Result: &forgev1.AuditResult{Code: code}, Details: map[string]string{"validate_only": "true"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := h.ok("audit", "list")
	if !strings.Contains(r.out, "ok (dry run)") || !strings.Contains(r.out, "invalid_argument (dry run)") || !strings.Contains(r.out, " bo ") {
		t.Errorf("table: %s", r.out)
	}
	if r := h.ok("audit", "get", "1"); strings.Contains(r.out, "dry run") {
		t.Errorf("get must keep the plain code: %s", r.out)
	}
}

func TestAuditListSinceDuration(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedAudit() // all in 2025-10-10 09:14–09:17 UTC
	// 36500 days (100 years) back covers them; 1h back from now does not (now is the real clock).
	if r := h.ok("audit", "list", "--since", "36500d", "-o", "name"); strings.Count(r.out, "\n") != 4 {
		t.Errorf("--since 36500d: %q", r.out)
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
