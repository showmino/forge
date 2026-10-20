package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// runEnd is how a run ended.
type runEnd struct {
	// code is "" when the run worked, else the operation error code.
	code    string
	message string
	res     run.Result
}

// planTarget returns the service id and "<service>/<env>" of a plan.
func planTarget(p *forgev1.ResourcePlan) (svc, target string) {
	svc = strings.TrimPrefix(p.GetService(), servicePrefix)
	return svc, svc + "/" + p.GetEnv()
}

// outcomeTemplate is the outcome event of the run a call starts, without its
// result: the call's own event so far, or forge-api's when there is none.
func outcomeTemplate(ctx context.Context, action, op, service, target string) *forgev1.AuditEvent {
	ev := &forgev1.AuditEvent{Action: action, Actor: &forgev1.AuditActor{Kind: actorSystem}}
	if rec := auditFrom(ctx); rec != nil {
		ev = rec.snapshot()
	}
	ev.Kind, ev.Operation, ev.Service, ev.Target = auditOutcome, op, service, target
	ev.Details, ev.Client, ev.Result, ev.Name, ev.Time = nil, nil, nil, "", nil
	return ev
}

// startRun starts the operation that runs rec in mode, and stores rec with
// the operation's name and the outcome template. write, when set, runs in
// the same transaction first; its error aborts the run. r.mu must be held.
func (r *Resources) startRun(ctx context.Context, rec *store.PlanRecord, mode, token string, write func(tx store.ResourceTx) error) (*forgev1.Operation, error) {
	kind := opResourcePlan
	if mode == run.ModeApply {
		kind = opResourceApply
	}
	planName := rec.Plan.GetName()
	svc, target := planTarget(rec.Plan)
	spec := run.Spec{Plan: planName, Mode: mode, Service: svc, Env: rec.Plan.GetEnv(), Token: token, Deadline: rec.Deadline}
	lr := &liveRun{ended: make(chan struct{})}
	// The work waits at the gate until the record is stored.
	gate := make(chan error, 1)
	op := r.o.Operations.Start(withoutOutcome(ctx), kind, resourceNamePrefix+target, actorOf(ctx), func(ctx context.Context, emit func(string, string)) error {
		if err := <-gate; err != nil {
			return err // nothing was claimed: the target is not this run's to free
		}
		defer r.unlock(target, planName)
		r.mu.Lock()
		lr.emit = emit
		r.live[planName] = lr
		r.mu.Unlock()
		return r.watchRun(ctx, spec, lr, emit)
	})
	rec.Plan.Operation = op.GetName()
	tmpl, err := protojson.Marshal(outcomeTemplate(ctx, kind, op.GetName(), rec.Plan.GetService(), resourceNamePrefix+target))
	if err == nil {
		rec.Audit = tmpl
		err = r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
			if write != nil {
				if err := write(tx); err != nil {
					return err
				}
			}
			return tx.PutResourcePlan(ctx, rec)
		})
	}
	if err != nil {
		gate <- &OpError{Code: OpCodeInternal, Message: "the run could not be recorded"}
		return nil, storeErr(err)
	}
	r.running[target] = op.GetName()
	gate <- nil
	return op, nil
}

func (r *Resources) unlock(target, planName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, target)
	delete(r.live, planName)
}

// cancelledMessage starts the message of a run that ended by a cancel.
const cancelledMessage = "the run was cancelled"

func (r *Resources) setCancelling(planName string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.cancelling[planName] = true
	} else {
		delete(r.cancelling, planName)
	}
}

func (r *Resources) isCancelling(planName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelling[planName]
}

// liveEmit adds an event to the operation watching plan's run, if there is one.
func (r *Resources) liveEmit(planName, step, message string) {
	r.mu.Lock()
	lr := r.live[planName]
	r.mu.Unlock()
	if lr != nil && lr.emit != nil {
		lr.emit(step, message)
	}
}

// signal tells the operation watching plan's run that the run ended.
func (r *Resources) signal(planName string) {
	r.mu.Lock()
	lr := r.live[planName]
	r.mu.Unlock()
	if lr != nil {
		lr.once.Do(func() { close(lr.ended) })
	}
}

