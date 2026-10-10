package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

const auditColumns = `id, time, kind, action, actor_kind, actor_subject, actor_email, actor_repository,
	service, target, result_code, result_message, operation, details, client_version, request_id`

func (p *Postgres) AppendAuditEvent(ctx context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error) {
	t := time.Now()
	if ev.GetTime() != nil {
		t = ev.GetTime().AsTime()
	}
	details := ev.GetDetails()
	if details == nil {
		details = map[string]string{}
	}
	d, err := json.Marshal(details)
	if err != nil {
		return nil, err
	}
	a, r, c := ev.GetActor(), ev.GetResult(), ev.GetClient()
	row := p.pool.QueryRow(ctx, `INSERT INTO audit_events (time, kind, action, actor_kind, actor_subject, actor_email,
			actor_repository, service, target, result_code, result_message, operation, details, client_version, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING `+auditColumns,
		t, ev.GetKind(), ev.GetAction(), a.GetKind(), a.GetSubject(), a.GetEmail(), a.GetRepository(),
		ev.GetService(), ev.GetTarget(), r.GetCode(), r.GetMessage(), ev.GetOperation(), d, c.GetVersion(), c.GetRequestId())
	return scanAuditEvent(row)
}

func (p *Postgres) GetAuditEvent(ctx context.Context, id int64) (*forgev1.AuditEvent, error) {
	e, err := scanAuditEvent(p.pool.QueryRow(ctx, "SELECT "+auditColumns+" FROM audit_events WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (p *Postgres) ListAuditEvents(ctx context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Service != "" {
		add("service = $%d", q.Service)
	}
	if q.Actor != "" {
		add("(actor_email = $%[1]d OR actor_repository = $%[1]d OR actor_kind = $%[1]d)", q.Actor)
	}
	if q.Action != "" {
		add("action = $%d", q.Action)
	}
	if q.FailedOnly {
		where = append(where, "result_code <> 'ok'")
	}
	if !q.Since.IsZero() {
		add("time >= $%d", q.Since)
	}
	if !q.Until.IsZero() {
		add("time < $%d", q.Until)
	}
	if q.BeforeID > 0 {
		add("id < $%d", q.BeforeID)
	}
	args = append(args, q.Limit)
	rows, err := p.pool.Query(ctx, fmt.Sprintf("SELECT %s FROM audit_events WHERE %s ORDER BY id DESC LIMIT $%d",
		auditColumns, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*forgev1.AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanAuditEvent(row pgx.Row) (*forgev1.AuditEvent, error) {
	var (
		id                                    int64
		t                                     time.Time
		kind, action, akind, asub, aemail     string
		arepo, service, target, code, message string
		operation, version, requestID         string
		details                               []byte
	)
	if err := row.Scan(&id, &t, &kind, &action, &akind, &asub, &aemail, &arepo,
		&service, &target, &code, &message, &operation, &details, &version, &requestID); err != nil {
		return nil, err
	}
	e := &forgev1.AuditEvent{
		Name: AuditEventName(id), Time: timestamppb.New(t), Kind: kind, Action: action,
		Actor:   &forgev1.AuditActor{Kind: akind, Subject: asub, Email: aemail, Repository: arepo},
		Service: service, Target: target,
		Result:    &forgev1.AuditResult{Code: code, Message: message},
		Operation: operation,
		Client:    &forgev1.AuditClient{Version: version, RequestId: requestID},
	}
	if err := json.Unmarshal(details, &e.Details); err != nil {
		return nil, fmt.Errorf("decode audit details: %w", err)
	}
	if len(e.Details) == 0 {
		e.Details = nil
	}
	return e, nil
}
