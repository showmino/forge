package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// auditService reads the audit log for every member (services.read).
type auditService struct{ store store.AuditStore }

func (s *auditService) ListAuditEvents(ctx context.Context, req *connect.Request[forgev1.ListAuditEventsRequest]) (*connect.Response[forgev1.ListAuditEventsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	var before int64
	if t := m.GetPageToken(); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err == nil {
			before, err = strconv.ParseInt(string(b), 10, 64)
		}
		if err != nil || before < 1 {
			return nil, invalid("page_token is malformed")
		}
	}
	q := store.AuditQuery{
		Service: serviceName(m.GetService()), Actor: m.GetActor(), Action: m.GetAction(),
		FailedOnly: m.GetFailedOnly(), BeforeID: before, Limit: size + 1,
	}
	if m.GetSince() != nil {
		q.Since = m.GetSince().AsTime()
	}
	if m.GetUntil() != nil {
		q.Until = m.GetUntil().AsTime()
	}
	evs, err := s.store.ListAuditEvents(ctx, q)
	if err != nil {
		return nil, storeErr(err)
	}
	resp := &forgev1.ListAuditEventsResponse{Events: evs}
	if len(evs) > size {
		resp.Events = evs[:size]
		id, _ := store.ParseAuditEventName(evs[size-1].GetName())
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
	}
	return connect.NewResponse(resp), nil
}

func (s *auditService) GetAuditEvent(ctx context.Context, req *connect.Request[forgev1.GetAuditEventRequest]) (*connect.Response[forgev1.GetAuditEventResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	id, ok := store.ParseAuditEventName(req.Msg.GetName())
	if !ok {
		return nil, invalid(fmt.Sprintf("name %q must look like auditEvents/{id}", req.Msg.GetName()))
	}
	ev, err := s.store.GetAuditEvent(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", store.AuditEventName(id)))
	}
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.GetAuditEventResponse{Event: ev}), nil
}
