package store

import (
	"context"
	"testing"
	"time"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryRules(t *testing.T) {
	t.Parallel()
	runRuleSuite(t, NewMemory())
}

func TestPostgresRules(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runRuleSuite(t, p)
}

// runRuleSuite checks a RuleStore that starts empty.
func runRuleSuite(t *testing.T, s RuleStore) {
	ctx := context.Background()
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if got, err := s.RuleModes(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, step := range []struct{ rule, mode, by string }{
		{"images.pinned-version", "warn", "ana@example.com"},
		{"charts.allowed-repos", "off", "ana@example.com"},
		{"images.pinned-version", "enforce", "bo@example.com"},
		{"charts.allowed-repos", "", "bo@example.com"},
	} {
		if err := s.SetRuleMode(ctx, step.rule, step.mode, step.by, at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RuleModes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["images.pinned-version"] != (RuleMode{Mode: "enforce", UpdatedBy: "bo@example.com", UpdatedAt: at}) {
		t.Errorf("modes %v", got)
	}
}
