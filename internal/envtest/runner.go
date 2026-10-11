package envtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/showmino/forge/internal/run"
)

// Runner is a fake run.Launcher. It plays the runner and OpenTofu: a plan
// run compares the wanted resources with the databases it keeps in memory,
// in the shape `tofu show -json` gives a kubernetes_manifest; an apply run
// makes that plan's changes and writes the state. Set the knobs before Start.
type Runner struct {
	// API is forge-api's side of a run.
	API run.API
	// StartErr fails Start.
	StartErr error
	// FailPlan and FailApply make a run of that mode report this error.
	FailPlan, FailApply string
	// Crash makes a run end without reporting: Status says failed, with this log.
	Crash string
	// Stuck makes a run never start: Status says so with this text, and the
	// run stays until it is stopped.
	Stuck string
	// Hang keeps a run going until it is stopped.
	Hang bool
	// Silent makes a run end cleanly without reporting a result.
	Silent bool
	// StorageClass is what the fake module asks for (default longhorn-retain).
	StorageClass string
	// PlanJSON, when set, is handed back as the plan in place of a computed one.
	PlanJSON []byte

	mu        sync.Mutex
	databases map[string]run.Resource // "<namespace>/<name>"
	runs      map[string]*fakeRun     // "<plan>/<mode>"
	started   []run.Spec
	stopped   []string // the stuck runs that were stopped, "<plan>/<mode>"
	wg        sync.WaitGroup
}

type fakeRun struct {
	status run.Status
	stop   context.CancelFunc
}

// knobs are the Runner's settings as one run sees them.
type knobs struct {
	failPlan, failApply, crash, class string
	hang, silent                      bool
	planJSON                          []byte
	api                               run.API
}

// Start implements run.Launcher.
func (r *Runner) Start(_ context.Context, s run.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.StartErr != nil {
		return r.StartErr
	}
	if r.runs == nil {
		r.runs, r.databases = map[string]*fakeRun{}, map[string]run.Resource{}
	}
	k := knobs{failPlan: r.FailPlan, failApply: r.FailApply, crash: r.Crash, class: r.StorageClass,
		hang: r.Hang, silent: r.Silent, planJSON: r.PlanJSON, api: r.API}
	if k.class == "" {
		k.class = "longhorn-retain"
	}
	ctx, cancel := context.WithCancel(context.Background())
	fr := &fakeRun{stop: cancel}
	r.runs[s.Plan+"/"+s.Mode] = fr
	r.started = append(r.started, s)
	if r.Stuck != "" {
		fr.status = run.Status{Done: true, Failed: true, Stuck: true, Log: r.Stuck}
		return nil
	}
	r.wg.Go(func() {
		defer cancel()
		st := r.play(ctx, k, s)
		r.mu.Lock()
		fr.status = st
		r.mu.Unlock()
	})
	return nil
}

// Status implements run.Launcher.
func (r *Runner) Status(_ context.Context, plan, mode string) (run.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fr, ok := r.runs[plan+"/"+mode]
	if !ok {
		return run.Status{}, fmt.Errorf("no %s run for %s", mode, plan)
	}
	return fr.status, nil
}

// Stop implements run.Launcher.
func (r *Runner) Stop(_ context.Context, plan, mode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fr, ok := r.runs[plan+"/"+mode]; ok {
		if fr.status.Stuck {
			r.stopped = append(r.stopped, plan+"/"+mode)
		}
		fr.stop()
	}
	return nil
}

// Close stops every run and waits for them to end.
func (r *Runner) Close() {
	r.mu.Lock()
	for _, fr := range r.runs {
		fr.stop()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// StuckStopped is how many runs that never started were stopped.
func (r *Runner) StuckStopped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopped)
}

// Started returns the specs of every run started, in order.
func (r *Runner) Started() []run.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

// Databases returns what exists, sorted: "<namespace>/<name> <n>Gi x<instances>".
func (r *Runner) Databases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for key, d := range r.databases {
		out = append(out, fmt.Sprintf("%s %dGi x%d", key, d.StorageGi, d.Instances))
	}
	slices.Sort(out)
	return out
}

func failed(format string, a ...any) run.Status {
	return run.Status{Done: true, Failed: true, Log: fmt.Sprintf(format, a...)}
}

// play is one run, from start to exit.
func (r *Runner) play(ctx context.Context, k knobs, s run.Spec) run.Status {
	switch {
	case k.hang:
		<-ctx.Done()
		return failed("stopped")
	case k.crash != "":
		return failed("%s", k.crash)
	case k.silent:
		return run.Status{Done: true}
	}
	work, err := k.api.Claim(ctx, s.Token)
	if err != nil {
		return failed("claim: %v", err)
	}
	_ = k.api.Event(ctx, s.Token, "init", "OpenTofu initialised")
	var res run.Result
	if work.Mode == run.ModeApply {
		_ = k.api.Event(ctx, s.Token, "apply", "applying the saved plan")
		res = r.apply(ctx, k, s.Token, work)
	} else {
		_ = k.api.Event(ctx, s.Token, "plan", "planning")
		res = r.plan(k, work)
	}
	if err := k.api.Complete(ctx, s.Token, res); err != nil {
		return failed("complete: %v", err)
	}
	return run.Status{Done: true}
}

// object is the kubernetes_manifest value of a database.
func object(namespace string, d run.Resource, class string) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": d.Name, "namespace": namespace},
		"spec": map[string]any{
			"instances": d.Instances,
			"storage":   map[string]any{"size": fmt.Sprintf("%dGi", d.StorageGi), "storageClass": class},
		},
	}}
}

