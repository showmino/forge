// Package run holds what forge-api and a resource runner share: how a run
// is started and watched (Launcher), and what a runner may ask of forge-api
// with its one-time token (API).
package run

import (
	"context"
	"errors"
	"time"
)

// Modes of a run.
const (
	ModePlan  = "plan"
	ModeApply = "apply"
)

// ErrLocked reports an OpenTofu state locked under another id.
var ErrLocked = errors.New("the state is locked")

// Spec is what a Launcher needs to start a runner.
type Spec struct {
	// Plan is the plan's name, "resourcePlans/<id>".
	Plan string
	// Mode is ModePlan or ModeApply.
	Mode string
	// Service is the service id, without "services/".
	Service string
	Env     string
	// Token authenticates the runner to the API. Never log it.
	Token    string
	Deadline time.Time
}

// Status is how a started runner is doing.
type Status struct {
	// Done is true once the runner's process has ended.
	Done bool
	// Failed is true when it ended without being able to report a result.
	Failed bool
	// Log is the end of its output, when it failed.
	Log string
}

// Launcher starts and watches runners (Kubernetes Jobs in production).
type Launcher interface {
	// Start starts a runner for s and returns once it is started. When it
	// fails it leaves nothing behind: no runner, and no copy of the token.
	// Its error and Status.Log are shown to users and stored: they must never
	// hold the token.
	Start(ctx context.Context, s Spec) error
	// Status reports on the runner started for plan and mode. Log is short:
	// the last lines of the runner's output.
	Status(ctx context.Context, plan, mode string) (Status, error)
	// Stop ends that runner and returns only once it has exited, or failed
	// to within the launcher's own limit. The runner gets time to stop
	// OpenTofu cleanly, and its token still works meanwhile, so the state of
	// what an interrupted apply made is saved. Stopping a runner that is
	// gone is not an error.
	Stop(ctx context.Context, plan, mode string) error
}

// Resource is one resource the module should make.
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}

// Variables are the OpenTofu module's input.
type Variables struct {
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	Namespace   string     `json:"namespace"`
	Resources   []Resource `json:"resources"`
}

// Work is what a runner is asked to do.
type Work struct {
	Plan      string    `json:"plan"`
	Mode      string    `json:"mode"`
	Variables Variables `json:"variables"`
	// PlanFile is the saved plan to apply; empty for a plan run.
	PlanFile []byte `json:"plan_file,omitempty"`
}

// Result is what a runner hands back.
type Result struct {
	// PlanFile is OpenTofu's saved plan and PlanJSON `tofu show -json` of
	// it; both set by a plan run.
	PlanFile []byte `json:"plan_file,omitempty"`
	PlanJSON []byte `json:"plan_json,omitempty"`
	// Secrets maps the name of every resource that exists after the apply,
	// changed by it or not, to the Secret holding its connection string; set
	// by an apply run.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Error says why the run failed; empty when it worked.
	Error string `json:"error,omitempty"`
}

// API is what a runner calls, authenticated by its run token.
type API interface {
	// Claim returns the run's work.
	Claim(ctx context.Context, token string) (*Work, error)
	// Event adds a progress line to the run's operation.
	Event(ctx context.Context, token, step, message string) error
	// Complete ends the run with r. It works once.
	Complete(ctx context.Context, token string, r Result) error
	// ReadState returns the OpenTofu state, nil when there is none.
	ReadState(ctx context.Context, token string) ([]byte, error)
	// WriteState stores the state; only an apply run may.
	WriteState(ctx context.Context, token, lockID string, data []byte) error
	// LockState locks the state; when it is held by another id it returns
	// the holder's info and ErrLocked.
	LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error)
	// UnlockState releases the lock.
	UnlockState(ctx context.Context, token, lockID string) error
}
