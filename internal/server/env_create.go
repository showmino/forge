package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// envPlan is a checked, resolved create.
type envPlan struct {
	id, serviceID, owner, repo  string
	ref, commit, image, creator string
	port                        int32
	health                      string
	ttl                         time.Duration
}

func (e *Environments) CreateEnvironment(ctx context.Context, req *connect.Request[forgev1.CreateEnvironmentRequest]) (*connect.Response[forgev1.CreateEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	m := req.Msg
	p, err := e.plan(ctx, m)
	if err != nil {
		return nil, err
	}
	preview := &forgev1.Environment{
		Name: envNamePrefix + p.id, Service: servicePrefix + p.serviceID, Ref: p.ref, Commit: p.commit,
		Image: p.image, Url: e.url(p.id), Namespace: envNamespace(p.id), Creator: p.creator,
		State: forgev1.Environment_STATE_CREATING,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if key := m.GetRequestId(); key != "" && !m.GetValidateOnly() {
		if name, ok := e.requests[key]; ok {
			if op, ok := e.o.Operations.Get(name); ok {
				return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Operation: op, Environment: preview}), nil
			}
		}
	}
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return nil, envErr(p.id, err)
	}
	if e.pending[p.id] || slicesContainsEnv(nss, p.id) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", envNamePrefix, p.id))
	}
	if n := e.countEnvs(nss); n >= e.o.Max {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%d environments exist, the most this Forge API allows; destroy one first", n))
	}
	if m.GetValidateOnly() {
		preview.State = forgev1.Environment_STATE_UNSPECIFIED
		preview.ExpireTime = timestamppb.New(e.o.Now().Add(p.ttl))
		return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Environment: preview}), nil
	}
	e.pending[p.id] = true
	op := e.o.Operations.Start(ctx, kindCreateEnv, envNamePrefix+p.id, p.creator, e.createWork(p))
	if key := m.GetRequestId(); key != "" {
		e.requests[key] = op.GetName()
	}
	return connect.NewResponse(&forgev1.CreateEnvironmentResponse{Operation: op, Environment: preview}), nil
}

func slicesContainsEnv(nss []corev1.Namespace, id string) bool {
	for i := range nss {
		if nss[i].Labels[labelEnvironment] == id {
			return true
		}
	}
	return false
}

// plan checks the request and resolves the ref, forge.yaml and image.
func (e *Environments) plan(ctx context.Context, m *forgev1.CreateEnvironmentRequest) (envPlan, error) {
	p := envPlan{id: m.GetEnvironmentId(), ref: m.GetRef()}
	if !envIDRE.MatchString(p.id) {
		return p, invalidArg("%q is not a valid environment name (lowercase letters, digits and '-', at most 40)", p.id)
	}
	ttl, err := ttlOf(m.GetTtl())
	if err != nil {
		return p, err
	}
	p.ttl = ttl
	if p.ref == "" {
		p.ref = "main"
	}
	if !refRE.MatchString(p.ref) || strings.Contains(p.ref, "..") {
		return p, invalidArg("%q is not a valid Git ref", p.ref)
	}
	sid, ok := strings.CutPrefix(m.GetService(), servicePrefix)
	if !ok || sid == "" {
		return p, invalidArg("service %q must be services/<name>", m.GetService())
	}
	p.serviceID = sid
	svc, ok := e.o.Catalog.get(servicePrefix + sid)
	if !ok {
		return p, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", servicePrefix, sid))
	}
	if p.owner, p.repo, ok = repoOf(svc.GetRepoUrl(), e.o.Org); !ok {
		return p, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no GitHub repo in %s (repo_url %q)", servicePrefix, sid, e.o.Org, svc.GetRepoUrl()))
	}
	if pr, ok := PrincipalFrom(ctx); ok {
		p.creator = pr.Subject
	}
	p.commit, err = e.o.GitHub.ResolveRef(ctx, p.owner, p.repo, p.ref)
	if errors.Is(err, github.ErrNotFound) {
		return p, connect.NewError(connect.CodeNotFound, fmt.Errorf("ref %q not found in github.com/%s/%s", p.ref, p.owner, p.repo))
	}
	if err != nil {
		return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("resolve %s in github.com/%s/%s: %w", p.ref, p.owner, p.repo, err))
	}
	p.port, p.health = manifest.DefaultRunPort, manifest.DefaultRunHealth
	data, err := e.o.GitHub.FileAt(ctx, p.owner, p.repo, manifest.FileName, p.commit)
	switch {
	case errors.Is(err, github.ErrNotFound): // no forge.yaml: defaults
	case err != nil:
		return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read forge.yaml at %s: %w", shortSHA(p.commit), err))
	default:
		mf, err := manifest.Parse(data)
		if err != nil {
			return p, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("forge.yaml at %s is invalid: %w", shortSHA(p.commit), err))
		}
		p.port, p.health = int32(mf.RunPort()), mf.RunHealth() //nolint:gosec // Parse checks run.port is 1-65535
	}
	p.image = "ghcr.io/" + strings.ToLower(p.owner) + "/" + p.serviceID + ":" + shortSHA(p.commit)
	return p, nil
}

func (e *Environments) donePending(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pending, id)
}

