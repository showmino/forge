package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const sessionColumns = `id, token_hash, kind, label, subject, email, login, user_groups,
	created_at, last_used_at, expires_at, checked_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.TokenHash, &s.Kind, &s.Label, &s.Subject, &s.Email, &s.Login, &s.Groups,
		&s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt, &s.CheckedAt)
	return s, err
}

func (p *Postgres) CreateSession(ctx context.Context, s Session) error {
	groups := s.Groups
	if groups == nil {
		groups = []string{}
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO sessions (`+sessionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		s.ID, s.TokenHash, s.Kind, s.Label, s.Subject, s.Email, s.Login, groups,
		s.CreatedAt, s.LastUsedAt, s.ExpiresAt, s.CheckedAt)
	return err
}

func (p *Postgres) SessionByToken(ctx context.Context, tokenHash string) (Session, error) {
	s, err := scanSession(p.pool.QueryRow(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE token_hash = $1", tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

func (p *Postgres) TouchSession(ctx context.Context, id string, lastUsed, expires time.Time) error {
	_, err := p.pool.Exec(ctx, "UPDATE sessions SET last_used_at = $2, expires_at = $3 WHERE id = $1", id, lastUsed, expires)
	return err
}

func (p *Postgres) SetSessionGroups(ctx context.Context, id string, groups []string, checked time.Time) error {
	if groups == nil {
		groups = []string{}
	}
	_, err := p.pool.Exec(ctx, "UPDATE sessions SET user_groups = $2, checked_at = $3 WHERE id = $1", id, groups, checked)
	return err
}

func (p *Postgres) ListSessions(ctx context.Context, subject string) ([]Session, error) {
	rows, err := p.pool.Query(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE subject = $1 ORDER BY created_at DESC, id", subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteSession(ctx context.Context, subject, id string) error {
	tag, err := p.pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1 AND subject = $2", id, subject)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= $1", now)
	return int(tag.RowsAffected()), err
}
