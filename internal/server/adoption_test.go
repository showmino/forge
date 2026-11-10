package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// A log of a few weeks: what GetAdoption counts and what it leaves out.
func TestGetAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) // a Wednesday
	st := store.NewMemory()
	add := func(daysAgo float64, action, who, role string, kind string, mods ...func(*forgev1.AuditEvent)) {
		t.Helper()
		ev := &forgev1.AuditEvent{
			Time: timestamppb.New(now.Add(-time.Duration(daysAgo * 24 * float64(time.Hour)))), Kind: "request", Action: action,
			Actor: &forgev1.AuditActor{Kind: kind, Email: who, Subject: who}, Result: &forgev1.AuditResult{Code: "ok"},
		}
		if role != "" {
			ev.Details = map[string]string{"role": role}
		}
		for _, m := range mods {
			m(ev)
		}
		if _, err := st.AppendAuditEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// The last 30 days.
	add(1, "deploy.start", "ana@example.com", "member", "user")
	add(2, "deploy.promote", "ana@example.com", "member", "user")
	add(3, "service.update", "bo@example.com", "admin", "user")
	add(4, "deploy.start", "showmino/payments", "", "workload") // CI: a deploy, not a person
	add(5, "environment.create", "li@example.com", "", "user")  // before roles were recorded
	add(6, "deploy.start", "ana@example.com", "member", "user", func(e *forgev1.AuditEvent) { e.Result.Code = "permission_denied" })
	add(7, "deploy.start", "ana@example.com", "member", "user", func(e *forgev1.AuditEvent) { e.Details["validate_only"] = "true" })
	add(8, "deploy.rollback", "ana@example.com", "member", "outcome", func(e *forgev1.AuditEvent) { e.Kind = "outcome" })
	// The 30 days before.
	add(40, "deploy.start", "ana@example.com", "member", "user")
	add(45, "service.update", "ana@example.com", "admin", "user")
	// Too old to count.
	add(120, "deploy.start", "old@example.com", "member", "user")

	s := &auditService{store: st, now: func() time.Time { return now }}
	resp, err := s.GetAdoption(readerCtx(), connect.NewRequest(&forgev1.GetAdoptionRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	last, prev := m.GetLast(), m.GetPrevious()
	// People: ana, bo and li changed something; CI is no person.
	if last.GetActiveUsers() != 3 || last.GetDeploys() != 3 || last.GetWrites() != 3 || last.GetSelfServed() != 2 {
		t.Errorf("last 30 days: %v", last)
	}
	if prev.GetActiveUsers() != 1 || prev.GetDeploys() != 1 || prev.GetWrites() != 2 || prev.GetSelfServed() != 1 {
		t.Errorf("the 30 before: %v", prev)
	}
	weeks := m.GetWeeks()
	if len(weeks) != 12 {
		t.Fatalf("%d weeks", len(weeks))
	}
	this := weeks[11]
	if !this.GetStart().AsTime().Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("this week starts %v", this.GetStart().AsTime())
	}
	// Monday 9 to Wednesday 11: the deploys of 1 and 2 days ago.
	if this.GetDeploys() != 2 || this.GetActiveUsers() != 1 || this.GetSelfServed() != 2 {
		t.Errorf("this week: %v", this)
	}
	// The log goes back further than 12 weeks: the numbers start with the
	// first of the 12 weeks, Monday 24 August.
	if !m.GetSince().AsTime().Equal(time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("since %v", m.GetSince().AsTime())
	}

	if _, err := s.GetAdoption(context.Background(), connect.NewRequest(&forgev1.GetAdoptionRequest{})); err == nil {
		t.Error("no login: allowed")
	}
}

// The audit interceptor records whether a person is an admin.
func TestAuditRecordsTheRole(t *testing.T) {
	t.Parallel()
	for who, p := range map[string]*Principal{
		"admin":  {Subject: "ana", Permissions: []string{PermServicesRead, PermServicesWrite}},
		"member": {Subject: "bo", Permissions: []string{PermServicesRead}},
	} {
		r := &auditRecord{ev: &forgev1.AuditEvent{}}
		r.setActor(p)
		r.describe(auditAction{describe: func(any) (string, string, map[string]string) { return "", "x", map[string]string{"a": "b"} }}, struct{}{})
		if ev := r.finish(nil, nil); ev.GetDetails()["role"] != who || ev.GetDetails()["a"] != "b" {
			t.Errorf("%s: %v", who, ev.GetDetails())
		}
	}
	r := &auditRecord{ev: &forgev1.AuditEvent{}}
	r.setActor(&Principal{Subject: "repo:x", Workload: &Workload{Repository: "showmino/x"}})
	if ev := r.finish(nil, nil); ev.GetDetails()["role"] != "" {
		t.Errorf("a workload has a role: %v", ev.GetDetails())
	}
}
