package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/showmino/forge/internal/store"
)

// openStore returns the service catalog's and audit log's store: PostgreSQL at
// url, migrated. Without a url it is memory only when inMemory says so
// (development: everything is lost when forge-api stops); otherwise forge-api
// does not start. The database may start after forge-api (k3s),
// so connecting is retried for about a minute (10 attempts, waiting 1.5s longer each time),
// except for an invalid URL or a cancelled ctx, which fail at once. Never log url: it holds the
// password.
func openStore(ctx context.Context, url string, inMemory bool, log *slog.Logger) (store.Backend, error) {
	if url == "" {
		if !inMemory {
			return nil, errors.New("no database: set --database-url (or FORGE_DATABASE_URL) to the PostgreSQL Forge keeps its data in; " +
				"--in-memory (FORGE_IN_MEMORY=1) runs without one, for development only: everything is lost when forge-api stops")
		}
		log.Warn("--in-memory: the service catalog, audit log and sessions are lost when forge-api stops")
		return store.NewMemory(), nil
	}
	if inMemory {
		return nil, errors.New("--in-memory and --database-url are both set: pick one")
	}
	var (
		pg  *store.Postgres
		err error
	)
	for attempt := 1; ; attempt++ {
		pg, err = store.OpenPostgres(ctx, url)
		var bad *pgconn.ParseConfigError
		if err == nil || attempt == 10 || errors.As(err, &bad) {
			break
		}
		log.Info("waiting for the database", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("open the database: %w", ctx.Err())
		case <-time.After(time.Duration(attempt) * 1500 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		return nil, fmt.Errorf("migrate the database: %w", err)
	}
	log.Info("service catalog in PostgreSQL")
	return pg, nil
}
