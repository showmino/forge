package store

import (
	"context"
	"encoding/json"
)

func (p *Postgres) AddRelease(ctx context.Context, r Release) (int, error) {
	parts, err := json.Marshal(r.Parts)
	if err != nil {
		return 0, err
	}
	// The number is the next one for the application. Two writers that
	// pick the same number meet at the primary key, and the second fails.
	var n int
	err = p.pool.QueryRow(ctx, `INSERT INTO application_releases (application, number, created_at, created_by, note, parts)
		SELECT $1, COALESCE(MAX(number), 0) + 1, $2, $3, $4, $5 FROM application_releases WHERE application = $1
		RETURNING number`, r.Application, r.CreatedAt, r.CreatedBy, r.Note, parts).Scan(&n)
	return n, err
}

func (p *Postgres) Releases(ctx context.Context, application string, limit int) ([]Release, error) {
	rows, err := p.pool.Query(ctx, `SELECT number, created_at, created_by, note, parts FROM application_releases
		WHERE application = $1 ORDER BY number DESC LIMIT $2`, application, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r := Release{Application: application}
		var parts []byte
		if err := rows.Scan(&r.Number, &r.CreatedAt, &r.CreatedBy, &r.Note, &parts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(parts, &r.Parts); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