// createWork is the create operation: image, namespace, apply, ready.
func (e *Environments) createWork(p envPlan) Work {
	return func(ctx context.Context, emit func(string, string)) error {
		defer e.donePending(p.id)
		emit("resolve", fmt.Sprintf("%s is %s", p.ref, shortSHA(p.commit)))
		if err := e.waitForImage(ctx, p, emit); err != nil {
			return err
		}
		ann := map[string]string{
			annRef: p.ref, annCommit: p.commit, annImage: p.image, annCreator: p.creator,
			annExpire: e.o.Now().Add(p.ttl).UTC().Format(time.RFC3339), annState: stateCreating,
		}
		if err := e.o.Cluster.CreateEnv(ctx, p.id, map[string]string{labelService: p.serviceID}, ann); err != nil {
			if errors.Is(err, errEnvExists) {
				return &OpError{Code: "already_exists", Message: envNamePrefix + p.id + " already exists"}
			}
			return err
		}
		e.donePending(p.id) // the namespace counts from now on
		emit("namespace", "created "+envNamespace(p.id))
		err := e.deployAndWait(ctx, p, emit)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return e.undo(ctx, p.id, emit)
		}
		var oe *OpError
		if !errors.As(err, &oe) {
			oe = &OpError{Code: "apply_failed", Message: err.Error()}
		}
		if aerr := e.o.Cluster.Annotate(context.WithoutCancel(ctx), p.id, map[string]string{annState: stateFailed, annStateMessage: clip(oe.Message)}); aerr != nil {
			e.o.Log.Warn("mark environment failed", "environment", p.id, "error", aerr)
		}
		return oe
	}
}

// waitForImage polls the registry until the image exists, failing fast when
// a CI run for the commit finished without success.
func (e *Environments) waitForImage(ctx context.Context, p envPlan, emit func(string, string)) error {
	timeout := time.NewTimer(e.o.ImageTimeout)
	defer timeout.Stop()
	last := ""
	for {
		ok, err := e.o.Images.Exists(ctx, p.image)
		if err == nil && ok {
			emit("image", "found "+p.image)
			return nil
		}
		status := "unknown"
		if runs, rerr := e.o.GitHub.RunsFor(ctx, p.owner, p.repo, p.commit); rerr == nil {
			if r := failedRun(runs); r != nil {
				return &OpError{Code: "ci_failed", Message: fmt.Sprintf("CI %q for %s finished with %s: %s", r.Name, shortSHA(p.commit), r.Conclusion, r.HTMLURL)}
			}
			status = "not started"
			if len(runs) > 0 {
				status = runs[0].Status
			}
		}
		msg := fmt.Sprintf("waiting for %s (CI: %s)", p.image, status)
		if err != nil {
			msg += "; registry: " + clip(err.Error())
		}
		if msg != last {
			emit("image", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "image_timeout", Message: fmt.Sprintf("%s did not appear within %s", p.image, e.o.ImageTimeout)}
		case <-time.After(e.o.ImagePoll):
		}
	}
}

// failedRun returns the newest run of a workflow when it finished without
// success; older runs of the same workflow do not count.
func failedRun(runs []github.WorkflowRun) *github.WorkflowRun {
	seen := map[string]bool{}
	for i := range runs {
		r := &runs[i]
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if r.Status == "completed" && r.Conclusion != "success" && r.Conclusion != "skipped" && r.Conclusion != "neutral" {
			return r
		}
	}
	return nil
}

// deployAndWait creates the workload and waits until it is available.
func (e *Environments) deployAndWait(ctx context.Context, p envPlan, emit func(string, string)) error {
	host := p.id + "." + e.o.Domain
	if err := e.o.Cluster.Deploy(ctx, p.id, Workload{Service: p.serviceID, Image: p.image, Host: host, Port: p.port, Health: p.health}); err != nil {
		return err
	}
	emit("apply", fmt.Sprintf("deployment %s on port %d, route %s", p.serviceID, p.port, host))
	timeout := time.NewTimer(e.o.ReadyTimeout)
	defer timeout.Stop()
	last := ""
	for {
		ready, problem, err := e.o.Cluster.Status(ctx, p.id, p.serviceID)
		if err == nil && ready {
			if err := e.o.Cluster.Annotate(ctx, p.id, map[string]string{annState: stateReady, annStateMessage: ""}); err != nil {
				return err
			}
			emit("ready", e.url(p.id))
			return nil
		}
		if err == nil && problem != "" && problem != last {
			emit("ready", problem)
			last = problem
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "not_ready", Message: fmt.Sprintf("%s was not ready within %s: %s", p.serviceID, e.o.ReadyTimeout, last)}
		case <-time.After(e.o.Poll):
		}
	}
}

// undo removes a cancelled create's namespace. The operation ends
// "cancelled", or "cancel_cleanup_failed" when the namespace stays.
func (e *Environments) undo(ctx context.Context, id string, emit func(string, string)) error {
	emit("cancel", "removing "+envNamespace(id))
	if err := e.removeNamespace(context.WithoutCancel(ctx), id); err != nil {
		return &OpError{Code: "cancel_cleanup_failed", Message: fmt.Sprintf("cancelled, but %s could not be removed: %v", envNamespace(id), err)}
	}
	return ctx.Err()
}

// removeNamespace deletes environment id's namespace and waits until it is gone.
func (e *Environments) removeNamespace(ctx context.Context, id string) error {
	if err := e.o.Cluster.DeleteEnv(ctx, id); err != nil && !errors.Is(err, errNoEnv) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.o.DestroyTimeout)
	defer cancel()
	for {
		if gone, err := e.o.Cluster.EnvGone(ctx, id); err == nil && gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s still exists after %s", envNamespace(id), e.o.DestroyTimeout)
		case <-time.After(e.o.Poll):
		}
	}
}
