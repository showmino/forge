package store

import (
	"context"
	"testing"
	"time"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryReleases(t *testing.T) {
	t.Parallel()
	runReleaseSuite(t, NewMemory())
}

func TestPostgresReleases(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runReleaseSuite(t, p)
}

// runReleaseSuite checks a ReleaseStore that starts empty.
func runReleaseSuite(t *testing.T, s ReleaseStore) {
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	if got, err := s.Releases(ctx, "shop", 10); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for i, app := range []string{"shop", "shop", "pay", "shop"} {
		n, err := s.AddRelease(ctx, Release{Application: app, CreatedAt: at.Add(time.Duration(i) * time.Hour), CreatedBy: "ana@example.com", Note: "", Parts: []ReleasePart{
			{Service: "services/" + app, Version: "v1.0.0", Commit: "abc", DeployCommit: "def"},
		}})
		want := map[int]int{0: 1, 1: 2, 2: 1, 3: 3}[i]
		if err != nil || n != want {
			t.Fatalf("release %d of %s: number %d, want %d (%v)", i, app, n, want, err)
		}
	}
	got, err := s.Releases(ctx, "shop", 2)
	if err != nil || len(got) != 2 || got[0].Number != 3 || got[1].Number != 2 {
		t.Fatalf("newest two of shop: %+v %v", got, err)
	}
	r := got[0]
	if r.Application != "shop" || r.CreatedBy != "ana@example.com" || !r.CreatedAt.Equal(at.Add(3*time.Hour)) || len(r.Parts) != 1 ||
		r.Parts[0] != (ReleasePart{Service: "services/shop", Version: "v1.0.0", Commit: "abc", DeployCommit: "def"}) {
		t.Errorf("release 3: %+v", r)
	}
	if got, err := s.Releases(ctx, "pay", 10); err != nil || len(got) != 1 || got[0].Number != 1 {
		t.Errorf("pay: %+v %v", got, err)
	}
}