// watchRun starts the runner and waits until the run ended: by the runner's
// Complete, by the runner dying, by the deadline, or by a cancel.
func (r *Resources) watchRun(ctx context.Context, spec run.Spec, lr *liveRun, emit func(string, string)) error {
	if spec.Mode == run.ModeApply && r.o.Namespaces != nil {
		if err := r.o.Namespaces.EnsureServiceNamespace(ctx, spec.Service, spec.Env); err != nil {
			if ctx.Err() != nil {
				return r.failRun(ctx, spec, OpCodeCancelled, cancelledMessage)
			}
			return r.failRun(ctx, spec, opCodeLaunchFailed, "namespace "+spec.Service+"-"+spec.Env+": "+clip(err.Error()))
		}
		emit("namespace", spec.Service+"-"+spec.Env)
	}
	if err := r.o.Launcher.Start(ctx, spec); err != nil {
		if ctx.Err() != nil {
			return r.failRun(ctx, spec, OpCodeCancelled, cancelledMessage)
		}
		return r.failRun(ctx, spec, opCodeLaunchFailed, "the runner could not be started: "+clip(err.Error()))
	}
	emit("launch", "the "+spec.Mode+" runner started")
	// The same deadline as the run's token: the namespace and the launch
	// have already used part of the run's time.
	deadline := time.NewTimer(spec.Deadline.Sub(r.o.Now()))
	defer deadline.Stop()
	tick := time.NewTicker(r.o.Poll)
	defer tick.Stop()
	stop := func() {
		if err := r.o.Launcher.Stop(context.WithoutCancel(ctx), spec.Plan, spec.Mode); err != nil {
			r.o.Log.Warn("stopping a resource runner", "plan", spec.Plan, "mode", spec.Mode, "error", err)
		}
	}
	statusErrs := 0      // consecutive failed status reads
	var exited time.Time // when the runner was first seen to have ended cleanly
	for {
		select {
		case <-lr.ended:
			return r.runError(ctx, spec.Plan, spec.Mode)
		case <-ctx.Done():
			// The runner is stopped before the cancel is recorded, so OpenTofu,
			// shutting down, can still save its state with the run's token.
			r.setCancelling(spec.Plan, true)
			defer r.setCancelling(spec.Plan, false)
			stop()
			return r.failRun(ctx, spec, OpCodeCancelled, cancelledMessage)
		case <-deadline.C:
			stop()
			return r.failRun(ctx, spec, opCodeRunTimeout, fmt.Sprintf("the run did not finish within %s", r.o.RunTimeout))
		case <-tick.C:
			st, err := r.o.Launcher.Status(ctx, spec.Plan, spec.Mode)
			if err != nil {
				if statusErrs++; statusErrs == 1 {
					r.o.Log.Warn("reading a resource runner's status", "plan", spec.Plan, "mode", spec.Mode, "error", err)
				}
				continue
			}
			statusErrs = 0
			if !st.Done {
				continue
			}
			if st.Stuck {
				stop() // it never started: nothing else will take it away
				return r.failRun(ctx, spec, opCodeLaunchFailed, "the runner did not start: "+runText(st.Log))
			}
			if st.Failed {
				return r.failRun(ctx, spec, opCodeRunnerFailed, "the runner failed: "+runText(st.Log))
			}
			// It ended cleanly: its result is on its way, or it never sent one.
			if exited.IsZero() {
				exited = time.Now()
			} else if time.Since(exited) >= r.o.Grace {
				return r.failRun(ctx, spec, opCodeRunnerFailed, "the runner ended without a result")
			}
		}
	}
}

// runText makes a runner's output fit for a message: its lines joined by
// "; ", and the END of it kept, because OpenTofu puts the error last. The
// caller still cleans it (failRun and Complete go through auditText).
func runText(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	s = strings.ToValidUTF8(strings.Join(lines, "; "), "\uFFFD")
	const max = auditMessageMax - 40 // room for the prefix failRun's caller adds
	if n := utf8.RuneCountInString(s); n > max {
		skip := n - max
		for i := range s {
			if skip == 0 {
				return "…" + s[i:]
			}
			skip--
		}
	}
	return s
}

