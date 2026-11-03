package store

import (
	"context"
	"slices"
	"time"
)

func (m *Memory) CreateApproval(_ context.Context, a Approval) error {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	if m.approvals == nil {
		m.approvals = map[string]Approval{}
	}
	for id, o := range m.approvals {
		if o.Service == a.Service && o.Environment == a.Environment && o.State == ApprovalPending {
			o.State = ApprovalSuperseded
			m.approvals[id] = o
		}
	}
	m.approvals[a.ID] = a
	m.aprOrder = append(m.aprOrder, a.ID)
	return nil
}

func (m *Memory) GetApproval(_ context.Context, id string) (Approval, error) {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

func (m *Memory) ListApprovals(_ context.Context, q ApprovalQuery) ([]Approval, error) {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	var out []Approval
	for _, id := range slices.Backward(m.aprOrder) {
		a := m.approvals[id]
		if (q.Service == "" || a.Service == q.Service) && (q.State == "" || a.State == q.State) {
			out = append(out, a)
			if len(out) == q.Limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) DecideApproval(_ context.Context, id, state, by, reason string, at time.Time) (Approval, error) {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	a, ok := m.approvals[id]
	switch {
	case !ok:
		return Approval{}, ErrNotFound
	case a.State != ApprovalPending:
		return a, ErrNotPending
	}
	a.State, a.DecidedBy, a.Reason, a.DecidedAt = state, by, reason, at
	m.approvals[id] = a
	return a, nil
}

func (m *Memory) SetDispatchError(_ context.Context, id, msg string) error {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	if a, ok := m.approvals[id]; ok {
		a.DispatchError = msg
		m.approvals[id] = a
	}
	return nil
}

func (m *Memory) ClaimApproval(_ context.Context, id string, at time.Time) (Approval, error) {
	m.aprMu.Lock()
	defer m.aprMu.Unlock()
	a, ok := m.approvals[id]
	switch {
	case !ok:
		return Approval{}, ErrNotFound
	case a.State != ApprovalApproved:
		return a, ErrNotPending
	}
	a.State, a.ClaimedAt = ApprovalClaimed, at
	m.approvals[id] = a
	return a, nil
}
