package store

import (
	"context"
	"testing"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemoryResources(t *testing.T) {
	t.Parallel()
	runResourceSuite(t, func(*testing.T) ResourceStore { return NewMemory() })
}

func TestPostgresResources(t *testing.T) {
	t.Parallel()
	runResourceSuite(t, func(t *testing.T) ResourceStore {
		p := openTestPostgres(t, pgtest.URL(t))
		if err := p.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p
	})
}

func TestBackends(t *testing.T) {
	t.Parallel()
	var _ Backend = NewMemory()
	var _ Backend = (*Postgres)(nil)
}