// failRun ends the run as failed and returns the operation's error. When
// the run had already ended another way, that ending decides.
func (r *Resources) failRun(ctx context.Context, spec run.Spec, code, message string) error {
	message = auditText(message, auditMessageMax)
	won, err := r.endRun(ctx, spec.Plan, spec.Mode, runEnd{code: code, message: message})
	if err != nil {
		return &OpError{Code: OpCodeInternal, Message: "the run's end could not be recorded: " + clip(err.Error())}
	}
	if won {
		return &OpError{Code: code, Message: message}
	}
	return r.runError(ctx, spec.Plan, spec.Mode)
}

// runError is the operation error of a run that ended, read from the plan.
func (r *Resources) runError(ctx context.Context, planName, mode string) error {
	rec, err := r.o.Store.GetResourcePlan(context.WithoutCancel(ctx), planName)
	if err != nil {
		return &OpError{Code: OpCodeInternal, Message: "the plan could not be read: " + clip(err.Error())}
	}
	p := rec.Plan
	switch p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
		if mode == run.ModePlan {
			return nil
		}
	case forgev1.ResourcePlan_STATE_APPLIED:
		return nil
	case forgev1.ResourcePlan_STATE_DENIED:
		return &OpError{Code: OpCodePolicyDenied, Message: deniedMessage(p.GetPolicy())}
	case forgev1.ResourcePlan_STATE_FAILED, forgev1.ResourcePlan_STATE_APPLY_FAILED:
		if strings.HasPrefix(p.GetMessage(), cancelledMessage) {
			return &OpError{Code: OpCodeCancelled, Message: p.GetMessage()}
		}
		if strings.Contains(p.GetMessage(), notReadyText) {
			return &OpError{Code: opCodeNotReady, Message: p.GetMessage()}
		}
		return &OpError{Code: opCodeRunFailed, Message: p.GetMessage()}
	}
	return &OpError{Code: OpCodeInternal, Message: fmt.Sprintf("%s is %s after its %s run", planName, planStateWord(p.GetState()), mode)}
}

// checkReady turns the success of an apply with no changes, which only puts
// Forge's records right, into a failure while a database is not healthy: such
// an apply runs no OpenTofu wait, so it must not call a database ready that a
// cut short create left starting, or broken. Health that cannot be read does
// not block it.
func (r *Resources) checkReady(ctx context.Context, p *forgev1.ResourcePlan, end *runEnd) {
	if r.o.Health == nil {
		return
	}
	svc, _ := planTarget(p)
	for _, s := range p.GetResources() {
		h, err := r.o.Health.DatabaseHealth(ctx, svc, p.GetEnv(), s.GetName())
		if err != nil {
			r.o.Log.Warn("reading a database's health before a repair", "plan", p.GetName(), "resource", s.GetName(), "error", err)
			continue
		}
		if h.GetReadyInstances() < s.GetInstances() {
			end.code = opCodeNotReady
			end.message = auditText(fmt.Sprintf("%s is not healthy yet (%s, %d of %d ready): plan and apply again once it is",
				s.GetName(), h.GetPhase(), h.GetReadyInstances(), s.GetInstances()), auditMessageMax)
			return
		}
	}
}

// planStateWord is a state as people read it: "apply_failed".
func planStateWord(s forgev1.ResourcePlan_State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATE_"))
}

// deniedMessage is the first refusal, and how many more there are.
func deniedMessage(p *forgev1.PolicyResult) string {
	deny := p.GetDeny()
	switch len(deny) {
	case 0:
		return "denied by policy"
	case 1:
		return deny[0]
	default:
		return fmt.Sprintf("%s (and %d more)", deny[0], len(deny)-1)
	}
}

// templateActor is the actor of a stored outcome template: who started the run.
func templateActor(template []byte) policy.Actor {
	ev := &forgev1.AuditEvent{}
	if protojson.Unmarshal(template, ev) != nil {
		return policy.Actor{}
	}
	return policy.Actor{Kind: ev.GetActor().GetKind(), Subject: ev.GetActor().GetSubject()}
}

