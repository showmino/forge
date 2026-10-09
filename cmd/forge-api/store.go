package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/showmino/forge/internal/store"
)

// openStore returns the service catalog's store: PostgreSQL at url, migrated,
// or memory when url is empty. The database may start after forge-api (k3s),
// so connecting is retried for about 45 seconds. Never log url: it holds the
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
		if err == nil || attempt == 10 || ctx.Err() != nil {
			break
		}
		log.Info("waiting for the database", "attempt", attempt, "error", err)
		time.Sleep(time.Duration(attempt) * time.Second)
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
