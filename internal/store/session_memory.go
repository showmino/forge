package store

import (
	"cmp"
	"context"
	"slices"
	"time"
)

func cloneSession(s Session) Session {
	s.Groups = slices.Clone(s.Groups)
	return s
}

func (m *Memory) CreateSession(_ context.Context, s Session) error {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]Session{}
	}
	m.sessions[s.ID] = cloneSession(s)
	return nil
}

func (m *Memory) SessionByToken(_ context.Context, tokenHash string) (Session, error) {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	for _, s := range m.sessions {
		if s.TokenHash == tokenHash {
			return cloneSession(s), nil
		}
	}
	return Session{}, ErrNotFound
}

func (m *Memory) TouchSession(_ context.Context, id string, lastUsed, expires time.Time) error {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.LastUsedAt, s.ExpiresAt = lastUsed, expires
		m.sessions[id] = s
	}
	return nil
}

func (m *Memory) SetSessionGroups(_ context.Context, id string, groups []string, checked time.Time) error {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Groups, s.CheckedAt = slices.Clone(groups), checked
		m.sessions[id] = s
	}
	return nil
}

func (m *Memory) ListSessions(_ context.Context, subject string) ([]Session, error) {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	var out []Session
	for _, s := range m.sessions {
		if s.Subject == subject {
			out = append(out, cloneSession(s))
		}
	}
	slices.SortFunc(out, func(a, b Session) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (m *Memory) DeleteSession(_ context.Context, subject, id string) error {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	if s, ok := m.sessions[id]; !ok || s.Subject != subject {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

func (m *Memory) DeleteExpiredSessions(_ context.Context, now time.Time) (int, error) {
	m.sesMu.Lock()
	defer m.sesMu.Unlock()
	n := 0
	for id, s := range m.sessions {
		if !now.Before(s.ExpiresAt) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}
