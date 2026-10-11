package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/store"
)

// kindDeleteService is the operation that takes a service down before it
// leaves the catalog.
const kindDeleteService = "delete_service"

// teardown is what DeleteService removes from the cluster. Either part may be
// nil: a Forge without previews or without deploys has none of them to remove.
type teardown struct {
	deploys *Deploys
	envs    *Environments
	ops     *Operations
}

// EnableTeardown makes DeleteService take down what a service runs (its
// previews, its staging and production deployments and their namespaces)
// before the service leaves the catalog. Without it, DeleteService only
// removes the catalog entry.
func (c *Catalog) EnableTeardown(d *Deploys, e *Environments, ops *Operations) {
	c.teardown = &teardown{deploys: d, envs: e, ops: ops}
}

// teardownPlan is what a service runs right now.
type teardownPlan struct {
	// previews are environment ids.
	previews []string
	// envs are deploy environments, in promotion order.
	envs []string
}

func (p teardownPlan) empty() bool { return len(p.previews) == 0 && len(p.envs) == 0 }

// plan finds what the service called name ("services/x") runs.
func (t *teardown) plan(ctx context.Context, name string) (teardownPlan, error) {
	var p teardownPlan
	svc := strings.TrimPrefix(name, servicePrefix)
	if t.envs != nil && t.envs.ready() == nil {
		nss, err := t.envs.o.Cluster.Envs(ctx)
		if err != nil {
			return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list the previews of %s: %w", name, err))
		}
		for i := range nss {
			if env := t.envs.envFromNamespace(&nss[i]); env.GetService() == name {
				p.previews = append(p.previews, strings.TrimPrefix(env.GetName(), envNamePrefix))
			}
		}
		slices.Sort(p.previews)
	}
	if t.deploys != nil && t.deploys.ready() == nil {
		for _, env := range deployEnvs {
			there, err := t.deploys.deployed(ctx, svc, env)
			if err != nil {
				return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("check %s%s/%s: %w", deployNamePrefix, svc, env, err))
			}
			if there {
				p.envs = append(p.envs, env)
			}
		}
	}
	return p, nil
}

// deployed reports whether anything of svc/env is left: its values in the
// deploy repo, its Argo CD application, or its namespace. A namespace of that
// name that is not Forge's does not count: it is someone else's.
func (d *Deploys) deployed(ctx context.Context, svc, env string) (bool, error) {
	if _, err := d.o.Repo.Read(ctx, valuesPath(svc, env), ""); err == nil {
		return true, nil
	} else if !errors.Is(err, github.ErrNotFound) {
		return false, err
	}
	if there, err := d.o.Cluster.ApplicationExists(ctx, d.o.ArgoNamespace, svc+"-"+env); err != nil || there {
		return there, err
	}
	err := d.o.Cluster.checkServiceNamespace(ctx, svc, env)
	if errors.Is(err, errNoEnv) { // missing, or not Forge's
		return false, nil
	}
	return err == nil, err
}

// waitGone polls gone until it reports true or timeout passes; it returns
// false when time ran out.
func waitGone(ctx context.Context, timeout, poll time.Duration, gone func() (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if ok, err := gone(); err == nil && ok {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, context.Cause(ctx)
		case <-time.After(poll):
		}
	}
}

