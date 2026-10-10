package store

import (
	"context"
	"errors"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// ErrStateLocked reports an OpenTofu state held by another lock.
var ErrStateLocked = errors.New("the state is locked")

// PlanRecord is a resource plan with what the API never shows: the plan
// files and the active run's token hash.
type PlanRecord struct {
	// Seq is assigned by the store when the plan is first put; a higher Seq is
	// a newer plan.
	Seq int64
	// Plan is the public part. Plan.Name ("resourcePlans/<id>") is the key.
	Plan *forgev1.ResourcePlan
	// CreatorSubject is the creator's token subject, for "my plans".
	CreatorSubject string
	// Digest identifies the settings the plan was made from.
	Digest string
	// StateVersion is the state's version when the plan was made.
	StateVersion int64
	// TokenHash is the SHA-256 of the active run's token, or "".
	TokenHash string
	// Deadline is when the active run must have ended; zero when none.
	Deadline time.Time
	// PlanFile is OpenTofu's saved plan; PlanJSON is `tofu show -json` of it.
	PlanFile []byte
	PlanJSON []byte
	// Audit is the active run's outcome event template, as protojson.
	Audit []byte
}

// PlanQuery selects plans for ListResourcePlans.
type PlanQuery struct {
	Service        string                       // "services/<id>"; "" means any
	Env            string                       // "" means any
	States         []forgev1.ResourcePlan_State // empty means any
	CreatorSubject string                       // "" means any
	BeforeSeq      int64                        // > 0: only plans with a lower Seq
	Limit          int                          // at most this many; must be positive
}

// ResourceState is the OpenTofu state of one service and environment.
type ResourceState struct {
	Data []byte
	// Version counts writes; 0 when nothing was written yet.
	Version  int64
	LockID   string
	LockInfo []byte
}

// ResourceStore keeps resource plans, resource records and OpenTofu state.
// Records are copied in and out: callers never share them with the store.
type ResourceStore interface {
	// GetResourcePlan returns the plan called name, or ErrNotFound.
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	// ResourcePlanByToken returns the plan whose TokenHash is tokenHash, or ErrNotFound.
	ResourcePlanByToken(ctx context.Context, tokenHash string) (*PlanRecord, error)
	// ListResourcePlans returns up to q.Limit matching plans, newest first.
	ListResourcePlans(ctx context.Context, q PlanQuery) ([]*PlanRecord, error)
	// GetResource returns the resource called name, or ErrNotFound.
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	// ListResources returns the resources of service ("services/<id>") and
	// env, sorted by name; "" means any.
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
	// WriteResources runs fn in a transaction; these transactions never
	// overlap. What fn writes takes effect only if fn returns nil. fn's error
	// is returned unchanged. fn must use only tx.
	WriteResources(ctx context.Context, fn func(tx ResourceTx) error) error

	// GetResourceState returns the state under key; the zero value when there is none.
	GetResourceState(ctx context.Context, key string) (ResourceState, error)
	// PutResourceState stores data and returns the new version. It fails with
	// ErrStateLocked when the state is locked under another id.
	PutResourceState(ctx context.Context, key, lockID string, data []byte) (int64, error)
	// LockResourceState locks the state under lockID. When another id holds
	// it, it returns that holder's info and ErrStateLocked.
	LockResourceState(ctx context.Context, key, lockID string, info []byte) ([]byte, error)
	// UnlockResourceState releases the lock held by lockID; "" releases any
	// lock. Another id's lock fails with ErrStateLocked.
	UnlockResourceState(ctx context.Context, key, lockID string) error
}

// ResourceTx is a write transaction on plans and resources. Its reads see
// its own writes.
type ResourceTx interface {
	GetResourcePlan(ctx context.Context, name string) (*PlanRecord, error)
	// PutResourcePlan stores p under p.Plan.Name, replacing any plan there,
	// and sets p.Seq when the plan is new.
	PutResourcePlan(ctx context.Context, p *PlanRecord) error
	GetResource(ctx context.Context, name string) (*forgev1.Resource, error)
	// PutResource stores r under r.Name, replacing any resource there.
	PutResource(ctx context.Context, r *forgev1.Resource) error
	// DeleteResource removes the resource called name, if there is one.
	DeleteResource(ctx context.Context, name string) error
	ListResources(ctx context.Context, service, env string) ([]*forgev1.Resource, error)
}