// evaluate checks planJSON against policy for the plan in rec, asked for by actor.
func (r *Resources) evaluate(ctx context.Context, rec *store.PlanRecord, planJSON []byte, actor policy.Actor) (*forgev1.PolicyResult, error) {
	svc, _ := planTarget(rec.Plan)
	f := policy.Forge{Service: svc, Environment: rec.Plan.GetEnv(), AllowDestroy: rec.Plan.GetAllowDestroy(), Actor: actor}
	for _, s := range rec.Plan.GetResources() {
		f.Resources = append(f.Resources, policy.Resource{Name: s.GetName(), Type: s.GetType(), StorageGi: s.GetStorageGi(), Instances: s.GetInstances()})
	}
	res, err := r.o.Policy.Evaluate(ctx, planJSON, f)
	if err != nil {
		return nil, err
	}
	for _, msgs := range [][]string{res.Deny, res.Warn} {
		for i, m := range msgs {
			msgs[i] = auditText(m, auditMessageMax)
		}
	}
	return &forgev1.PolicyResult{Denied: res.Denied(), Deny: res.Deny, Warn: res.Warn, Rules: r.o.Policy.Fingerprint()}, nil
}

// planCheck is what a plan run's result turned out to be.
type planCheck struct {
	changes      []*forgev1.ResourceChange
	policy       *forgev1.PolicyResult
	stateVersion int64
}

// checkPlan reads and checks a plan run's result. A result that cannot be
// read or checked turns end into a failure: such a plan is never ready.
func (r *Resources) checkPlan(ctx context.Context, rec *store.PlanRecord, end *runEnd) planCheck {
	fail := func(format string, a ...any) planCheck {
		end.code, end.message = opCodeRunFailed, auditText(fmt.Sprintf(format, a...), auditMessageMax)
		return planCheck{}
	}
	res := end.res
	switch {
	case len(res.PlanFile) == 0 || len(res.PlanJSON) == 0:
		return fail("the runner returned no plan")
	case len(res.PlanFile) > maxRunResult || len(res.PlanJSON) > maxRunResult:
		return fail("the plan is larger than %d MiB", maxRunResult>>20)
	}
	changes, err := resource.Changes(res.PlanJSON)
	if err != nil {
		return fail("the plan could not be read: %v", err)
	}
	cleanChanges(changes)
	pol, err := r.evaluate(ctx, rec, res.PlanJSON, templateActor(rec.Audit))
	if err != nil {
		return fail("policy could not be evaluated: %v", err)
	}
	_, target := planTarget(rec.Plan)
	st, err := r.o.Store.GetResourceState(ctx, target)
	if err != nil {
		return fail("the state could not be read: %v", err)
	}
	return planCheck{changes: changes, policy: pol, stateVersion: st.Version}
}

