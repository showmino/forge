package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const approvalColumns = `id, service, environment, commit_sha, run_id, run_url, requested_by, triggered_by, state,
	decided_by, reason, dispatch_error, created_at, decided_at, claimed_at`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var decided, claimed *time.Time
	err := row.Scan(&a.ID, &a.Service, &a.Environment, &a.Commit, &a.RunID, &a.RunURL, &a.RequestedBy, &a.TriggeredBy, &a.State,
		&a.DecidedBy, &a.Reason, &a.DispatchError, &a.CreatedAt, &decided, &claimed)
	if decided != nil {
		a.DecidedAt = *decided
	}
	if claimed != nil {
		a.ClaimedAt = *claimed
	}
	return a, err
}

func (p *Postgres) CreateApproval(ctx context.Context, a Approval) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// One request of a service and environment at a time: two at once
		// would each supersede the older rows but not each other's.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('approvals:' || $1 || '/' || $2))", a.Service, a.Environment); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $3
			WHERE service = $1 AND environment = $2 AND state IN ($4, $5)`,
			a.Service, a.Environment, ApprovalSuperseded, ApprovalPending, ApprovalApproved); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO approvals (id, service, environment, commit_sha, run_id, run_url,
			requested_by, triggered_by, state, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			a.ID, a.Service, a.Environment, a.Commit, a.RunID, a.RunURL, a.RequestedBy, a.TriggeredBy, a.State, a.CreatedAt)
		return err
	})
}

func (p *Postgres) GetApproval(ctx context.Context, id string) (Approval, error) {
	a, err := scanApproval(p.pool.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

func (p *Postgres) ListApprovals(ctx context.Context, q ApprovalQuery) ([]Approval, error) {
	rows, err := p.pool.Query(ctx, "SELECT "+approvalColumns+` FROM approvals
		WHERE ($1 = '' OR service = $1) AND ($2 = '' OR state = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3`, q.Service, q.State, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// move changes the approval id from state from to the rest of the update,
// reporting ErrNotPending when it is in another state.
func (p *Postgres) move(ctx context.Context, id, from, set string, args ...any) (Approval, error) {
	a, err := scanApproval(p.pool.QueryRow(ctx, "UPDATE approvals SET "+set+
		" WHERE id = $1 AND state = $2 RETURNING "+approvalColumns, append([]any{id, from}, args...)...))
	if !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	cur, err := p.GetApproval(ctx, id)
	if err != nil {
		return Approval{}, err
	}
	return cur, ErrNotPending
}

func (p *Postgres) DecideApproval(ctx context.Context, id, state, by, reason string, at time.Time) (Approval, error) {
	return p.move(ctx, id, ApprovalPending, "state = $3, decided_by = $4, reason = $5, decided_at = $6", state, by, reason, at)
}

func (p *Postgres) SetDispatchError(ctx context.Context, id, msg string) error {
	_, err := p.pool.Exec(ctx, "UPDATE approvals SET dispatch_error = $2 WHERE id = $1", id, msg)
	return err
}

func (p *Postgres) ClaimApproval(ctx context.Context, id string, at time.Time) (Approval, error) {
	return p.move(ctx, id, ApprovalApproved, "state = $3, claimed_at = $4", ApprovalClaimed, at)
}
