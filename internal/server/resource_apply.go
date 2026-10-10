package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// applicable checks everything about a saved plan that needs no policy: its
// state and age, that it changes something, and that neither forge.yaml nor
// the real state moved since it was made.
func (r *Resources) applicable(ctx context.Context, rec *store.PlanRecord, m *forgev1.ApplyResourcePlanRequest) error {
	p := r.view(rec)
	name := p.GetName()
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
	case forgev1.ResourcePlan_STATE_DENIED:
		return precondition("%s was denied by policy", name)
	case forgev1.ResourcePlan_STATE_APPLIED:
		return precondition("%s was already applied", name)
	case forgev1.ResourcePlan_STATE_FAILED, forgev1.ResourcePlan_STATE_APPLY_FAILED:
		return precondition("%s failed; run 'forge resource plan' again", name)
	case forgev1.ResourcePlan_STATE_EXPIRED:
		return precondition("%s expired; run 'forge resource plan' again", name)
	case forgev1.ResourcePlan_STATE_PLANNING:
		return precondition("%s is still being made", name)
	case forgev1.ResourcePlan_STATE_APPLYING:
		return precondition("%s is already being applied", name)
	default:
		return precondition("%s is in state %s", name, planStateWord(p.GetState()))
	}
	if len(p.GetChanges()) == 0 {
		return precondition("%s has no changes: nothing to apply", name)
	}
	if m.GetCheckResources() {
		settings, err := resource.Resolve(m.GetResources(), p.GetEnv())
		if err != nil {
			return invalidArg("resources: %v", err)
		}
		if resource.Digest(settings) != rec.Digest {
			return precondition("forge.yaml changed since %s was made; run 'forge resource plan' again", name)
		}
	}
	_, target := planTarget(p)
	st, err := r.o.Store.GetResourceState(ctx, target)
	if err != nil {
		return storeErr(err)
	}
	if st.Version != rec.StateVersion {
		return precondition("the resources changed since %s was made; run 'forge resource plan' again", name)
	}
	return nil
}

// deny marks a ready plan as denied by pol: rules that changed since the
// plan was made refused it.
func (r *Resources) deny(ctx context.Context, planName string, pol *forgev1.PolicyResult) error {
	return r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return nil
		}
		cur.Plan.State, cur.Plan.Policy = forgev1.ResourcePlan_STATE_DENIED, pol
		cur.PlanFile, cur.PlanJSON = nil, nil
		return tx.PutResourcePlan(ctx, cur)
	})
}