// endRun moves a plan out of its running state for mode. It works once: the
// first caller changes the plan (and, after an apply, the resources), appends
// the outcome event and wakes the operation watching the run. It reports
// whether this call was that first one.
func (r *Resources) endRun(ctx context.Context, planName, mode string, end runEnd) (bool, error) {
	ctx = context.WithoutCancel(ctx)
	rec, err := r.o.Store.GetResourcePlan(ctx, planName)
	if err != nil {
		return false, err
	}
	if modeOf(rec.Plan.GetState()) != mode {
		return false, nil
	}
	var check planCheck
	if mode == run.ModePlan && end.code == "" {
		check = r.checkPlan(ctx, rec, &end)
	}
	if mode == run.ModeApply && end.code == "" && len(rec.Plan.GetChanges()) == 0 {
		r.checkReady(ctx, rec.Plan, &end)
	}
	now := timestamppb.New(r.o.Now())
	var final *store.PlanRecord
	err = r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if modeOf(cur.Plan.GetState()) != mode || cur.TokenHash != rec.TokenHash {
			return nil // it ended another way first
		}
		p := cur.Plan
		cur.TokenHash, cur.Deadline, cur.Audit = "", time.Time{}, nil
		cur.PlanFile, cur.PlanJSON = nil, nil
		switch {
		case mode == run.ModePlan && end.code != "":
			p.State, p.Message = forgev1.ResourcePlan_STATE_FAILED, end.message
		case mode == run.ModePlan && check.policy.GetDenied():
			p.State, p.Changes, p.Policy = forgev1.ResourcePlan_STATE_DENIED, check.changes, check.policy
		case mode == run.ModePlan:
			p.State, p.Changes, p.Policy = forgev1.ResourcePlan_STATE_PLANNED, check.changes, check.policy
			cur.PlanFile, cur.PlanJSON, cur.StateVersion = end.res.PlanFile, end.res.PlanJSON, check.stateVersion
			if len(p.GetChanges()) == 0 {
				if inSync, err := recordsMatch(ctx, tx, p); err == nil && !inSync {
					p.Message = repairMessage
				}
			}
		case end.code != "":
			p.State, p.Message = forgev1.ResourcePlan_STATE_APPLY_FAILED, end.message
			if err := failResources(ctx, tx, p, now); err != nil {
				return err
			}
		default:
			p.State, p.ApplyTime = forgev1.ResourcePlan_STATE_APPLIED, now
			if err := r.applyResources(ctx, tx, p, end.res.Secrets, now); err != nil {
				return err
			}
		}
		final = cur
		return tx.PutResourcePlan(ctx, cur)
	})
	if err != nil || final == nil {
		return false, err
	}
	r.appendOutcome(ctx, final.Plan, rec.Audit, mode, end)
	switch p := final.Plan; p.GetState() {
	case forgev1.ResourcePlan_STATE_PLANNED:
		r.liveEmit(planName, "policy", "passed; "+resource.Count(p.GetChanges()))
		if p.GetMessage() == repairMessage {
			r.liveEmit(planName, "records", repairMessage)
		}
	case forgev1.ResourcePlan_STATE_DENIED:
		r.liveEmit(planName, "policy", "denied: "+deniedMessage(p.GetPolicy()))
	case forgev1.ResourcePlan_STATE_APPLIED:
		r.liveEmit(planName, "apply", appliedHint(p))
	}
	r.signal(planName)
	return true, nil
}

// appliedHint tells the caller what an apply means for the app.
func appliedHint(p *forgev1.ResourcePlan) string {
	if len(p.GetResources()) > 0 {
		return "Deploy to give the app DATABASE_URL."
	}
	return "The next deploy drops DATABASE_URL."
}

// resourceName is "resources/<service>/<env>/<name>".
func resourceName(p *forgev1.ResourcePlan, name string) string {
	_, target := planTarget(p)
	return resourceNamePrefix + target + "/" + name
}

