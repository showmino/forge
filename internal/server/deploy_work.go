package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/resource"
)

// lock claims target ("<svc>/<env>") for opName; d.mu must be held.
func (d *Deploys) lock(target, opName string) error {
	if running, ok := d.running[target]; ok {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is already running for %s%s", running, deployNamePrefix, target))
	}
	d.running[target] = opName
	return nil
}

// claim locks svc/env and then checks, with the lock held, that the service
// is still in the catalog and is not being deleted: a delete holds these
// locks while it runs and removes the entry before it lets go. d.mu must be
// held.
func (d *Deploys) claim(ctx context.Context, svc, env string) error {
	target := svc + "/" + env
	if err := d.lock(target, "an operation"); err != nil {
		return err
	}
	err := d.o.Catalog.refuseDeleting(servicePrefix + svc)
	if err == nil {
		var ok bool
		if ok, err = d.o.Catalog.has(ctx, servicePrefix+svc); err == nil && !ok {
			err = notFound(servicePrefix + svc)
		}
	}
	if err != nil {
		delete(d.running, target)
	}
	return err
}

func (d *Deploys) unlock(target string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, target)
}

func actorOf(ctx context.Context) string {
	p, ok := PrincipalFrom(ctx)
	switch {
	case !ok:
		return "unknown"
	case p.Email != "":
		return p.Email
	default:
		return p.Subject
	}
}

func (d *Deploys) StartDeploy(ctx context.Context, req *connect.Request[forgev1.StartDeployRequest]) (*connect.Response[forgev1.StartDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	m := req.Msg
	if err := validEnv(m.GetEnv()); err != nil {
		return nil, err
	}
	ref, err := startRef(ctx, m.GetService(), m.GetEnv(), m.GetRef())
	if err != nil {
		return nil, err
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, m.GetService(), ref)
	if err != nil {
		return nil, err
	}
	if w := workloadOf(ctx); w != nil && src.commit != w.SHA {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolved %s to %s, not the CI token's commit %s", ref, shortSHA(src.commit), shortSHA(w.SHA)))
	}
	bump, err := bumpOf(m.GetBump(), m.GetEnv())
	if err != nil {
		return nil, err
	}
	env, actor := m.GetEnv(), actorOf(ctx)
	v := deployValues{
		Name: src.serviceID, Env: env, Image: src.image, Port: src.port, Health: src.health, Replicas: 1,
		Host:  d.host(src.serviceID, env),
		Forge: deployRecord{Service: servicePrefix + src.serviceID, Env: env, Ref: src.ref, Commit: src.commit, Actor: actor, Kind: kindDeploy},
	}
	if err := d.o.Cluster.ServiceNamespaceFree(ctx, src.serviceID, m.GetEnv()); err != nil {
		return nil, err
	}
	if err := d.o.Cluster.ApplicationFree(ctx, d.o.ArgoNamespace, src.serviceID+"-"+m.GetEnv()); err != nil {
		return nil, err
	}
	if v.Forge.Version, err = d.plannedVersion(ctx, src.owner, src.repo, src.commit, env, bump); err != nil {
		return nil, err
	}
	preview := d.deploymentFrom(src.serviceID, env, v, "", AppStatus{})
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.StartDeployResponse{Deployment: preview}), nil
	}
	target := src.serviceID + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if key := m.GetRequestId(); key != "" {
		if name, ok := d.requests[requestKey(ctx, key)]; ok {
			if op, ok := d.o.Operations.Get(name); ok {
				return connect.NewResponse(&forgev1.StartDeployResponse{Operation: op, Deployment: preview}), nil
			}
		}
	}
	if err := d.claim(ctx, src.serviceID, env); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, opDeploy, deployNamePrefix+target, actor, func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		emit("resolve", fmt.Sprintf("%s is %s", src.ref, shortSHA(src.commit)))
		if err := waitForImage(ctx, d.o.GitHub, d.o.Images, src, d.o.ImageTimeout, d.o.ImagePoll, emit); err != nil {
			return err
		}
		// Only now: a version is for a build that exists.
		version, err := d.cutVersion(ctx, src.owner, src.repo, src.commit, env, bump, emit)
		if err != nil {
			return err
		}
		v.Forge.Version = version
		return d.apply(ctx, src.serviceID, env, v, emit)
	})
	d.running[target] = op.GetName()
	if key := m.GetRequestId(); key != "" {
		d.requests[requestKey(ctx, key)] = op.GetName()
	}
	return connect.NewResponse(&forgev1.StartDeployResponse{Operation: op, Deployment: preview}), nil
}

