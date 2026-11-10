package store

import (
	"context"
	"maps"
	"time"
)

func (m *Memory) RuleModes(_ context.Context) (map[string]RuleMode, error) {
	m.ruleMu.Lock()
	defer m.ruleMu.Unlock()
	return maps.Clone(m.rules), nil
}

func (m *Memory) SetRuleMode(_ context.Context, rule, mode, by string, at time.Time) error {
	m.ruleMu.Lock()
	defer m.ruleMu.Unlock()
	if mode == "" {
		delete(m.rules, rule)
		return nil
	}
	if m.rules == nil {
		m.rules = map[string]RuleMode{}
	}
	m.rules[rule] = RuleMode{Mode: mode, UpdatedBy: by, UpdatedAt: at}
	return nil
}