// applyResources makes Forge's records say what an applied plan declared:
// one ready record per declared resource, and no record for anything else in
// the target. It works from the declared resources, not from the plan's
// changes, so an apply also repairs records that a failed or interrupted
// apply left behind.
func (r *Resources) applyResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, secrets map[string]string, now *timestamppb.Timestamp) error {
	if p.GetService() == "" || p.GetEnv() == "" {
		return errNoTarget
	}
	list, err := tx.ListResources(ctx, p.GetService(), p.GetEnv())
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, s := range p.GetResources() {
		declared[resourceName(p, s.GetName())] = true
	}
	old := map[string]*forgev1.Resource{}
	for _, res := range list {
		old[res.GetName()] = res
		if !declared[res.GetName()] {
			if err := tx.DeleteResource(ctx, res.GetName()); err != nil {
				return err
			}
		}
	}
	for _, s := range p.GetResources() {
		name := resourceName(p, s.GetName())
		prev := old[name]
		// A run that returns no Secret for a resource leaves the name it had.
		secret := ""
		if _, returned := secrets[s.GetName()]; returned || prev.GetSecret() == "" {
			secret = r.secretName(name, secrets[s.GetName()])
		}
		if secret == "" {
			secret = prev.GetSecret()
		}
		if prev != nil && prev.GetState() == forgev1.Resource_STATE_READY && prev.GetSecret() == secret && proto.Equal(prev.GetSettings(), s) {
			continue
		}
		res := &forgev1.Resource{
			Name: name, Service: p.GetService(), Env: p.GetEnv(), Type: s.GetType(), Settings: s,
			State: forgev1.Resource_STATE_READY, Secret: secret,
			LastPlan: p.GetName(), CreateTime: now, UpdateTime: now,
		}
		if prev != nil && prev.GetCreateTime() != nil {
			res.CreateTime = prev.GetCreateTime()
		}
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// failResources marks the resources a failed apply touched.
func failResources(ctx context.Context, tx store.ResourceTx, p *forgev1.ResourcePlan, now *timestamppb.Timestamp) error {
	for _, c := range p.GetChanges() {
		res, err := tx.GetResource(ctx, resourceName(p, c.GetResource()))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		res.State, res.LastPlan, res.UpdateTime = forgev1.Resource_STATE_FAILED, p.GetName(), now
		if err := tx.PutResource(ctx, res); err != nil {
			return err
		}
	}
	return nil
}

// appendOutcome appends the run's outcome event, built from the template
// stored when the run started.
func (r *Resources) appendOutcome(ctx context.Context, p *forgev1.ResourcePlan, template []byte, mode string, end runEnd) {
	ev := &forgev1.AuditEvent{}
	if err := protojson.Unmarshal(template, ev); err != nil || ev.GetAction() == "" {
		action := opResourcePlan
		if mode == run.ModeApply {
			action = opResourceApply
		}
		_, target := planTarget(p)
		ev = &forgev1.AuditEvent{Action: action, Actor: &forgev1.AuditActor{Kind: actorSystem},
			Service: p.GetService(), Target: resourceNamePrefix + target, Operation: p.GetOperation()}
	}
	ev.Kind = auditOutcome
	verdict, changes := "", ""
	if pol := p.GetPolicy(); pol != nil {
		verdict, changes = "passed", resource.Count(p.GetChanges())
		if pol.GetDenied() {
			verdict = "denied"
		}
	}
	ev.Details = details("environment", p.GetEnv(), "plan", p.GetName(), "policy", verdict,
		"policy_rules", p.GetPolicy().GetRules(), "allow_destroy", flag(p.GetAllowDestroy()), "changes", changes)
	switch {
	case p.GetState() == forgev1.ResourcePlan_STATE_DENIED:
		ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(OpCodePolicyDenied+": "+deniedMessage(p.GetPolicy()), auditMessageMax)}
	case end.code == "":
		ev.Result = &forgev1.AuditResult{Code: "ok"}
	case end.code == OpCodeCancelled:
		ev.Result = &forgev1.AuditResult{Code: "cancelled", Message: auditText(end.message, auditMessageMax)}
	default:
		ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(end.code+": "+end.message, auditMessageMax)}
	}
	r.audit.Load().append(ctx, ev)
}

// The runner's API. Every call carries the run's token.

// runTokenError returns a new error each time: a *connect.Error is mutated
// lazily (Meta), so one shared value would race between handlers.
func runTokenError() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("the run token is not valid"))
}

// authRun returns the plan and mode of the run token belongs to. A token
// works only while its run is going and before the run's deadline.
func (r *Resources) authRun(ctx context.Context, token string) (*store.PlanRecord, string, error) {
	if token == "" {
		return nil, "", runTokenError()
	}
	rec, err := r.o.Store.ResourcePlanByToken(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", runTokenError()
	}
	if err != nil {
		return nil, "", storeErr(err)
	}
	mode := modeOf(rec.Plan.GetState())
	if mode == "" || !r.o.Now().Before(rec.Deadline) {
		return nil, "", runTokenError()
	}
	return rec, mode, nil
}

// Claim implements run.API.
// CheckRunToken reports whether token is the token of a run that is still
// going, and nothing else.
func (r *Resources) CheckRunToken(ctx context.Context, token string) error {
	_, _, err := r.authRun(ctx, token)
	return err
}

