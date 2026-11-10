package store

import (
	"context"
	"testing"
	"time"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryTeams(t *testing.T) {
	t.Parallel()
	runTeamSuite(t, NewMemory())
}

func TestPostgresTeams(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runTeamSuite(t, p)
}

// runTeamSuite checks a TeamStore that starts empty.
func runTeamSuite(t *testing.T, s TeamStore) {
	ctx := context.Background()
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

	got, err := s.TeamSettings(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if err := s.SetTeamOncall(ctx, "billing", "billing-primary", "ana@example.com", at); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamOncall(ctx, "risk", "risk-primary", "bo@example.com", at); err != nil {
		t.Fatal(err)
	}
	// Changing it again replaces it.
	if err := s.SetTeamOncall(ctx, "billing", "billing-weekdays", "bo@example.com", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An empty rotation removes it.
	if err := s.SetTeamOncall(ctx, "risk", "", "bo@example.com", at); err != nil {
		t.Fatal(err)
	}
	got, err = s.TeamSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := got["billing"]
	if len(got) != 1 || !ok {
		t.Fatalf("settings %v", got)
	}
	if b.Oncall != "billing-weekdays" || b.UpdatedBy != "bo@example.com" || !b.UpdatedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("billing %+v", b)
	}
}
