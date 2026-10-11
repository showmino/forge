package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

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

func (t *teardown) hasDeploys() bool  { return t.deploys != nil }
func (t *teardown) hasPreviews() bool { return t.envs != nil }

// teardownPlan is what a service runs right now.
type teardownPlan struct {
	// previews are environment ids: the service's preview namespaces, and
	// creates of it that have not made their namespace yet.
	previews []string
	// envs are deploy environments, in promotion order.
	envs []string
}

func (p teardownPlan) empty() bool { return len(p.previews) == 0 && len(p.envs) == 0 }

// removes names what the plan takes down, for the caller to read.
func (p teardownPlan) removes(svc string) []string {
	var out []string
	for _, id := range p.previews {
		out = append(out, envNamePrefix+id)
	}
	for _, env := range slices.Backward(p.envs) {
		out = append(out, deployNamePrefix+svc+"/"+env)
	}
	return out
}

// previewsOf lists the previews of the service with id svc: its namespaces
// and its creates still waiting for an image.
func (e *Environments) previewsOf(ctx context.Context, svc string) ([]string, error) {
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range nss {
		if nss[i].Labels[labelService] == svc {
			ids = append(ids, nss[i].Labels[labelEnvironment])
		}
	}
	e.mu.Lock()
	for id, of := range e.pending {
		if of == svc && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	e.mu.Unlock()
	slices.Sort(ids)
	return ids, nil
}

// plan finds what the service called name ("services/x") runs.
func (t *teardown) plan(ctx context.Context, name string) (teardownPlan, error) {
	var p teardownPlan
	svc := strings.TrimPrefix(name, servicePrefix)
	if t.hasPreviews() {
		ids, err := t.envs.previewsOf(ctx, svc)
		if err != nil {
			return p, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list the previews of %s: %w", name, err))
		}
		p.previews = ids
	}
	if t.hasDeploys() {
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

// deployed reports whether anything of svc/env is left that is Forge's: its
// values in the deploy repo, its Argo CD application, or its namespace. An
// application or a namespace of that name that is not Forge's does not
// count: it is someone else's, and is left alone.
func (d *Deploys) deployed(ctx context.Context, svc, env string) (bool, error) {
	if _, err := d.o.Repo.Read(ctx, valuesPath(svc, env), ""); err == nil {
		return true, nil
	} else if !errors.Is(err, github.ErrNotFound) {
		return false, err
	}
	if there, err := d.o.Cluster.ForgeApplicationExists(ctx, d.o.ArgoNamespace, svc+"-"+env); err != nil || there {
		return there, err
	}
	err := d.o.Cluster.checkServiceNamespace(ctx, svc, env)
	if errors.Is(err, errNoEnv) { // missing, or not Forge's
		return false, nil
	}
	return err == nil, err
}

// waitGone polls gone until it reports true or timeout passes. It returns
// whether the thing is gone and, when it is not, the last error of the check
// (nil when every check answered "still there"). A check the API refuses
// ends the wait at once: asking again will not change the answer.
func waitGone(ctx context.Context, timeout, poll time.Duration, gone func(context.Context) (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		ok, err := gone(ctx)
		if err == nil && ok {
			return true, nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return false, err
		}
		if ctx.Err() == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return false, last
		case <-time.After(poll):
		}
	}
}

// notGone words why a wait ended without the thing being gone.
func notGone(what string, timeout time.Duration, last error) string {
	if last != nil {
		return fmt.Sprintf("could not check whether %s is gone: %v", what, last)
	}
	return fmt.Sprintf("%s still exists after %s", what, timeout)
}

// undeploy removes svc from env: its values leave the deploy repo, Argo CD
// then removes the application and its workloads, and last the namespace
// goes. Every step is safe to repeat, so a delete that failed half way can be
// asked for again.
func (d *Deploys) undeploy(ctx context.Context, svc, env string, emit func(string, string)) error {
	path, target := valuesPath(svc, env), svc+"-"+env
	removed, commit := false, ""
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
		removed, commit = true, sha
	}
	if !removed {
		return &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; delete again"}
	}
	// From here on the takedown is under way whatever happens to this
	// operation: Argo CD acts on the deploy repo, not on us. A cancel must say so.
	stands := func(err error) error {
		if ctx.Err() == nil {
			return err
		}
		at := ""
		if commit != "" {
			at = " (commit " + shortSHA(commit) + ")"
		}
		return &OpError{Code: OpCodeCancelled, Message: fmt.Sprintf(
			"cancelled; %s is already removed from %s in the deploy repo%s and Argo CD is taking it down. Deploy again to restore it, or delete again to finish",
			svc, env, at)}
	}

	gone, last := waitGone(ctx, d.o.SyncTimeout, d.o.Poll, func(ctx context.Context) (bool, error) {
		there, err := d.o.Cluster.ForgeApplicationExists(ctx, d.o.ArgoNamespace, target)
		return !there, err
	})
	if !gone {
		return stands(&OpError{Code: "sync_timeout", Message: notGone("the Argo CD application "+target, d.o.SyncTimeout, last) +
			"; its workloads may still be running. Delete again once it is gone"})
	}
	emit("argo", "Argo CD removed "+target+" and its workloads")

	switch err := d.o.Cluster.DeleteServiceNamespace(ctx, svc, env); {
	case errors.Is(err, errUnlabelled):
		// A namespace of this name that is not Forge's is someone else's.
		emit("namespace", "left "+target+" alone: it is not Forge's")
		return nil
	case err != nil:
		return stands(fmt.Errorf("delete namespace %s: %w", target, err))
	}
	gone, last = waitGone(ctx, d.o.NamespaceTimeout, d.o.Poll, func(ctx context.Context) (bool, error) {
		err := d.o.Cluster.checkServiceNamespace(ctx, svc, env)
		if errors.Is(err, errNoEnv) { // gone, or replaced by one that is not Forge's
			return true, nil
		}
		return false, err
	})
	if !gone {
		return stands(&OpError{Code: "namespace_timeout", Message: notGone("namespace "+target, d.o.NamespaceTimeout, last) + "; delete again once it is gone"})
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
	return storeErr(c.store.Write(ctx, func(tx store.Tx) error {
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
	}))
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
			// Looked at again: the plan may be minutes old by now.
			mine, err := t.envs.previewsOf(ctx, svc)
			if err != nil {
				return fmt.Errorf("list the previews of %s: %w", name, err)
			}
			if !slices.Contains(mine, id) {
				continue // destroyed meanwhile
			}
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
		// Nothing new may be started for a service being deleted, but a
		// request that was already past its checks can still have landed.
		// Look once more before the entry goes: whatever is there now would
		// otherwise run on unseen.
		if again, err := t.plan(ctx, name); err != nil {
			return err
		} else if !again.empty() {
			return &OpError{Code: "changed", Message: fmt.Sprintf(
				"%s got something new while it was being deleted (%s); it is still in the catalog. Delete again", name, strings.Join(again.removes(svc), ", "))}
		}
		if err := c.checkNoResources(ctx, name); err != nil {
			return &OpError{Code: "changed", Message: err.Error()}
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

// startTeardown claims the service, looks at what it runs and either removes
// it from the catalog at once (nothing runs) or starts the operation that
// takes it down first. op is nil when the service was removed at once, and
// for a dry run; removes names what the delete takes (or would take) down.
func (c *Catalog) startTeardown(ctx context.Context, name, etag string, dry bool) (op *forgev1.Operation, removes []string, err error) {
	t := c.teardown
	svc := strings.TrimPrefix(name, servicePrefix)
	// Asking again while a delete runs gets that delete, not a refusal.
	if t.ops != nil && !dry {
		for _, running := range t.ops.List(name, true) {
			if running.GetKind() == kindDeleteService {
				return running, nil, nil
			}
		}
	}
	// A dry run claims nothing, but says so when a real delete would be refused.
	if dry {
		if err := c.refuseDeleting(name); err != nil {
			return nil, nil, err
		}
	} else if !c.markDeleting(name, "a delete") {
		return nil, nil, c.refuseDeleting(name)
	}
	release := func() {}
	if t.hasDeploys() {
		unlock, err := t.deploys.lockService(svc)
		if err != nil {
			if !dry {
				c.unmarkDeleting(name)
			}
			return nil, nil, err
		}
		release = unlock
	}
	if !dry {
		unlock := release
		release = func() { unlock(); c.unmarkDeleting(name) }
	}
	// Until the operation owns them, the claims are given back on every way out.
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()

	plan, err := t.plan(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	removes = plan.removes(svc)
	if dry || plan.empty() {
		return nil, removes, c.deleteChecked(ctx, name, etag, dry)
	}
	// Checked now, so a delete that cannot end well does not take anything down.
	if err := c.deleteChecked(ctx, name, etag, true); err != nil {
		return nil, nil, err
	}
	handedOff = true
	op = t.ops.Start(ctx, kindDeleteService, name, actorOf(ctx), c.teardownWork(name, plan, release))
	c.delMu.Lock()
	if _, still := c.deleting[name]; still {
		c.deleting[name] = op.GetName()
	}
	c.delMu.Unlock()
	return op, removes, nil
}
