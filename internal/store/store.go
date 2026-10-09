// Package store keeps the service catalog: in memory for development and
// tests, or in PostgreSQL.
package store

import (
	"context"
	"errors"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// ErrNotFound reports a service name or request ID the store does not hold.
var ErrNotFound = errors.New("not found")

// Store holds catalog services by resource name ("services/<id>"), each with
// a revision. Services are copied in and out: callers never share them with
// the store.
type Store interface {
	// Get returns the service and its revision, or ErrNotFound.
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	// List returns up to q.Limit services, sorted by name.
	List(ctx context.Context, q ListQuery) ([]*forgev1.Service, error)
	// Names returns every service name, sorted.
	Names(ctx context.Context) ([]string, error)
	// ForRepo returns the sorted names of the services whose repo_url is
	// https://github.com/<owner>/<repo>, compared case-insensitively.
	ForRepo(ctx context.Context, owner, repo string) ([]string, error)
	// Write runs fn in a transaction; write transactions never overlap. What
	// fn writes through tx takes effect only if fn returns nil. fn's error is
	// returned unchanged. fn must use only tx: calling the Store itself from
	// inside fn deadlocks the memory store and can exhaust the PostgreSQL pool.
	Write(ctx context.Context, fn func(tx Tx) error) error
	// Close releases the store's resources.
	Close()
}

// ListQuery selects services for List.
type ListQuery struct {
	OwnerTeam string // "" means every team
	After     string // only names greater than this
	Limit     int    // at most this many; must be positive
}

// Tx is a write transaction. Its reads see its own writes.
type Tx interface {
	// Get returns the service and its revision, or ErrNotFound.
	Get(ctx context.Context, name string) (*forgev1.Service, int64, error)
	// Put stores svc under svc.Name with revision, replacing any service there.
	Put(ctx context.Context, svc *forgev1.Service, revision int64) error
	// Delete removes the service called name, if there is one.
	Delete(ctx context.Context, name string) error
	// Dependents returns the sorted names of the other services that list
	// name among their dependencies.
	Dependents(ctx context.Context, name string) ([]string, error)
	// Recall returns the service a RegisterService with requestID returned,
	// or ErrNotFound.
	Recall(ctx context.Context, requestID string) (*forgev1.Service, error)
	// Remember records svc as the result of requestID.
	Remember(ctx context.Context, requestID string, svc *forgev1.Service) error
}

// RepoKey returns "<owner>/<repo>", lowercased, for a repo_url of the form
// https://github.com/<owner>/<repo> (".git" and a trailing "/" allowed), and
// "" for any other URL.
func RepoKey(repoURL string) string {
	rest, ok := strings.CutPrefix(repoURL, "https://github.com/")
	if !ok {
		return ""
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return ""
	}
	return strings.ToLower(owner + "/" + repo)
}
