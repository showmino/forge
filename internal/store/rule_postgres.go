package store

import (
	"context"
	"time"
)

func (p *Postgres) RuleModes(ctx context.Context) (map[string]RuleMode, error) {
	rows, err := p.pool.Query(ctx, "SELECT rule, mode, updated_by, updated_at FROM rule_modes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuleMode{}
	for rows.Next() {
		var rule string
		var m RuleMode
		if err := rows.Scan(&rule, &m.Mode, &m.UpdatedBy, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.UpdatedAt = m.UpdatedAt.UTC()
		out[rule] = m
	}
	return out, rows.Err()
}

func (p *Postgres) SetRuleMode(ctx context.Context, rule, mode, by string, at time.Time) error {
	if mode == "" {
		_, err := p.pool.Exec(ctx, "DELETE FROM rule_modes WHERE rule = $1", rule)
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO rule_modes (rule, mode, updated_by, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (rule) DO UPDATE SET mode = $2, updated_by = $3, updated_at = $4`, rule, mode, by, at)
	return err
}
