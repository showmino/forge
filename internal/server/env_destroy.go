package server

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// startDestroy starts removing environment id, or returns the destroy that
// is already running for it. It returns nil when there is nothing to
// destroy: no namespace and no running create.
func (e *Environments) startDestroy(ctx context.Context, id, creator string) *forgev1.Operation {
	target := envNamePrefix + id
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, op := range e.o.Operations.List(target, true) {
		if op.GetKind() == kindDestroyEnv {
			return op
		}
	}
	// A destroy may have finished since the caller looked.
	if _, err := e.o.Cluster.Env(ctx, id); errors.Is(err, errNoEnv) && !e.creating(id) {
		return nil
	}
	return e.o.Operations.Start(ctx, kindDestroyEnv, target, creator, e.destroyWork(id))
}

// creating reports whether a create of environment id is running.
func (e *Environments) creating(id string) bool {
	for _, op := range e.o.Operations.List(envNamePrefix+id, true) {
		if op.GetKind() == kindCreateEnv {
			return true
		}
	}
	return false
}

// destroyWork cancels a running create of the environment, then deletes its
// namespace and waits until it is gone.
func (e *Environments) destroyWork(id string) Work {
	return func(ctx context.Context, emit func(string, string)) error {
		for _, op := range e.o.Operations.List(envNamePrefix+id, true) {
			if op.GetKind() != kindCreateEnv {
				continue
			}
			emit("cancel", "stopping "+op.GetName())
			e.o.Operations.Cancel(op.GetName())
			if _, err := e.o.Operations.Wait(ctx, op.GetName()); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if err := e.o.Cluster.Annotate(ctx, id, map[string]string{annState: stateDestroying}); err != nil && !errors.Is(err, errNoEnv) {
			return err
		}
		if err := e.removeNamespace(ctx, id); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &OpError{Code: "destroy_failed", Message: err.Error()}
		}
		emit("destroy", "removed "+envNamespace(id))
		return nil
	}
}

// expiry is when ns's environment expires: its expire-time annotation, or
// creation plus the longest TTL when that is missing or unreadable.
func expiry(ns *corev1.Namespace) time.Time {
	if t, err := time.Parse(time.RFC3339, ns.Annotations[annExpire]); err == nil {
		return t
	}
	return ns.CreationTimestamp.Add(maxTTL)
}

// Reap starts a destroy for every expired environment.
func (e *Environments) Reap(ctx context.Context) error {
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return err
	}
	now := e.o.Now()
	for i := range nss {
		ns := &nss[i]
		if expiry(ns).After(now) {
			continue
		}
		id := ns.Labels[labelEnvironment]
		op := e.startDestroy(context.Background(), id, "forge-reaper")
		if op == nil {
			continue
		}
		e.o.Log.Info("environment expired", "environment", id, "operation", op.GetName())
	}
	return nil
}

// RunReaper calls Reap every interval until ctx ends.
func (e *Environments) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Reap(ctx); err != nil {
				e.o.Log.Warn("reaper", "error", err)
			}
		}
	}
}