func (r *Resources) Claim(ctx context.Context, token string) (*run.Work, error) {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	svc, target := planTarget(rec.Plan)
	// One run at a time per target: a lock still held is a dead run's.
	if err := r.o.Store.UnlockResourceState(ctx, target, ""); err != nil {
		return nil, storeErr(err)
	}
	w := &run.Work{Plan: rec.Plan.GetName(), Mode: mode, Variables: run.Variables{
		Service: svc, Environment: rec.Plan.GetEnv(), Namespace: svc + "-" + rec.Plan.GetEnv(), Resources: []run.Resource{},
	}}
	for _, s := range rec.Plan.GetResources() {
		w.Variables.Resources = append(w.Variables.Resources, run.Resource{Name: s.GetName(), Type: s.GetType(), StorageGi: s.GetStorageGi(), Instances: s.GetInstances()})
	}
	if mode == run.ModeApply {
		w.PlanFile = rec.PlanFile
	}
	return w, nil
}

// Event implements run.API.
func (r *Resources) Event(ctx context.Context, token, step, message string) error {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	r.liveEmit(rec.Plan.GetName(), auditText(step, 32), auditText(message, auditMessageMax))
	return nil
}

// Complete implements run.API.
func (r *Resources) Complete(ctx context.Context, token string, res run.Result) error {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	end := runEnd{res: res}
	switch {
	case res.Error != "" && r.isCancelling(rec.Plan.GetName()):
		end.code, end.message = OpCodeCancelled, auditText(cancelledMessage+": "+runText(res.Error), auditMessageMax)
	case res.Error != "":
		end.code, end.message = opCodeRunFailed, auditText(runText(res.Error), auditMessageMax)
	}
	won, err := r.endRun(ctx, rec.Plan.GetName(), mode, end)
	if err != nil {
		return storeErr(err)
	}
	if !won {
		return runTokenError()
	}
	return nil
}

// ReadState implements run.API.
func (r *Resources) ReadState(ctx context.Context, token string) ([]byte, error) {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	_, target := planTarget(rec.Plan)
	st, err := r.o.Store.GetResourceState(ctx, target)
	return st.Data, storeErr(err)
}

// WriteState implements run.API.
func (r *Resources) WriteState(ctx context.Context, token, lockID string, data []byte) error {
	rec, mode, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	if mode != run.ModeApply {
		return precondition("a plan run may not write the state")
	}
	_, target := planTarget(rec.Plan)
	_, err = r.o.Store.PutResourceState(ctx, target, lockID, data)
	if errors.Is(err, store.ErrStateLocked) {
		return run.ErrLocked
	}
	return storeErr(err)
}

// LockState implements run.API.
func (r *Resources) LockState(ctx context.Context, token, lockID string, info []byte) ([]byte, error) {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return nil, err
	}
	_, target := planTarget(rec.Plan)
	holder, err := r.o.Store.LockResourceState(ctx, target, lockID, info)
	if errors.Is(err, store.ErrStateLocked) {
		return holder, run.ErrLocked
	}
	return nil, storeErr(err)
}

// UnlockState implements run.API.
func (r *Resources) UnlockState(ctx context.Context, token, lockID string) error {
	rec, _, err := r.authRun(ctx, token)
	if err != nil {
		return err
	}
	if lockID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("a state unlock needs the lock id"))
	}
	_, target := planTarget(rec.Plan)
	err = r.o.Store.UnlockResourceState(ctx, target, lockID)
	if errors.Is(err, store.ErrStateLocked) {
		return run.ErrLocked
	}
	return storeErr(err)
}

// cleanChanges makes the text a runner's plan put into changes safe to store
// and show: no control or bidi characters, and bounded.
func cleanChanges(changes []*forgev1.ResourceChange) {
	for _, c := range changes {
		c.Action, c.Resource, c.Type = auditText(c.Action, auditFieldMax), auditText(c.Resource, auditFieldMax), auditText(c.Type, auditFieldMax)
		for _, st := range []*forgev1.ResourceSettings{c.Before, c.After} {
			if st != nil {
				st.Name, st.Type = auditText(st.Name, auditFieldMax), auditText(st.Type, auditFieldMax)
			}
		}
	}
}

var secretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// secretName is the Secret name a runner returned for resource, when it is
// a valid Kubernetes name; otherwise "" and a log line. Forge stores the
// name only and never reads the Secret.
func (r *Resources) secretName(resource, name string) string {
	if secretNameRE.MatchString(name) {
		return name
	}
	r.o.Log.Warn("a resource run returned no usable Secret name", "resource", resource)
	return ""
}
