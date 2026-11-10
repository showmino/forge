package store

import (
	"context"
	"maps"
	"time"
)

func (m *Memory) TeamSettings(_ context.Context) (map[string]TeamSetting, error) {
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	return maps.Clone(m.teams), nil
}

func (m *Memory) SetTeamOncall(_ context.Context, team, oncall, by string, at time.Time) error {
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	if oncall == "" {
		delete(m.teams, team)
		return nil
	}
	if m.teams == nil {
		m.teams = map[string]TeamSetting{}
	}
	m.teams[team] = TeamSetting{Oncall: oncall, UpdatedBy: by, UpdatedAt: at}
	return nil
}
