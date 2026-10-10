package store

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func (m *Memory) AppendAuditEvent(_ context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error) {
	c := proto.Clone(ev).(*forgev1.AuditEvent)
	if c.Time == nil {
		c.Time = timestamppb.Now()
	}
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	c.Name = AuditEventName(int64(len(m.audit) + 1))
	m.audit = append(m.audit, c)
	return proto.Clone(c).(*forgev1.AuditEvent), nil
}

func (m *Memory) GetAuditEvent(_ context.Context, id int64) (*forgev1.AuditEvent, error) {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	if id < 1 || id > int64(len(m.audit)) {
		return nil, ErrNotFound
	}
	return proto.Clone(m.audit[id-1]).(*forgev1.AuditEvent), nil
}

func (m *Memory) ListAuditEvents(_ context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error) {
	m.auditMu.Lock()
	defer m.auditMu.Unlock()
	var out []*forgev1.AuditEvent
	for i := len(m.audit) - 1; i >= 0 && len(out) < q.Limit; i-- {
		if q.BeforeID > 0 && int64(i+1) >= q.BeforeID {
			continue
		}
		if e := m.audit[i]; auditMatches(e, q) {
			out = append(out, proto.Clone(e).(*forgev1.AuditEvent))
		}
	}
	return out, nil
}

// auditMatches applies q's filters, other than paging, to e.
func auditMatches(e *forgev1.AuditEvent, q AuditQuery) bool {
	a := e.GetActor()
	t := e.GetTime().AsTime()
	switch {
	case q.Service != "" && e.GetService() != q.Service,
		q.Actor != "" && a.GetEmail() != q.Actor && a.GetRepository() != q.Actor && a.GetKind() != q.Actor,
		q.Action != "" && e.GetAction() != q.Action,
		q.FailedOnly && e.GetResult().GetCode() == "ok",
		!q.Since.IsZero() && t.Before(q.Since),
		!q.Until.IsZero() && !t.Before(q.Until):
		return false
	}
	return true
}
