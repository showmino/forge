package store

import (
	"context"
	"slices"
)

func (m *Memory) AddRelease(_ context.Context, r Release) (int, error) {
	m.relMu.Lock()
	defer m.relMu.Unlock()
	if m.releases == nil {
		m.releases = map[string][]Release{}
	}
	r.Number = len(m.releases[r.Application]) + 1
	r.Parts = slices.Clone(r.Parts)
	m.releases[r.Application] = append(m.releases[r.Application], r)
	return r.Number, nil
}

func (m *Memory) Releases(_ context.Context, application string, limit int) ([]Release, error) {
	m.relMu.Lock()
	defer m.relMu.Unlock()
	all := m.releases[application]
	var out []Release
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		r := all[i]
		r.Parts = slices.Clone(r.Parts)
		out = append(out, r)
	}
	return out, nil
}
