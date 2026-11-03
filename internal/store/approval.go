package store

import (
	"context"
	"errors"
	"time"
)

// Approval states.
const (
	ApprovalPending    = "pending"
	ApprovalApproved   = "approved"
	ApprovalRejected   = "rejected"
	ApprovalSuperseded = "superseded"
	ApprovalClaimed    = "claimed"
)

// ErrNotPending is a decision or claim on an approval that is past it: a
// decision on one no longer pending, a claim on one not approved.
var ErrNotPending = errors.New("not in the state this needs")

// Approval is one production run of an infrastructure repo waiting for, or
// given, its approval (phase 12).
type Approval struct {
	// ID is "apr_<hex>".
	ID string
	// Service is the catalog id, without "services/".
	Service     string
	Environment string
	Commit      string
	RunID       int64
	RunURL      string
	// RequestedBy is the GitHub user who started the run (its actor), and
	// TriggeredBy who started the attempt that asked (triggering_actor).
	// Neither may approve it.
	RequestedBy, TriggeredBy string
	State                    string
	// DecidedBy is the GitHub user who approved or rejected it.
	DecidedBy, Reason string
	// DispatchError is why the production run could not be started.
	DispatchError string

	CreatedAt time.Time
	// DecidedAt and ClaimedAt are zero until then.
	DecidedAt, ClaimedAt time.Time
}

// ApprovalQuery selects approvals; empty fields select all.
type ApprovalQuery struct {
	Service string
	State   string
	// Limit is at most this many, newest first; it must be positive.
	Limit int
}

// ApprovalStore keeps approvals.
type ApprovalStore interface {
	// CreateApproval stores a, a pending approval, and marks every other
	// approval of the same service and environment that is pending, or
	// approved but not claimed, superseded, in one step: only the newest
	// commit can deploy.
	CreateApproval(ctx context.Context, a Approval) error
	// GetApproval returns the approval id, or ErrNotFound.
	GetApproval(ctx context.Context, id string) (Approval, error)
	// ListApprovals returns the approvals q selects, newest first.
	ListApprovals(ctx context.Context, q ApprovalQuery) ([]Approval, error)
	// DecideApproval moves a pending approval to state (approved or
	// rejected), by whom, why and when. ErrNotPending when it is not
	// pending, ErrNotFound when there is none.
	DecideApproval(ctx context.Context, id, state, by, reason string, at time.Time) (Approval, error)
	// SetDispatchError records why the production run could not be started
	// ("" once it was).
	SetDispatchError(ctx context.Context, id, msg string) error
	// ClaimApproval moves an approved approval to claimed, once.
	// ErrNotPending when it is not approved.
	ClaimApproval(ctx context.Context, id string, at time.Time) (Approval, error)
}
