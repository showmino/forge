package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// A service's deploy environments: staging, production or both
// (docs/specs/2026-11-09-service-environments-design.md).

// kindRemove is the operation that takes one environment's deploy down.
const kindRemove = "delete_deployment"

// serviceEnvs returns the environments s has, in deploy order. A service
// made before the choice existed has none recorded: an infrastructure repo
// has its GitHub environments, every other service both.
func serviceEnvs(s *forgev1.Service) []string {
	if len(s.GetEnvironments()) > 0 {
		return slices.Clone(s.GetEnvironments())
	}
	if i := s.GetInfrastructure(); i != nil && len(i.GetEnvironments()) > 0 {
		if envs, err := normalEnvs(i.GetEnvironments()); err == nil {
			return envs
		}
	}
	return slices.Clone(deployEnvs)
}

// claimIn is claim, and then checks, with the lock held, that the service
// still has env: an environments change holds the service's locks while it
// checks and writes, so a deploy either sees the new environments or keeps
// the change waiting. d.mu must be held.
func (d *Deploys) claimIn(ctx context.Context, svc, env string) error {
	if err := d.claim(ctx, svc, env); err != nil {
		return err
	}
	if err := d.requireEnv(ctx, svc, env); err != nil {
		delete(d.running, svc+"/"+env)
		return err
	}
	return nil
}

// normalEnvs checks a requested list of environments and returns it in
// deploy order, each once. Empty is both.
func normalEnvs(envs []string) ([]string, error) {
	if len(envs) == 0 {
		return slices.Clone(deployEnvs), nil
	}
	for _, e := range envs {
		if !slices.Contains(deployEnvs, e) {
			return nil, invalid(fmt.Sprintf("environment %q must be staging or production", e))
		}
	}
	var out []string
	for _, e := range deployEnvs {
		if slices.Contains(envs, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// envsText words a service's environments: "production only", "staging and
// production".
func envsText(envs []string) string {
	if len(envs) == 1 {
		return envs[0] + " only"
	}
	return strings.Join(envs, " and ")
}

// lacksEnv is the refusal of a deploy or rollback in an environment svc
// does not have.
func lacksEnv(svc *forgev1.Service, env string) error {
	id := strings.TrimPrefix(svc.GetName(), servicePrefix)
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"%s has %s, not %s: add it with 'forge service update %s --environments staging,production'",
		svc.GetName(), envsText(serviceEnvs(svc)), env, id))
}

// requireEnv refuses env unless the service svc (an id) has it.
func (d *Deploys) requireEnv(ctx context.Context, svc, env string) error {
	s, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		return err
	}
	if !slices.Contains(serviceEnvs(s), env) {
		return lacksEnv(s, env)
	}
	return nil
}

// requireBoth refuses a promotion of a service that has one environment.
func (d *Deploys) requireBoth(ctx context.Context, svc string) error {
	s, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		return err
	}
	envs := serviceEnvs(s)
	if len(envs) == len(deployEnvs) {
		return nil
	}
	how := fmt.Sprintf("deploy to it with 'forge deploy start %s --env %s'", svc, envs[0])
	if envs[0] == "staging" {
		how = fmt.Sprintf("add production with 'forge service update %s --environments staging,production' to promote", svc)
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has %s, so there is nothing to promote: %s", s.GetName(), envsText(envs), how))
}

// DeleteDeployment takes down what runs in one environment, as an
// operation. The service keeps its other environment and its previews.
func (d *Deploys) DeleteDeployment(ctx context.Context, req *connect.Request[forgev1.DeleteDeploymentRequest]) (*connect.Response[forgev1.DeleteDeploymentResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := authorize(ctx, ActDeployRemove, servicePrefix+svc); err != nil {
		return nil, err
	}
	there, err := d.deployed(ctx, svc, env)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("check %s%s/%s: %w", deployNamePrefix, svc, env, err))
	}
	if !there {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("nothing of %s runs in %s", servicePrefix+svc, env))
	}
	// What runs there, when its values are still in the deploy repo; a
	// delete that failed half way may have removed them already.
	var shown *forgev1.Deployment
	if v, commit, err := d.current(ctx, svc, env); err == nil {
		shown = d.deploymentFrom(svc, env, v, commit, AppStatus{})
	}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.DeleteDeploymentResponse{Deployment: shown}), nil
	}
	target := svc + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.claim(ctx, svc, env); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, kindRemove, deployNamePrefix+target, actorOf(ctx), func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		return d.undeploy(ctx, svc, env, emit)
	})
	d.running[target] = op.GetName()
	return connect.NewResponse(&forgev1.DeleteDeploymentResponse{Operation: op, Deployment: shown}), nil
}

// checkDroppedEnvs refuses to drop an environment from svc (a name) that
// still runs something: it must be taken down first.
func (c *Catalog) checkDroppedEnvs(ctx context.Context, cur *forgev1.Service, next []string) error {
	for _, env := range serviceEnvs(cur) {
		if slices.Contains(next, env) {
			continue
		}
		if c.teardown == nil || !c.teardown.hasDeploys() {
			continue // no deploys on this Forge: nothing can run there
		}
		there, err := c.teardown.deploys.deployed(ctx, strings.TrimPrefix(cur.GetName(), servicePrefix), env)
		if err != nil {
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf("check %s in %s: %w", cur.GetName(), env, err))
		}
		if there {
			id := strings.TrimPrefix(cur.GetName(), servicePrefix)
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s still runs in %s: take it down first with 'forge deploy remove %s --env %s'", cur.GetName(), env, id, env))
		}
	}
	return nil
}