// apply commits v for svc/env, then waits for its namespace and for Argo CD.
// v.Forge's operation and time are filled in here.
func (d *Deploys) apply(ctx context.Context, svc, env string, v deployValues, emit func(string, string)) error {
	if p, ok := ctx.Value(opNameKey{}).(string); ok {
		v.Forge.Operation = p
	}
	v.Forge.Time = d.o.Now().UTC().Format(time.RFC3339)
	// Worked out now, for this environment: never copied from an older
	// deploy (a rollback) or from another environment (a promote).
	db, err := d.database(ctx, svc, env)
	if err != nil {
		return fmt.Errorf("read the resources of %s%s in %s: %w", servicePrefix, svc, env, err)
	}
	if v.Database = db; db != nil {
		emit("database", "DATABASE_URL comes from Secret "+db.Secret)
	}
	rec := v.Forge
	rec.Image = v.Image
	content := v.marshal()
	commit, err := d.commit(ctx, valuesPath(svc, env), func() ([]byte, error) { return content, nil }, commitMessage(rec), emit)
	if err != nil {
		return err
	}
	emit("commit", fmt.Sprintf("forge-deploys %s: %s", shortSHA(commit), v.Image))
	stands := func(err error) error {
		if ctx.Err() != nil {
			return &OpError{Code: OpCodeCancelled, Message: fmt.Sprintf("cancelled; the deploy commit %s stands and Argo CD keeps applying it", shortSHA(commit))}
		}
		return err
	}
	nctx, cancel := context.WithTimeout(ctx, d.o.NamespaceTimeout)
	err = d.o.Cluster.WaitServiceNamespace(nctx, svc, env, d.o.Poll)
	cancel()
	if err != nil {
		return stands(&OpError{Code: "namespace_timeout", Message: fmt.Sprintf("namespace %s-%s did not appear within %s: %v", svc, env, d.o.NamespaceTimeout, err)})
	}
	if err := d.o.Cluster.WriteServicePullSecret(ctx, svc, env); err != nil {
		return stands(fmt.Errorf("pull secret for %s-%s: %w", svc, env, err))
	}
	emit("namespace", svc+"-"+env)
	return stands(d.awaitSync(ctx, svc, env, commit, content, emit))
}

// database is the database the service's pods should reach in env: the one
// with a Secret that is not being deleted. nil when there is none.
func (d *Deploys) database(ctx context.Context, svc, env string) (*deployDatabase, error) {
	if d.o.Resources == nil {
		return nil, nil
	}
	rs, err := d.o.Resources.ListResources(ctx, servicePrefix+svc, env)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.GetType() == resource.TypePostgres && r.GetSecret() != "" && r.GetState() != forgev1.Resource_STATE_DELETING {
			return &deployDatabase{Secret: r.GetSecret()}, nil
		}
	}
	return nil, nil
}

// commit writes build()'s content to path on the deploy repo's head. When
// the branch moved it rebuilds on the new head and retries (3 attempts).
func (d *Deploys) commit(ctx context.Context, path string, build func() ([]byte, error), message string, emit func(string, string)) (string, error) {
	for attempt := range 3 {
		if attempt > 0 {
			emit("commit", "the deploy repo moved; retrying on the new head")
		}
		content, err := build()
		if err != nil {
			return "", err
		}
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return "", fmt.Errorf("deploy repo head: %w", err)
		}
		sha, err := d.o.Repo.CommitFile(ctx, head, tree, path, content, message)
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		return sha, err
	}
	return "", &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; try again"}
}

// awaitSync waits until Argo CD has synced the environment's values to
// deployCommit and the app is healthy. Argo resolves the values source to the
// deploy repo's head, so once another commit lands it reports that newer SHA
// for good: a revision then counts when it is deployCommit or when the values
// file at that revision equals content, what we committed.
func (d *Deploys) awaitSync(ctx context.Context, svc, env, deployCommit string, content []byte, emit func(string, string)) error {
	timeout := time.NewTimer(d.o.SyncTimeout)
	defer timeout.Stop()
	last := ""
	// checked caches, per reported revision, whether it holds our values.
	// Reported SHAs are immutable, and the chart revision is never in the
	// deploy repo, so a NotFound is final ("not ours"). Other errors are not
	// cached: they may pass.
	checked := map[string]bool{}
	atRevision := func(revs []string) bool {
		if slices.Contains(revs, deployCommit) {
			return true
		}
		for _, rev := range revs {
			same, done := checked[rev]
			if !done {
				data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), rev)
				switch {
				case errors.Is(err, github.ErrNotFound):
					checked[rev] = false
				case err != nil:
					continue // try again
				default:
					same = bytes.Equal(data, content)
					checked[rev] = same
				}
			}
			if same {
				return true
			}
		}
		return false
	}
	for {
		st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
		var msg string
		if err != nil {
			msg = "reading the Argo CD application: " + clip(err.Error())
		} else {
			atRev := atRevision(st.Revisions)
			if atRev && st.Sync == "Synced" && st.Health == "Healthy" {
				emit("sync", "http://"+d.host(svc, env))
				return nil
			}
			msg = fmt.Sprintf("%s, %s", st.Sync, st.Health)
			if !atRev {
				msg += ", waiting for " + shortSHA(deployCommit)
			}
			if st.Message != "" {
				msg += ": " + st.Message
			}
			if problem, perr := d.o.Cluster.ServicePodProblem(ctx, svc, env); perr == nil && problem != "" {
				msg += "; pod: " + problem
			}
		}
		if msg != last {
			emit("sync", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return &OpError{Code: "not_healthy", Message: fmt.Sprintf("%s-%s was not Synced and Healthy within %s: %s", svc, env, d.o.SyncTimeout, last)}
		case <-time.After(d.o.Poll):
		}
	}
}
