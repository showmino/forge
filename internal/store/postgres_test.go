package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/showmino/forge/internal/store/pgtest"
)

func openTestPostgres(t *testing.T, url string) *Postgres {
	t.Helper()
	p, err := OpenPostgres(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPostgres(t *testing.T) {
	t.Parallel()
	runSuite(t, func(t *testing.T) Store {
		p := openTestPostgres(t, pgtest.URL(t))
		if err := p.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p
	})
}

func TestPostgresMigrate(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	a, b := openTestPostgres(t, url), openTestPostgres(t, url)
	var wg sync.WaitGroup
	for _, p := range []*Postgres{a, b} {
		wg.Go(func() {
			if err := p.Migrate(context.Background()); err != nil {
				t.Errorf("concurrent migrate: %v", err)
			}
		})
	}
	wg.Wait()
	if err := a.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := a.pool.QueryRow(context.Background(), "SELECT count(*) FROM forge_migrations").Scan(&n); err != nil || n != 3 {
		t.Fatalf("forge_migrations has %d rows (%v), want 3", n, err)
	}
}

func TestOpenPostgresBadURL(t *testing.T) {
	t.Parallel()
	_, err := OpenPostgres(context.Background(), "postgres://forge:s3cret-value@127.0.0.1:1/forge?connect_timeout=1")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("the error shows the password: %v", err)
	}
}
