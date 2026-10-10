package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// AuditQuery selects audit events for ListAuditEvents.
type AuditQuery struct {
	Service    string    // "services/<id>"; "" means any
	Actor      string    // an actor's email, repository or kind; "" means any
	Action     string    // "" means any
	FailedOnly bool      // only events whose result code is not "ok"
	Since      time.Time // zero means no lower bound; inclusive
	Until      time.Time // zero means no upper bound; exclusive
	BeforeID   int64     // > 0: only events with a lower id (the page cursor)
	Limit      int       // at most this many; must be positive
}

// AuditStore keeps the audit log. Events are append-only: nothing changes or
// deletes one.
type AuditStore interface {
	// AppendAuditEvent stores a copy of ev, assigning its Name
	// ("auditEvents/<id>", ids increasing) and, when ev.Time is nil, the
	// current time. It returns the stored event.
	AppendAuditEvent(ctx context.Context, ev *forgev1.AuditEvent) (*forgev1.AuditEvent, error)
	// GetAuditEvent returns the event with id, or ErrNotFound.
	GetAuditEvent(ctx context.Context, id int64) (*forgev1.AuditEvent, error)
	// ListAuditEvents returns up to q.Limit matching events, newest first.
	ListAuditEvents(ctx context.Context, q AuditQuery) ([]*forgev1.AuditEvent, error)
}

// Backend is everything forge-api keeps: the catalog and the audit log.
type Backend interface {
	Store
	AuditStore
}

const auditPrefix = "auditEvents/"

// AuditEventName is the resource name of the event with id.
func AuditEventName(id int64) string { return auditPrefix + strconv.FormatInt(id, 10) }

// ParseAuditEventName accepts "auditEvents/<id>" or "<id>" and returns a
// positive id.
func ParseAuditEventName(name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(name, auditPrefix), 10, 64)
	return id, err == nil && id > 0
}
