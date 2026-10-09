package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Advisory lock keys: arbitrary, fixed, and Forge's own.
const (
	migrateLock int64 = 0x666f726765_01 // "forge", 1
	writeLock   int64 = 0x666f726765_02 // "forge", 2
)

// Postgres is a Store in PostgreSQL.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects to the database at url and checks the connection.
// It does not migrate; call Migrate.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database URL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Migrate applies the embedded migrations not yet applied, in order. A
// session advisory lock keeps two processes from migrating at once.
func (p *Postgres) Migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS forge_migrations (
		version    integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, f := range files {
		version, err := strconv.Atoi(strings.SplitN(path.Base(f), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", f)
		}
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM forge_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO forge_migrations (version) VALUES ($1)", version)
			return err
		}); err != nil {
			return fmt.Errorf("migration %s: %w", path.Base(f), err)
		}
	}
	return nil
}

// querier is what pgxpool.Pool and pgx.Tx have in common.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p *Postgres) Get(ctx context.Context, name string) (*forgev1.Service, int64, error) {
	return getService(ctx, p.pool, name)
}

func (p *Postgres) List(ctx context.Context, q ListQuery) ([]*forgev1.Service, error) {
	rows, err := p.pool.Query(ctx, `SELECT data FROM services
		WHERE name > $1 AND ($2::text = '' OR owner_team = $2)
		ORDER BY name LIMIT $3`, q.After, q.OwnerTeam, q.Limit)
	if err != nil {
		return nil, err
	}
	data, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.Service, 0, len(data))
	for _, d := range data {
		s, err := decode(d)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (p *Postgres) Names(ctx context.Context) ([]string, error) {
	return names(ctx, p.pool, "SELECT name FROM services ORDER BY name")
}

func (p *Postgres) ForRepo(ctx context.Context, owner, repo string) ([]string, error) {
	return names(ctx, p.pool, "SELECT name FROM services WHERE repo = $1 ORDER BY name", strings.ToLower(owner+"/"+repo))
}

// Write runs fn in a transaction that first takes a transaction-scoped
// advisory lock, so write transactions run one at a time.
func (p *Postgres) Write(ctx context.Context, fn func(tx Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", writeLock); err != nil {
		return err
	}
	if err := fn(pgTx{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Close() { p.pool.Close() }

type pgTx struct{ tx pgx.Tx }

func (t pgTx) Get(ctx context.Context, name string) (*forgev1.Service, int64, error) {
	return getService(ctx, t.tx, name)
}

func (t pgTx) Put(ctx context.Context, svc *forgev1.Service, revision int64) error {
	data, err := protojson.Marshal(svc)
	if err != nil {
		return err
	}
	var repo *string
	if k := RepoKey(svc.GetRepoUrl()); k != "" {
		repo = &k
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO services (name, revision, repo, owner_team, data, create_time, update_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET revision = EXCLUDED.revision, repo = EXCLUDED.repo,
			owner_team = EXCLUDED.owner_team, data = EXCLUDED.data, update_time = EXCLUDED.update_time`,
		svc.GetName(), revision, repo, svc.GetOwnerTeam(), data, svc.GetCreateTime().AsTime(), svc.GetUpdateTime().AsTime())
	return err
}

func (t pgTx) Delete(ctx context.Context, name string) error {
	_, err := t.tx.Exec(ctx, "DELETE FROM services WHERE name = $1", name)
	return err
}

func (t pgTx) Dependents(ctx context.Context, name string) ([]string, error) {
	return names(ctx, t.tx, `SELECT name FROM services
		WHERE name <> $1 AND data->'dependencies' ? $1 ORDER BY name`, name)
}

func (t pgTx) Recall(ctx context.Context, requestID string) (*forgev1.Service, error) {
	var data []byte
	err := t.tx.QueryRow(ctx, `SELECT service FROM service_requests
		WHERE request_id = $1 AND created_at > now() - interval '24 hours'`, requestID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(data)
}

func (t pgTx) Remember(ctx context.Context, requestID string, svc *forgev1.Service) error {
	data, err := protojson.Marshal(svc)
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, "DELETE FROM service_requests WHERE created_at < now() - interval '24 hours'"); err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO service_requests (request_id, service) VALUES ($1, $2)
		ON CONFLICT (request_id) DO UPDATE SET service = EXCLUDED.service, created_at = now()`, requestID, data)
	return err
}

func getService(ctx context.Context, q querier, name string) (*forgev1.Service, int64, error) {
	var (
		data []byte
		rev  int64
	)
	err := q.QueryRow(ctx, "SELECT data, revision FROM services WHERE name = $1", name).Scan(&data, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	s, err := decode(data)
	return s, rev, err
}

func names(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func decode(data []byte) (*forgev1.Service, error) {
	s := &forgev1.Service{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("decode a stored service: %w", err)
	}
	return s, nil
}
