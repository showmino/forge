package store

import (
	"context"
	"time"
)

// TeamSetting is what Forge keeps about one team of the organization; the
// team itself, its name and its members, are GitHub's (phase 15).
type TeamSetting struct {
	// Oncall names the team's on-call rotation, for example "billing-primary".
	Oncall    string
	UpdatedBy string
	UpdatedAt time.Time
}

// TeamStore keeps the settings of teams, by team slug.
type TeamStore interface {
	// TeamSettings returns every team that has a setting.
	TeamSettings(ctx context.Context) (map[string]TeamSetting, error)
	// SetTeamOncall sets the on-call rotation of team; "" removes it.
	SetTeamOncall(ctx context.Context, team, oncall, by string, at time.Time) error
}
