package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// The operations that change what runs without a deploy. Their commits to
// the deploy repo carry no Forge- lines, so the deploy history, a rollback
// and a promotion's version never count them as deploys.
const (
	kindRestart = "restart"
	kindScale   = "scale"
)

// RestartDeployment restarts an environment's pods: it marks the pod
// template with the time (values' restartedAt), and Argo CD rolls the pods
// one at a time, as for a deploy. The next deploy leaves the mark out.
func (d *Deploys) RestartDeployment(ctx context.Context, req *connect.Request[forgev1.RestartDeploymentRequest]) (*connect.Response[forgev1.RestartDeploymentResponse], error) {
	svc, env, v, err := d.actionTarget(ctx, req.Msg.GetName(), ActDeployRestart)
	if err != nil {
		return nil, err
	}
	at := d.o.Now().UTC().Format(time.RFC3339)
	change := func(v *deployValues) error {
		if v.Chart != nil {
			return errRunsChart(svc, env)
		}
		v.RestartedAt = at
		return nil
	}
	if err := change(&v); err != nil {
		return nil, err
	}
	preview := d.deploymentFrom(svc, env, v, "", AppStatus{})
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.RestartDeploymentResponse{Deployment: preview}), nil
	}
	msg := fmt.Sprintf("restart %s in %s (%s)\n\nNew pods, the same deploy.\n", svc, env, actorOf(ctx))
	op, err := d.startValuesChange(ctx, kindRestart, svc, env, msg, change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.RestartDeploymentResponse{Operation: op, Deployment: preview}), nil
}

// ScaleDeployment sets an environment's pod count until the next deploy,
// promote or rollback, which writes forge.yaml's again. An autoscaled
// deployment refuses it: KEDA would undo it.
func (d *Deploys) ScaleDeployment(ctx context.Context, req *connect.Request[forgev1.ScaleDeploymentRequest]) (*connect.Response[forgev1.ScaleDeploymentResponse], error) {
	n := int(req.Msg.GetReplicas())
	if n < 1 || n > manifest.MaxReplicas {
		return nil, invalid(fmt.Sprintf("replicas %d must be 1 to %d", n, manifest.MaxReplicas))
	}
	svc, env, v, err := d.actionTarget(ctx, req.Msg.GetName(), ActDeployScale)
	if err != nil {
		return nil, err
	}
	previous := v.Replicas
	change := func(v *deployValues) error {
		switch {
		case v.Chart != nil:
			return errRunsChart(svc, env)
		case v.Autoscaling != nil:
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"%s%s/%s is autoscaled (%d to %d pods): change run.autoscaling in forge.yaml and deploy", deployNamePrefix, svc, env, v.Autoscaling.Min, v.Autoscaling.Max))
		}
		v.Replicas = n
		return nil
	}
	if err := change(&v); err != nil {
		return nil, err
	}
	preview := d.deploymentFrom(svc, env, v, "", AppStatus{})
	resp := &forgev1.ScaleDeploymentResponse{Deployment: preview, PreviousReplicas: int32(min(previous, manifest.MaxReplicas))} //nolint:gosec // bounded
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(resp), nil
	}
	msg := fmt.Sprintf("scale %s in %s to %d (%s)\n\nUntil the next deploy, which runs forge.yaml's run.replicas.\n", svc, env, n, actorOf(ctx))
	op, err := d.startValuesChange(ctx, kindScale, svc, env, msg, change)
	if err != nil {
		return nil, err
	}
	resp.Operation = op
	return connect.NewResponse(resp), nil
}

// actionTarget checks a restart's or a scale's caller and target and
// returns what runs there now.
func (d *Deploys) actionTarget(ctx context.Context, name, action string) (svc, env string, v deployValues, err error) {
	if err = d.ready(); err != nil {
		return
	}
	if svc, env, err = parseDeployName(name); err != nil {
		return
	}
	if err = authorize(ctx, action, servicePrefix+svc); err != nil {
		return
	}
	if err = d.requireEnv(ctx, svc, env); err != nil {
		return
	}
	v, _, err = d.current(ctx, svc, env)
	return
}

func errRunsChart(svc, env string) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"%s%s/%s runs a chart, whose own values decide its pods: Forge cannot restart or scale them", deployNamePrefix, svc, env))
}

// startValuesChange locks svc/env and starts an operation that applies
// change to the environment's values at the deploy repo's head, commits them
// with message and waits for Argo CD to sync and the pods to be healthy.
func (d *Deploys) startValuesChange(ctx context.Context, kind, svc, env, message string, change func(*deployValues) error) (*forgev1.Operation, error) {
	target := svc + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.claimIn(ctx, svc, env); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, kind, deployNamePrefix+target, actorOf(ctx), func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		if p, ok := ctx.Value(opNameKey{}).(string); ok {
			message += "Operation: " + p + "\n"
		}
		return d.changeValues(ctx, svc, env, message, change, emit)
	})
	d.running[target] = op.GetName()
	return op, nil
}

// changeValues commits change(values) on the deploy repo's head, reading
// the values at that same head (a deploy of another service may move it),
// and waits for Argo CD.
func (d *Deploys) changeValues(ctx context.Context, svc, env, message string, change func(*deployValues) error, emit func(string, string)) error {
	path := valuesPath(svc, env)
	var commit string
	var content []byte
	for attempt := 0; ; attempt++ {
		if attempt == 3 {
			return &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; try again"}
		}
		if attempt > 0 {
			emit("commit", "the deploy repo moved; retrying on the new head")
		}
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return fmt.Errorf("deploy repo head: %w", err)
		}
		data, err := d.o.Repo.Read(ctx, path, head)
		if errors.Is(err, github.ErrNotFound) {
			return &OpError{Code: "not_deployed", Message: fmt.Sprintf("%s%s/%s was taken down", deployNamePrefix, svc, env)}
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		v, err := parseValues(data)
		if err != nil {
			return err
		}
		if err := change(&v); err != nil {
			return &OpError{Code: "failed_precondition", Message: connectMessage(err)}
		}
		content = v.marshal()
		commit, err = d.o.Repo.CommitFile(ctx, head, tree, path, content, message)
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	emit("commit", "forge-deploys "+shortSHA(commit))
	err := d.awaitSync(ctx, svc, env, commit, content, emit)
	if ctx.Err() != nil {
		return &OpError{Code: OpCodeCancelled, Message: fmt.Sprintf("cancelled; the commit %s stands and Argo CD keeps applying it", shortSHA(commit))}
	}
	return err
}

// connectMessage is err's message without Connect's code prefix.
func connectMessage(err error) string {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}