type fakeChange struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Change  struct {
		Actions []string `json:"actions"`
		Before  any      `json:"before"`
		After   any      `json:"after"`
	} `json:"change"`
}

func newChange(name string, actions []string, before, after any) fakeChange {
	c := fakeChange{Address: fmt.Sprintf("kubernetes_manifest.database[%q]", name), Type: "kubernetes_manifest", Name: "database"}
	c.Change.Actions, c.Change.Before, c.Change.After = actions, before, after
	return c
}

// plan compares what is wanted with what exists.
func (r *Runner) plan(k knobs, work *run.Work) run.Result {
	if k.failPlan != "" {
		return run.Result{Error: k.failPlan}
	}
	if k.planJSON != nil {
		return run.Result{PlanFile: k.planJSON, PlanJSON: k.planJSON}
	}
	ns := work.Variables.Namespace
	r.mu.Lock()
	defer r.mu.Unlock()
	changes := []fakeChange{}
	wanted := map[string]bool{}
	for _, d := range work.Variables.Resources {
		wanted[d.Name] = true
		cur, exists := r.databases[ns+"/"+d.Name]
		switch {
		case !exists:
			changes = append(changes, newChange(d.Name, []string{"create"}, nil, object(ns, d, k.class)))
		case cur != d:
			changes = append(changes, newChange(d.Name, []string{"update"}, object(ns, cur, k.class), object(ns, d, k.class)))
		default:
			changes = append(changes, newChange(d.Name, []string{"no-op"}, object(ns, cur, k.class), object(ns, cur, k.class)))
		}
	}
	for key, cur := range r.databases {
		if strings.HasPrefix(key, ns+"/") && !wanted[cur.Name] {
			changes = append(changes, newChange(cur.Name, []string{"delete"}, object(ns, cur, k.class), nil))
		}
	}
	slices.SortFunc(changes, func(a, b fakeChange) int { return strings.Compare(a.Address, b.Address) })
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": changes})
	if err != nil {
		return run.Result{Error: err.Error()}
	}
	return run.Result{PlanFile: b, PlanJSON: b}
}

// apply makes the changes of the saved plan and writes the state.
func (r *Runner) apply(ctx context.Context, k knobs, token string, work *run.Work) run.Result {
	if k.failApply != "" {
		return run.Result{Error: k.failApply}
	}
	var doc struct {
		ResourceChanges []struct {
			Change struct {
				Actions []string `json:"actions"`
				Before  *side    `json:"before"`
				After   *side    `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(work.PlanFile, &doc); err != nil {
		return run.Result{Error: "the saved plan is not readable: " + err.Error()}
	}
	lockID := "lock-" + work.Plan
	if _, err := k.api.LockState(ctx, token, lockID, []byte(`{"ID":"`+lockID+`"}`)); err != nil {
		return run.Result{Error: "lock the state: " + err.Error()}
	}
	defer func() { _ = k.api.UnlockState(ctx, token, lockID) }()
	old, err := k.api.ReadState(ctx, token)
	if err != nil {
		return run.Result{Error: "read the state: " + err.Error()}
	}
	var prev struct {
		Serial int `json:"serial"`
	}
	if len(old) > 0 {
		if err := json.Unmarshal(old, &prev); err != nil {
			return run.Result{Error: "the state is not readable: " + err.Error()}
		}
	}

	ns := work.Variables.Namespace
	r.mu.Lock()
	for _, rc := range doc.ResourceChanges {
		acts := rc.Change.Actions
		if slices.Contains(acts, "delete") && rc.Change.Before != nil {
			delete(r.databases, rc.Change.Before.key())
		}
		if (slices.Contains(acts, "create") || slices.Contains(acts, "update")) && rc.Change.After != nil {
			r.databases[rc.Change.After.key()] = rc.Change.After.resource()
		}
	}
	names := []string{}
	secrets := map[string]string{}
	for key, d := range r.databases {
		if strings.HasPrefix(key, ns+"/") {
			names = append(names, d.Name)
			secrets[d.Name] = d.Name + "-app"
		}
	}
	r.mu.Unlock()
	slices.Sort(names)

	state, err := json.Marshal(map[string]any{"version": 4, "serial": prev.Serial + 1, "resources": names})
	if err != nil {
		return run.Result{Error: err.Error()}
	}
	if err := k.api.WriteState(ctx, token, lockID, state); err != nil {
		if errors.Is(err, run.ErrLocked) {
			return run.Result{Error: "the state is locked by another run"}
		}
		return run.Result{Error: "write the state: " + err.Error()}
	}
	return run.Result{Secrets: secrets}
}

// side is the before or after of a fake change.
type side struct {
	Manifest struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Instances int32 `json:"instances"`
			Storage   struct {
				Size string `json:"size"`
			} `json:"storage"`
		} `json:"spec"`
	} `json:"manifest"`
}

func (s *side) key() string { return s.Manifest.Metadata.Namespace + "/" + s.Manifest.Metadata.Name }

func (s *side) resource() run.Resource {
	var gi int32
	_, _ = fmt.Sscanf(s.Manifest.Spec.Storage.Size, "%dGi", &gi)
	return run.Resource{Name: s.Manifest.Metadata.Name, Type: "postgres", StorageGi: gi, Instances: s.Manifest.Spec.Instances}
}
