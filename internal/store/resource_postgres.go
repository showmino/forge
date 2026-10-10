package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

const planColumns = `seq, data, creator_subject, digest, state_version, token_hash, deadline, plan_file, plan_json, audit`

// listColumns is planColumns without the plan files and the audit template,
// which can be megabytes each.
const listColumns = `seq, data, creator_subject, digest, state_version, token_hash, deadline`

// scanPlan reads a row of planColumns; with files false, of listColumns.
func scanPlan(row pgx.Row, files bool) (*PlanRecord, error) {
	var (
		p        PlanRecord
		data     []byte
		deadline *time.Time
	)
	dest := []any{&p.Seq, &data, &p.CreatorSubject, &p.Digest, &p.StateVersion, &p.TokenHash, &deadline}
	if files {
		dest = append(dest, &p.PlanFile, &p.PlanJSON, &p.Audit)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	p.Plan = &forgev1.ResourcePlan{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, p.Plan); err != nil {
		return nil, fmt.Errorf("decode a stored resource plan: %w", err)
	}
	if deadline != nil {
		p.Deadline = *deadline
	}
	return &p, nil
}

func getPlan(ctx context.Context, q querier, where string, arg any) (*PlanRecord, error) {
	p, err := scanPlan(q.QueryRow(ctx, "SELECT "+planColumns+" FROM resource_plans WHERE "+where, arg), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func decodeResource(data []byte) (*forgev1.Resource, error) {
	r := &forgev1.Resource{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("decode a stored resource: %w", err)
	}
	return r, nil
}

func getResource(ctx context.Context, q querier, name string) (*forgev1.Resource, error) {
	var data []byte
	err := q.QueryRow(ctx, "SELECT data FROM resources WHERE name = $1", name).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeResource(data)
}

func queryResources(ctx context.Context, q querier, service, env string) ([]*forgev1.Resource, error) {
	rows, err := q.Query(ctx, `SELECT data FROM resources
		WHERE ($1::text = '' OR service = $1) AND ($2::text = '' OR env = $2) ORDER BY name`, service, env)
	if err != nil {
		return nil, err
	}
	data, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.Resource, 0, len(data))
	for _, d := range data {
		r, err := decodeResource(d)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (p *Postgres) GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error) {
	return getPlan(ctx, p.pool, "name = $1", name)
}

func (p *Postgres) ResourcePlanByToken(ctx context.Context, tokenHash string) (*PlanRecord, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	return getPlan(ctx, p.pool, "token_hash = $1", tokenHash)
}

func (p *Postgres) ListResourcePlans(ctx context.Context, q PlanQuery) ([]*PlanRecord, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Service != "" {
		add("service = $%d", q.Service)
	}
	if q.Env != "" {
		add("env = $%d", q.Env)
	}
	if len(q.States) > 0 {
		states := make([]string, len(q.States))
		for i, s := range q.States {
			states[i] = s.String()
		}
		add("state = ANY($%d)", states)
	}
	if q.CreatorSubject != "" {
		add("creator_subject = $%d", q.CreatorSubject)
	}
	if q.BeforeSeq > 0 {
		add("seq < $%d", q.BeforeSeq)
	}
	args = append(args, q.Limit)
	rows, err := p.pool.Query(ctx, fmt.Sprintf("SELECT %s FROM resource_plans WHERE %s ORDER BY seq DESC LIMIT $%d",
		listColumns, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlanRecord
	for rows.Next() {
		rec, err := scanPlan(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (p *Postgres) GetResource(ctx context.Context, name string) (*forgev1.Resource, error) {
	return getResource(ctx, p.pool, name)
}

func (p *Postgres) ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error) {
	return queryResources(ctx, p.pool, service, env)
}

// WriteResources runs fn in a transaction that first takes a transaction-scoped
// advisory lock, so these transactions run one at a time.
func (p *Postgres) WriteResources(ctx context.Context, fn func(tx ResourceTx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", resourceLock); err != nil {
		return err
	}
	if err := fn(pgResourceTx{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type pgResourceTx struct{ tx pgx.Tx }

func (t pgResourceTx) GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error) {
	return getPlan(ctx, t.tx, "name = $1", name)
}

func (t pgResourceTx) PutResourcePlan(ctx context.Context, p *PlanRecord) error {
	data, err := protojson.Marshal(p.Plan)
	if err != nil {
		return err
	}
	var deadline *time.Time
	if !p.Deadline.IsZero() {
		deadline = &p.Deadline
	}
	return t.tx.QueryRow(ctx, `INSERT INTO resource_plans (name, service, env, state, creator_subject, data, digest,
			state_version, token_hash, deadline, plan_file, plan_json, audit)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (name) DO UPDATE SET service = EXCLUDED.service, env = EXCLUDED.env, state = EXCLUDED.state,
			creator_subject = EXCLUDED.creator_subject, data = EXCLUDED.data, digest = EXCLUDED.digest,
			state_version = EXCLUDED.state_version, token_hash = EXCLUDED.token_hash, deadline = EXCLUDED.deadline,
			plan_file = EXCLUDED.plan_file, plan_json = EXCLUDED.plan_json, audit = EXCLUDED.audit
		RETURNING seq`,
		p.Plan.GetName(), p.Plan.GetService(), p.Plan.GetEnv(), p.Plan.GetState().String(), p.CreatorSubject, data, p.Digest,
		p.StateVersion, p.TokenHash, deadline, p.PlanFile, p.PlanJSON, p.Audit).Scan(&p.Seq)
}

func (t pgResourceTx) GetResource(ctx context.Context, name string) (*forgev1.Resource, error) {
	return getResource(ctx, t.tx, name)
}

func (t pgResourceTx) PutResource(ctx context.Context, r *forgev1.Resource) error {
	data, err := protojson.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO resources (name, service, env, data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET service = EXCLUDED.service, env = EXCLUDED.env, data = EXCLUDED.data`,
		r.GetName(), r.GetService(), r.GetEnv(), data)
	return err
}

func (t pgResourceTx) DeleteResource(ctx context.Context, name string) error {
	_, err := t.tx.Exec(ctx, "DELETE FROM resources WHERE name = $1", name)
	return err
}

func (t pgResourceTx) ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error) {
	return queryResources(ctx, t.tx, service, env)
}

func (p *Postgres) GetResourceState(ctx context.Context, key string) (ResourceState, error) {
	var st ResourceState
	err := p.pool.QueryRow(ctx, "SELECT data, version, lock_id, lock_info FROM resource_states WHERE key = $1", key).
		Scan(&st.Data, &st.Version, &st.LockID, &st.LockInfo)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResourceState{}, nil
	}
	return st, err
}

func (p *Postgres) PutResourceState(ctx context.Context, key, lockID string, data []byte) (int64, error) {
	var version int64
	// One statement: the lock condition is checked by the same row lock that
	// writes, also when another session inserts the row first.
	err := p.pool.QueryRow(ctx, `INSERT INTO resource_states (key, data, version) VALUES ($1, $2, 1)
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, version = resource_states.version + 1
		WHERE resource_states.lock_id = '' OR resource_states.lock_id = $3
		RETURNING version`, key, data, lockID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrStateLocked // the row exists and another id holds its lock
	}
	return version, err
}

func (p *Postgres) LockResourceState(ctx context.Context, key, lockID string, info []byte) ([]byte, error) {
	if lockID == "" {
		return nil, errors.New("a state lock needs an id")
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO resource_states (key, lock_id, lock_info) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET lock_id = EXCLUDED.lock_id, lock_info = EXCLUDED.lock_info
		WHERE resource_states.lock_id = '' OR resource_states.lock_id = EXCLUDED.lock_id`, key, lockID, info)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var holder []byte
	if err := p.pool.QueryRow(ctx, "SELECT lock_info FROM resource_states WHERE key = $1", key).Scan(&holder); err != nil {
		return nil, err
	}
	return holder, ErrStateLocked
}

func (p *Postgres) UnlockResourceState(ctx context.Context, key, lockID string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE resource_states SET lock_id = '', lock_info = NULL
		WHERE key = $1 AND ($2 = '' OR lock_id = '' OR lock_id = $2)`, key, lockID)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM resource_states WHERE key = $1)", key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrStateLocked
	}
	return nil
}
