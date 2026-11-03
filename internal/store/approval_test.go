package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryApprovals(t *testing.T) {
	t.Parallel()
	runApprovalSuite(t, NewMemory())
}

func TestPostgresApprovals(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runApprovalSuite(t, p)
}

// runApprovalSuite checks an ApprovalStore that starts empty.
func runApprovalSuite(t *testing.T, s ApprovalStore) {
	ctx := context.Background()
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	mk := func(id, svc string, min int) Approval {
		return Approval{ID: id, Service: svc, Environment: "production", Commit: "c-" + id, RunID: 7, RunURL: "https://github.com/x/actions/runs/7",
			RequestedBy: "ana", TriggeredBy: "cy", State: ApprovalPending, CreatedAt: at.Add(time.Duration(min) * time.Minute)}
	}
	for _, a := range []Approval{mk("apr_1", "net", 0), mk("apr_2", "dns", 1), mk("apr_3", "net", 2)} {
		if err := s.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// A newer approval of net supersedes the older one; dns's is untouched.
	if a, err := s.GetApproval(ctx, "apr_1"); err != nil || a.State != ApprovalSuperseded {
		t.Errorf("apr_1: %+v %v, want superseded", a, err)
	}
	if a, err := s.GetApproval(ctx, "apr_2"); err != nil || a.State != ApprovalPending || a.Commit != "c-apr_2" || a.RunID != 7 || a.TriggeredBy != "cy" || !a.CreatedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("apr_2: %+v %v", a, err)
	}
	if _, err := s.GetApproval(ctx, "apr_x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}

	list, err := s.ListApprovals(ctx, ApprovalQuery{Limit: 10})
	if err != nil || len(list) != 3 || list[0].ID != "apr_3" || list[2].ID != "apr_1" {
		t.Errorf("list all: %v %v", ids(list), err)
	}
	list, _ = s.ListApprovals(ctx, ApprovalQuery{Service: "net", State: ApprovalPending, Limit: 10})
	if len(list) != 1 || list[0].ID != "apr_3" {
		t.Errorf("net pending: %v", ids(list))
	}
	list, _ = s.ListApprovals(ctx, ApprovalQuery{Limit: 1})
	if len(list) != 1 {
		t.Errorf("limit: %v", ids(list))
	}

	// Decide once; a second decision or a superseded one is refused.
	a, err := s.DecideApproval(ctx, "apr_3", ApprovalApproved, "bo", "looks fine", at.Add(time.Hour))
	if err != nil || a.State != ApprovalApproved || a.DecidedBy != "bo" || a.Reason != "looks fine" || !a.DecidedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("approve: %+v %v", a, err)
	}
	if _, err := s.DecideApproval(ctx, "apr_3", ApprovalRejected, "cy", "", at); !errors.Is(err, ErrNotPending) {
		t.Errorf("decide twice: %v", err)
	}
	if a, err := s.DecideApproval(ctx, "apr_1", ApprovalApproved, "bo", "", at); !errors.Is(err, ErrNotPending) || a.State != ApprovalSuperseded {
		t.Errorf("decide a superseded one: %+v %v", a, err)
	}
	if _, err := s.DecideApproval(ctx, "apr_x", ApprovalApproved, "bo", "", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("decide a missing one: %v", err)
	}
	if err := s.SetDispatchError(ctx, "apr_3", "403 from GitHub"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetApproval(ctx, "apr_3"); a.DispatchError != "403 from GitHub" {
		t.Errorf("dispatch error: %+v", a)
	}

	// Claim once, and only an approved one.
	if _, err := s.ClaimApproval(ctx, "apr_2", at); !errors.Is(err, ErrNotPending) {
		t.Errorf("claim a pending one: %v", err)
	}
	a, err = s.ClaimApproval(ctx, "apr_3", at.Add(2*time.Hour))
	if err != nil || a.State != ApprovalClaimed || !a.ClaimedAt.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("claim: %+v %v", a, err)
	}
	if _, err := s.ClaimApproval(ctx, "apr_3", at); !errors.Is(err, ErrNotPending) {
		t.Errorf("claim twice: %v", err)
	}
	// A new request does not touch a claimed approval, but supersedes an
	// approved one not claimed yet: an older commit never deploys after it.
	if _, err := s.DecideApproval(ctx, "apr_2", ApprovalApproved, "bo", "", at); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Approval{mk("apr_4", "net", 3), mk("apr_5", "dns", 4)} {
		if err := s.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if a, _ := s.GetApproval(ctx, "apr_3"); a.State != ApprovalClaimed {
		t.Errorf("claimed after a new request: %+v", a)
	}
	if a, _ := s.GetApproval(ctx, "apr_2"); a.State != ApprovalSuperseded {
		t.Errorf("approved, not claimed, after a new request: %+v", a)
	}
}

func ids(as []Approval) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}
