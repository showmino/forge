package store

import (
	"context"
	"time"
)

func (p *Postgres) TeamSettings(ctx context.Context) (map[string]TeamSetting, error) {
	rows, err := p.pool.Query(ctx, "SELECT team, oncall, updated_by, updated_at FROM team_settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TeamSetting{}
	for rows.Next() {
		var team string
		var s TeamSetting
		if err := rows.Scan(&team, &s.Oncall, &s.UpdatedBy, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.UpdatedAt = s.UpdatedAt.UTC()
		out[team] = s
	}
	return out, rows.Err()
}

func (p *Postgres) SetTeamOncall(ctx context.Context, team, oncall, by string, at time.Time) error {
	if oncall == "" {
		_, err := p.pool.Exec(ctx, "DELETE FROM team_settings WHERE team = $1", team)
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO team_settings (team, oncall, updated_by, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (team) DO UPDATE SET oncall = $2, updated_by = $3, updated_at = $4`, team, oncall, by, at)
	return err
}
