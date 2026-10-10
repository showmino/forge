package store

import (
	"context"
	"slices"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// Memory is a Store in memory: it is lost when the process ends.
type Memory struct {
	mu       sync.Mutex
	services map[string]memEntry
	requests map[string]*forgev1.Service
}

type memEntry struct {
	svc *forgev1.Service
	rev int64
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{services: map[string]memEntry{}, requests: map[string]*forgev1.Service{}}
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, name string) (*forgev1.Service, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.services[name]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return clone(e.svc), e.rev, nil
}

// List implements Store.
func (m *Memory) List(_ context.Context, q ListQuery) ([]*forgev1.Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for n, e := range m.services {
		if n > q.After && (q.OwnerTeam == "" || e.svc.GetOwnerTeam() == q.OwnerTeam) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	if len(names) > q.Limit {
		names = names[:q.Limit]
	}
	out := make([]*forgev1.Service, 0, len(names))
	for _, n := range names {
		out = append(out, clone(m.services[n].svc))
	}
	return out, nil
}

// Names implements Store.
func (m *Memory) Names(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.services))
	for n := range m.services {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

// ForRepo implements Store.
func (m *Memory) ForRepo(_ context.Context, owner, repo string) ([]string, error) {
	key := strings.ToLower(owner + "/" + repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, e := range m.services {
		if RepoKey(e.svc.GetRepoUrl()) == key {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Write holds the store's lock for fn and applies tx's writes when fn
// succeeds.
func (m *Memory) Write(_ context.Context, fn func(tx Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{m: m, services: map[string]*memEntry{}, requests: map[string]*forgev1.Service{}}
	if err := fn(tx); err != nil {
		return err
	}
	for n, e := range tx.services {
		if e == nil {
			delete(m.services, n)
		} else {
			m.services[n] = *e
		}
	}
	for id, s := range tx.requests {
		m.requests[id] = s
	}
	return nil
}

// Close implements Store. A Memory store holds nothing to release.
func (m *Memory) Close() {}

// memTx stages writes; a nil entry is a delete. The caller holds m.mu.
type memTx struct {
	m        *Memory
	services map[string]*memEntry
	requests map[string]*forgev1.Service
}

func (t *memTx) lookup(name string) (memEntry, bool) {
	if e, staged := t.services[name]; staged {
		if e == nil {
			return memEntry{}, false
		}
		return *e, true
	}
	e, ok := t.m.services[name]
	return e, ok
}

func (t *memTx) Get(_ context.Context, name string) (*forgev1.Service, int64, error) {
	e, ok := t.lookup(name)
	if !ok {
		return nil, 0, ErrNotFound
	}
	return clone(e.svc), e.rev, nil
}

func (t *memTx) Put(_ context.Context, svc *forgev1.Service, revision int64) error {
	t.services[svc.GetName()] = &memEntry{svc: clone(svc), rev: revision}
	return nil
}

func (t *memTx) Delete(_ context.Context, name string) error {
	t.services[name] = nil
	return nil
}

func (t *memTx) Dependents(_ context.Context, name string) ([]string, error) {
	seen := map[string]bool{}
	for n := range t.m.services {
		seen[n] = true
	}
	for n := range t.services {
		seen[n] = true
	}
	var out []string
	for n := range seen {
		if e, ok := t.lookup(n); ok && n != name && slices.Contains(e.svc.GetDependencies(), name) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (t *memTx) Recall(_ context.Context, requestID string) (*forgev1.Service, error) {
	if s, ok := t.requests[requestID]; ok {
		return clone(s), nil
	}
	if s, ok := t.m.requests[requestID]; ok {
		return clone(s), nil
	}
	return nil, ErrNotFound
}

func (t *memTx) Remember(_ context.Context, requestID string, svc *forgev1.Service) error {
	t.requests[requestID] = clone(svc)
	return nil
}

func clone(s *forgev1.Service) *forgev1.Service { return proto.Clone(s).(*forgev1.Service) }
