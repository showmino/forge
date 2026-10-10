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

// openStore returns the service catalog's store: PostgreSQL at url, migrated,
// or memory when url is empty. The database may start after forge-api (k3s),
// so connecting is retried for about a minute (10 attempts, waiting 1.5s longer each time),
// except for an invalid URL or a cancelled ctx, which fail at once. Never log url: it holds the
// password.
func openStore(ctx context.Context, url string, log *slog.Logger) (store.Store, error) {
	if url == "" {
		log.Warn("no --database-url: the service catalog is in memory and is lost when forge-api stops")
		return store.NewMemory(), nil
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
