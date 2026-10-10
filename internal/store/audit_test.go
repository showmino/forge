package store

import (
	"context"
	"strings"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
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
