package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// What a CI workload may do inside the RPCs the interceptor lets it call.

func workloadDenied(w *Workload, format string, a ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("a CI token from %s may "+format, append([]any{w.Repository}, a...)...))
}

// startRef checks that the caller may deploy service to env and returns the
// ref to deploy: a human's ref, or a CI caller's token commit.
func startRef(ctx context.Context, service, env, ref string) (string, error) {
	w := workloadOf(ctx)
	if w == nil {
		return ref, require(ctx, PermServicesWrite)
	}
	if service != servicePrefix+w.Service || require(ctx, PermDeploy(w.Service, env)) != nil {
		return "", workloadDenied(w, "only deploy %s%s to staging, not %s to %s", servicePrefix, w.Service, service, env)
	}
	return w.SHA, nil
}

// ownDeployment checks a CI caller's read of svc/env: its own staging only.
func ownDeployment(ctx context.Context, svc, env string) error {
	w := workloadOf(ctx)
	if w == nil || (svc == w.Service && env == "staging") {
		return nil
	}
	return workloadDenied(w, "only read %s%s/staging", deployNamePrefix, w.Service)
}

// ownOperation checks a CI caller's read of op: only operations it started.
func ownOperation(ctx context.Context, op *forgev1.Operation) error {
	w := workloadOf(ctx)
	if w == nil {
		return nil
	}
	if p, _ := PrincipalFrom(ctx); op.GetCreator() == p.Subject {
		return nil
	}
	return workloadDenied(w, "only read operations it started")
}

// requestKey scopes a request_id to its caller, so one caller's id never
// returns another caller's operation.
func requestKey(ctx context.Context, id string) string {
	subject := ""
	if p, ok := PrincipalFrom(ctx); ok {
		subject = p.Subject
	}
	return subject + "\x00" + id
}
