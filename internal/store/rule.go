package store

import (
	"context"
	"time"
)

// RuleMode is the mode an admin set for one switchable policy rule
// (phase 15): "enforce", "warn" or "off".
type RuleMode struct {
	Mode      string
	UpdatedBy string
	UpdatedAt time.Time
}

// RuleStore keeps the modes admins set, by rule id. A rule with none has
// its default.
type RuleStore interface {
	RuleModes(ctx context.Context) (map[string]RuleMode, error)
	// SetRuleMode sets rule's mode; "" removes it, back to the default.
	SetRuleMode(ctx context.Context, rule, mode, by string, at time.Time) error
}
