package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func clonePlan(p *PlanRecord) *PlanRecord {
	c := *p
	c.Plan = proto.Clone(p.Plan).(*forgev1.ResourcePlan)
	c.PlanFile, c.PlanJSON, c.Audit = slices.Clone(p.PlanFile), slices.Clone(p.PlanJSON), slices.Clone(p.Audit)
	return &c
}

func cloneResource(r *forgev1.Resource) *forgev1.Resource { return proto.Clone(r).(*forgev1.Resource) }

// listResources returns copies of the matching entries of all, sorted by name.
func listResources(all map[string]*forgev1.Resource, service, env string) []*forgev1.Resource {
	var out []*forgev1.Resource
	for _, r := range all {
		if (service == "" || r.GetService() == service) && (env == "" || r.GetEnv() == env) {
			out = append(out, cloneResource(r))
		}
	}
	slices.SortFunc(out, func(a, b *forgev1.Resource) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out
}

func (m *Memory) GetResourcePlan(_ context.Context, name string) (*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	p, ok := m.plans[name]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePlan(p), nil
}

func (m *Memory) ResourcePlanByToken(_ context.Context, tokenHash string) (*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	if tokenHash != "" {
		for _, p := range m.plans {
			if p.TokenHash == tokenHash {
				return clonePlan(p), nil
			}
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListResourcePlans(_ context.Context, q PlanQuery) ([]*PlanRecord, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	var out []*PlanRecord
	for _, p := range m.plans {
		switch {
		case q.Service != "" && p.Plan.GetService() != q.Service,
			q.Env != "" && p.Plan.GetEnv() != q.Env,
			len(q.States) > 0 && !slices.Contains(q.States, p.Plan.GetState()),
			q.CreatorSubject != "" && p.CreatorSubject != q.CreatorSubject,
			q.BeforeSeq > 0 && p.Seq >= q.BeforeSeq:
			continue
		}
		out = append(out, clonePlan(p))
	}
	slices.SortFunc(out, func(a, b *PlanRecord) int { return int(b.Seq - a.Seq) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *Memory) GetResource(_ context.Context, name string) (*forgev1.Resource, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	r, ok := m.resources[name]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneResource(r), nil
}

func (m *Memory) ListResources(_ context.Context, service, env string) ([]*forgev1.Resource, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	return listResources(m.resources, service, env), nil
}

// WriteResources runs fn on copies of the maps and keeps them only when fn
// returns nil. Entries are never changed in place, so copying the maps is enough.
func (m *Memory) WriteResources(_ context.Context, fn func(tx ResourceTx) error) error {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	tx := &memResourceTx{plans: maps.Clone(m.plans), resources: maps.Clone(m.resources), seq: m.planSeq}
	if err := fn(tx); err != nil {
		return err
	}
	m.plans, m.resources, m.planSeq = tx.plans, tx.resources, tx.seq
	return nil
}

type memResourceTx struct {
	plans     map[string]*PlanRecord
	resources map[string]*forgev1.Resource
	seq       int64
}

func (t *memResourceTx) GetResourcePlan(_ context.Context, name string) (*PlanRecord, error) {
	p, ok := t.plans[name]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePlan(p), nil
}

func (t *memResourceTx) PutResourcePlan(_ context.Context, p *PlanRecord) error {
	if old, ok := t.plans[p.Plan.GetName()]; ok {
		p.Seq = old.Seq
	} else {
		t.seq++
		p.Seq = t.seq
	}
	t.plans[p.Plan.GetName()] = clonePlan(p)
	return nil
}

func (t *memResourceTx) GetResource(_ context.Context, name string) (*forgev1.Resource, error) {
	r, ok := t.resources[name]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneResource(r), nil
}

func (t *memResourceTx) PutResource(_ context.Context, r *forgev1.Resource) error {
	t.resources[r.GetName()] = cloneResource(r)
	return nil
}

func (t *memResourceTx) DeleteResource(_ context.Context, name string) error {
	delete(t.resources, name)
	return nil
}

func (t *memResourceTx) ListResources(_ context.Context, service, env string) ([]*forgev1.Resource, error) {
	return listResources(t.resources, service, env), nil
}

func (m *Memory) GetResourceState(_ context.Context, key string) (ResourceState, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	st.Data, st.LockInfo = slices.Clone(st.Data), slices.Clone(st.LockInfo)
	return st, nil
}

func (m *Memory) PutResourceState(_ context.Context, key, lockID string, data []byte) (int64, error) {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	if st.LockID != "" && st.LockID != lockID {
		return 0, ErrStateLocked
	}
	st.Data = slices.Clone(data)
	st.Version++
	m.states[key] = st
	return st.Version, nil
}

func (m *Memory) LockResourceState(_ context.Context, key, lockID string, info []byte) ([]byte, error) {
	if lockID == "" {
		return nil, errors.New("a state lock needs an id")
	}
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st := m.states[key]
	if st.LockID != "" && st.LockID != lockID {
		return slices.Clone(st.LockInfo), ErrStateLocked
	}
	st.LockID, st.LockInfo = lockID, slices.Clone(info)
	m.states[key] = st
	return nil, nil
}

func (m *Memory) UnlockResourceState(_ context.Context, key, lockID string) error {
	m.resMu.Lock()
	defer m.resMu.Unlock()
	st, ok := m.states[key]
	if !ok {
		return nil
	}
	if lockID != "" && st.LockID != "" && st.LockID != lockID {
		return ErrStateLocked
	}
	st.LockID, st.LockInfo = "", nil
	m.states[key] = st
	return nil
}
