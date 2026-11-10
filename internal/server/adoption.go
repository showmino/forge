package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// Adoption (phase 15, milestone E): how much Forge is used, read from the
// audit log.

const (
	adoptionPeriod = 30 * 24 * time.Hour
	adoptionWeeks  = 12
	adoptionFresh  = 5 * time.Minute
	// adoptionPage is how many events a page of the audit log holds, and
	// adoptionMax how many are read at most: a busy Forge counts its most
	// recent ones.
	adoptionPage = 1000
	adoptionMax  = 200_000
)

var deployActions = map[string]bool{ActDeployStart: true, ActDeployPromote: true, ActDeployRollback: true}

func (s *auditService) GetAdoption(ctx context.Context, _ *connect.Request[forgev1.GetAdoptionRequest]) (*connect.Response[forgev1.GetAdoptionResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adoption != nil && now().Before(s.adoptionUntil) {
		return connect.NewResponse(s.adoption), nil
	}
	out, err := adoption(ctx, s.store, now())
	if err != nil {
		return nil, err
	}
	s.adoption, s.adoptionUntil = out, now().Add(adoptionFresh)
	return connect.NewResponse(out), nil
}

// tally counts one stretch of time.
type tally struct {
	people             map[string]bool
	deploys            int32
	writes, selfServed int32
}

func newTally() *tally { return &tally{people: map[string]bool{}} }

func (t *tally) add(e *forgev1.AuditEvent) {
	if deployActions[e.GetAction()] {
		t.deploys++
	}
	a := e.GetActor()
	if a.GetKind() != actorUser {
		return
	}
	who := a.GetEmail()
	if who == "" {
		who = a.GetSubject()
	}
	t.people[who] = true
	switch e.GetDetails()["role"] {
	case "member":
		t.writes++
		t.selfServed++
	case "admin":
		t.writes++
	}
}

// counts reports whether e is a change that happened: a write call that
// succeeded and was not a dry run.
func counts(e *forgev1.AuditEvent) bool {
	return e.GetKind() == auditRequest && e.GetResult().GetCode() == "ok" && e.GetDetails()["validate_only"] != "true"
}

// mondayOf is the start of t's week, Monday 00:00 UTC.
func mondayOf(t time.Time) time.Time {
	d := t.UTC().Truncate(24 * time.Hour)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func adoption(ctx context.Context, st store.AuditStore, now time.Time) (*forgev1.GetAdoptionResponse, error) {
	weekStart := mondayOf(now).AddDate(0, 0, -7*(adoptionWeeks-1))
	from := now.Add(-2 * adoptionPeriod)
	if weekStart.Before(from) {
		from = weekStart
	}
	last, prev := newTally(), newTally()
	weeks := make([]*tally, adoptionWeeks)
	for i := range weeks {
		weeks[i] = newTally()
	}
	oldest := now
	var before int64
	read := 0
	for read < adoptionMax {
		page, err := st.ListAuditEvents(ctx, store.AuditQuery{Since: from, Until: now, BeforeID: before, Limit: adoptionPage})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the audit log: %w", err))
		}
		for _, e := range page {
			read++
			at := e.GetTime().AsTime()
			if at.Before(oldest) {
				oldest = at
			}
			if !counts(e) {
				continue
			}
			switch {
			case !at.Before(now.Add(-adoptionPeriod)):
				last.add(e)
			case !at.Before(now.Add(-2 * adoptionPeriod)):
				prev.add(e)
			}
			if w := int(at.Sub(weekStart) / (7 * 24 * time.Hour)); !at.Before(weekStart) && w < adoptionWeeks {
				weeks[w].add(e)
			}
		}
		if len(page) < adoptionPage {
			break
		}
		id, ok := store.ParseAuditEventName(page[len(page)-1].GetName())
		if !ok {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("an audit event without an id: %q", page[len(page)-1].GetName()))
		}
		before = id
	}
	// Whether the log goes back further than the window: then the numbers
	// cover all of it.
	older, err := st.ListAuditEvents(ctx, store.AuditQuery{Until: from, Limit: 1})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the audit log: %w", err))
	}
	since := oldest
	if len(older) > 0 || read >= adoptionMax {
		since = from
	}
	out := &forgev1.GetAdoptionResponse{Last: last.period(), Previous: prev.period(), Since: timestamppb.New(since)}
	for i, w := range weeks {
		out.Weeks = append(out.Weeks, &forgev1.AdoptionWeek{
			Start: timestamppb.New(weekStart.AddDate(0, 0, 7*i)), Deploys: w.deploys, SelfServed: w.selfServed, ActiveUsers: int32(len(w.people)), //nolint:gosec // bounded by adoptionMax
		})
	}
	return out, nil
}

func (t *tally) period() *forgev1.AdoptionPeriod {
	return &forgev1.AdoptionPeriod{ActiveUsers: int32(len(t.people)), Deploys: t.deploys, Writes: t.writes, SelfServed: t.selfServed} //nolint:gosec // bounded by adoptionMax
}