// markResources records, as an apply starts, what it is about to make or delete.
func markResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, now *timestamppb.Timestamp) error {
	for _, c := range p.GetChanges() {
		name := resourceName(p, c.GetResource())
		old, err := tx.GetResource(ctx, name)
		missing := errors.Is(err, store.ErrNotFound)
		if err != nil && !missing {
			return err
		}
		var res *forgev1.Resource
		switch {
		case c.GetAction() == "delete" && !missing:
			res = old
			res.State = forgev1.Resource_STATE_DELETING
		case c.GetAction() == "create" && missing && c.GetAfter() != nil:
			res = &forgev1.Resource{Name: name, Service: p.GetService(), Env: p.GetEnv(), Type: c.GetAfter().GetType(),
				Settings: c.GetAfter(), State: forgev1.Resource_STATE_APPLYING, CreateTime: now}
		default:
			continue
		}
		res.LastPlan, res.UpdateTime = p.GetName(), now
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// ApplyResourcePlan checks a saved plan, checks policy again, and starts the
// run that applies exactly that plan.
func (r *Resources) ApplyResourcePlan(ctx context.Context, req *connect.Request[forgev1.ApplyResourcePlanRequest]) (*connect.Response[forgev1.ApplyResourcePlanResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	m := req.Msg
	planName, err := parsePlanName(m.GetName())
	if err != nil {
		return nil, err
	}
	rec, err := r.o.Store.GetResourcePlan(ctx, planName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(planName)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	_, target := planTarget(rec.Plan)
	noteAuditService(ctx, rec.Plan.GetService())
	noteAuditTarget(ctx, resourceNamePrefix+target)
	noteAuditDetail(ctx, "plan", planName)
	noteAuditDetail(ctx, "environment", rec.Plan.GetEnv())
	if err := r.applicable(ctx, rec, m); err != nil {
		return nil, err
	}

	// The rules may have changed since the plan was made: today's decide.
	pol, err := r.evaluate(ctx, rec, rec.PlanJSON, policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("policy could not be evaluated, so %s was not applied: %w", planName, err))
	}
	noteAuditDetail(ctx, "policy_rules", pol.GetRules())
	if pol.GetDenied() {
		noteAuditDetail(ctx, "policy", "denied")
		if err := r.deny(ctx, planName, pol); err != nil {
			return nil, storeErr(err)
		}
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is denied by policy: %s", planName, strings.Join(pol.GetDeny(), "; ")))
	}
	noteAuditDetail(ctx, "policy", "passed")
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.ApplyResourcePlanResponse{Plan: r.view(rec)}), nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if running, ok := r.running[target]; ok {
		return nil, precondition("%s is already running for %s%s", running, resourceNamePrefix, target)
	}
	if other, err := r.storedRun(ctx, rec.Plan.GetService(), rec.Plan.GetEnv()); err != nil {
		return nil, err
	} else if other != "" {
		return nil, precondition("%s is still running for %s%s; it ends within %s", other, resourceNamePrefix, target, r.o.RunTimeout)
	}
	now := r.o.Now()
	token, hash := newRunToken()
	rec.Plan.State, rec.Plan.Applier, rec.Plan.Policy = forgev1.ResourcePlan_STATE_APPLYING, actorOf(ctx), pol
	rec.TokenHash, rec.Deadline = hash, now.Add(r.o.RunTimeout)
	op, err := r.startRun(ctx, rec, run.ModeApply, token, func(tx store.ResourceTx) error {
		// The plan may have been applied, denied or expired since it was read.
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return precondition("%s is no longer ready to apply", planName)
		}
		return markResources(ctx, tx, rec.Plan, timestamppb.New(now))
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ApplyResourcePlanResponse{Operation: op, Plan: r.view(rec)}), nil
}

var resourceShortRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)

// parseResourceName checks "resources/<service>/<env>/<name>".
func parseResourceName(name string) (string, error) {
	bad := invalidArg("%q is not a resource name (resources/<service>/<env>/<name>)", name)
	rest, ok := strings.CutPrefix(name, resourceNamePrefix)
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 || !manifest.ValidID(parts[0]) || validEnv(parts[1]) != nil || !resourceShortRE.MatchString(parts[2]) {
		return "", bad
	}
	return name, nil
}

// withHealth sets res.Health from the cluster: "unknown" when it cannot be read.
func (r *Resources) withHealth(ctx context.Context, res *forgev1.Resource) {
	res.Health = &forgev1.ResourceHealth{Phase: "unknown"}
	if r.o.Health == nil {
		return
	}
	svc := strings.TrimPrefix(res.GetService(), servicePrefix)
	h, err := r.o.Health.DatabaseHealth(ctx, svc, res.GetEnv(), res.GetSettings().GetName())
	if err != nil || h == nil {
		if err != nil {
			r.o.Log.Warn("reading a database's health", "resource", res.GetName(), "error", err)
		}
		return
	}
	res.Health = h
}

// ListResources returns resources with their live health.
func (r *Resources) ListResources(ctx context.Context, req *connect.Request[forgev1.ListResourcesRequest]) (*connect.Response[forgev1.ListResourcesResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	service := ""
	if m.GetService() != "" {
		name, err := parseName(m.GetService())
		if err != nil {
			return nil, err
		}
		service = name
	}
	if m.GetEnv() != "" {
		if err := validEnv(m.GetEnv()); err != nil {
			return nil, err
		}
	}
	rs, err := r.o.Store.ListResources(ctx, service, m.GetEnv())
	if err != nil {
		return nil, storeErr(err)
	}
	for _, res := range rs {
		r.withHealth(ctx, res)
	}
	return connect.NewResponse(&forgev1.ListResourcesResponse{Resources: rs}), nil
}

// GetResource returns one resource with its live health.
func (r *Resources) GetResource(ctx context.Context, req *connect.Request[forgev1.GetResourceRequest]) (*connect.Response[forgev1.GetResourceResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name, err := parseResourceName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	res, err := r.o.Store.GetResource(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	r.withHealth(ctx, res)
	return connect.NewResponse(&forgev1.GetResourceResponse{Resource: res}), nil
}