// undeploy removes svc from env: its values leave the deploy repo, Argo CD
// then removes the application and its workloads, and last the namespace
// goes. Every step is safe to repeat, so a delete that failed half way can be
// asked for again.
func (d *Deploys) undeploy(ctx context.Context, svc, env string, emit func(string, string)) error {
	path, target := valuesPath(svc, env), svc+"-"+env
	removed := false
	for attempt := 0; attempt < 3 && !removed; attempt++ {
		if attempt > 0 {
			emit("commit", "the deploy repo moved; retrying on the new head")
		}
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return fmt.Errorf("deploy repo head: %w", err)
		}
		if _, err := d.o.Repo.Read(ctx, path, head); errors.Is(err, github.ErrNotFound) {
			removed = true // nothing to remove: never deployed, or an earlier delete got this far
			break
		} else if err != nil {
			return fmt.Errorf("read %s in the deploy repo: %w", path, err)
		}
		sha, err := d.o.Repo.DeleteFile(ctx, head, tree, path, fmt.Sprintf("remove %s from %s (%s)", svc, env, actorOf(ctx)))
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		if err != nil {
			return fmt.Errorf("remove %s from the deploy repo: %w", path, err)
		}
		emit("commit", fmt.Sprintf("forge-deploys %s: removed %s from %s", shortSHA(sha), svc, env))
		removed = true
	}
	if !removed {
		return &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; delete again"}
	}

	gone, err := waitGone(ctx, d.o.SyncTimeout, d.o.Poll, func() (bool, error) {
		there, err := d.o.Cluster.ApplicationExists(ctx, d.o.ArgoNamespace, target)
		return !there, err
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !gone {
		return &OpError{Code: "sync_timeout", Message: fmt.Sprintf(
			"Argo CD still has the application %s after %s (%v); its workloads may still be running. Delete again once it is gone", target, d.o.SyncTimeout, err)}
	}
	emit("argo", "Argo CD removed "+target+" and its workloads")

	if err := d.o.Cluster.DeleteServiceNamespace(ctx, svc, env); err != nil {
		return fmt.Errorf("delete namespace %s: %w", target, err)
	}
	gone, err = waitGone(ctx, d.o.NamespaceTimeout, d.o.Poll, func() (bool, error) {
		there, err := d.o.Cluster.ServiceNamespaceExists(ctx, svc, env)
		return !there, err
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !gone {
		return &OpError{Code: "namespace_timeout", Message: fmt.Sprintf("namespace %s still exists after %s (%v); delete again once it is gone", target, d.o.NamespaceTimeout, err)}
	}
	emit("namespace", "removed "+target)
	return nil
}

// lockService claims every deploy environment of svc, so nothing is deployed
// while the service is being deleted. It returns the release, or the reason
// (a deploy, promote or rollback that is still running).
func (d *Deploys) lockService(svc string) (func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, env := range deployEnvs {
		if err := d.lock(svc+"/"+env, "a delete"); err != nil {
			for _, held := range deployEnvs[:i] {
				delete(d.running, svc+"/"+held)
			}
			return nil, err
		}
	}
	return func() {
		for _, env := range deployEnvs {
			d.unlock(svc + "/" + env)
		}
	}, nil
}

// deleteChecked removes name from the catalog after checking, in the same
// transaction, that it exists, that etag (when given) still matches and that
// nothing depends on it. With dry it only checks.
func (c *Catalog) deleteChecked(ctx context.Context, name, etag string, dry bool) error {
	return c.store.Write(ctx, func(tx store.Tx) error {
		cur, _, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if etag != "" && etag != cur.GetEtag() {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, etag, cur.GetEtag()))
		}
		deps, err := tx.Dependents(ctx, name)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a dependency of %s", name, deps[0]))
		}
		if dry {
			return nil
		}
		return tx.Delete(ctx, name)
	})
}

// teardownWork takes down what plan names, then removes name from the
// catalog. The entry goes last: until then the service stays visible, and a
// delete that failed can be asked for again.
func (c *Catalog) teardownWork(name string, plan teardownPlan, release func()) Work {
	svc := strings.TrimPrefix(name, servicePrefix)
	return func(ctx context.Context, emit func(string, string)) error {
		defer release()
		t := c.teardown
		for _, id := range plan.previews {
			emit("preview", "destroying "+id)
			if err := t.envs.destroyWork(id)(ctx, emit); err != nil {
				return err
			}
		}
		// Production first: staging keeps running until production is down.
		for _, env := range slices.Backward(plan.envs) {
			if err := t.deploys.undeploy(ctx, svc, env, emit); err != nil {
				return err
			}
		}
		if err := c.deleteChecked(ctx, name, "", false); err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				return nil // someone removed the entry meanwhile: the service is gone, as asked
			}
			return &OpError{Code: "catalog", Message: "everything " + name + " ran is removed, but its catalog entry is not: " + err.Error()}
		}
		emit("catalog", "removed "+name+" from the catalog; its GitHub repo is kept")
		return nil
	}
}

// startTeardown locks the service's deploys, looks at what it runs and either
// removes it from the catalog at once (nothing runs) or starts the operation
// that takes it down first. op is nil when the service was removed at once.
func (c *Catalog) startTeardown(ctx context.Context, name, etag string, dry bool) (*forgev1.Operation, error) {
	t := c.teardown
	svc := strings.TrimPrefix(name, servicePrefix)
	release := func() {}
	if t.deploys != nil && t.deploys.ready() == nil && !dry {
		var err error
		if release, err = t.deploys.lockService(svc); err != nil {
			return nil, err
		}
	}
	plan, err := t.plan(ctx, name)
	if err != nil {
		release()
		return nil, err
	}
	if dry || plan.empty() {
		defer release()
		return nil, storeErr(c.deleteChecked(ctx, name, etag, dry))
	}
	// Checked now, so a delete that cannot end well does not take anything down.
	if err := c.deleteChecked(ctx, name, etag, true); err != nil {
		release()
		return nil, storeErr(err)
	}
	return t.ops.Start(ctx, kindDeleteService, name, actorOf(ctx), c.teardownWork(name, plan, release)), nil
}
