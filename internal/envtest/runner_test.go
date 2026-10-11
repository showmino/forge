package envtest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/run"
)

// runAPI is a run.API that serves one piece of work and records the rest.
type runAPI struct {
	mu      sync.Mutex
	work    run.Work
	state   []byte
	lock    string
	events  []string
	results []run.Result
	done    chan struct{}
}

func newRunAPI(w run.Work) *runAPI { return &runAPI{work: w, done: make(chan struct{}, 4)} }

func (a *runAPI) Claim(context.Context, string) (*run.Work, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.work
	return &w, nil
}

func (a *runAPI) Event(_ context.Context, _, step, message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, step+": "+message)
	return nil
}

func (a *runAPI) Complete(_ context.Context, _ string, r run.Result) error {
	a.mu.Lock()
	a.results = append(a.results, r)
	a.mu.Unlock()
	a.done <- struct{}{}
	return nil
}

func (a *runAPI) ReadState(context.Context, string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, nil
}

func (a *runAPI) WriteState(_ context.Context, _, lockID string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lock != lockID {
		return run.ErrLocked
	}
	a.state = data
	return nil
}

func (a *runAPI) LockState(_ context.Context, _, lockID string, _ []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lock = lockID
	return nil, nil
}

func (a *runAPI) UnlockState(context.Context, string, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lock = ""
	return nil
}

// last waits for the next result.
func (a *runAPI) last(t *testing.T) run.Result {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not complete")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.results[len(a.results)-1]
}

func vars(resources ...run.Resource) run.Variables {
	return run.Variables{Service: "payments", Environment: "staging", Namespace: "payments-staging", Resources: resources}
}

// actions returns "<action> <name>" for every change of a fake plan.
func actions(t *testing.T, planJSON []byte) string {
	t.Helper()
	var doc struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, planJSON)
	}
	var out []string
	for _, rc := range doc.ResourceChanges {
		out = append(out, strings.Join(rc.Change.Actions, "+")+" "+rc.Address)
	}
	return strings.Join(out, ", ")
}

func TestRunnerPlansAndApplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	main5 := run.Resource{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}
	api := newRunAPI(run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Variables: vars(main5)})
	r := &Runner{API: api}
	t.Cleanup(r.Close)

	step := func(mode string, w run.Work) run.Result {
		t.Helper()
		api.mu.Lock()
		api.work = w
		api.mu.Unlock()
		if err := r.Start(ctx, run.Spec{Plan: w.Plan, Mode: mode, Service: "payments", Env: "staging", Token: "tok"}); err != nil {
			t.Fatal(err)
		}
		return api.last(t)
	}

	// Nothing exists: the plan creates the database.
	plan := step(run.ModePlan, run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Variables: vars(main5)})
	if plan.Error != "" || actions(t, plan.PlanJSON) != `create kubernetes_manifest.database["main"]` || len(plan.PlanFile) == 0 {
		t.Fatalf("first plan: %+v", plan)
	}
	if !strings.Contains(string(plan.PlanJSON), `"storageClass":"longhorn-retain"`) || !strings.Contains(string(plan.PlanJSON), `"namespace":"payments-staging"`) {
		t.Errorf("plan JSON: %s", plan.PlanJSON)
	}
	if len(r.Databases()) != 0 {
		t.Errorf("a plan must change nothing: %v", r.Databases())
	}

	// Applying it makes the database, writes the state and names the Secret.
	applied := step(run.ModeApply, run.Work{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModeApply, Variables: vars(main5), PlanFile: plan.PlanFile})
	if applied.Error != "" || applied.Secrets["main"] != "main-app" {
		t.Fatalf("apply: %+v", applied)
	}
	if got := r.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}
	if !strings.Contains(string(api.state), `"serial":1`) || api.lock != "" {
		t.Errorf("state %s, lock %q", api.state, api.lock)
	}

	// The same settings again: nothing to do.
	same := step(run.ModePlan, run.Work{Plan: "resourcePlans/bbbbbbbbbbbb", Mode: run.ModePlan, Variables: vars(main5)})
	if actions(t, same.PlanJSON) != `no-op kubernetes_manifest.database["main"]` {
		t.Errorf("unchanged plan: %s", same.PlanJSON)
	}

	// More storage is an update; an empty list deletes.
	main10 := main5
	main10.StorageGi = 10
	grow := step(run.ModePlan, run.Work{Plan: "resourcePlans/cccccccccccc", Mode: run.ModePlan, Variables: vars(main10)})
	if actions(t, grow.PlanJSON) != `update kubernetes_manifest.database["main"]` {
		t.Errorf("grow plan: %s", grow.PlanJSON)
	}
	step(run.ModeApply, run.Work{Plan: "resourcePlans/cccccccccccc", Mode: run.ModeApply, Variables: vars(main10), PlanFile: grow.PlanFile})
	if got := r.Databases(); !slices.Equal(got, []string{"payments-staging/main 10Gi x1"}) {
		t.Errorf("after the update: %v", got)
	}
	if !strings.Contains(string(api.state), `"serial":2`) {
		t.Errorf("state after two applies: %s", api.state)
	}
	gone := step(run.ModePlan, run.Work{Plan: "resourcePlans/dddddddddddd", Mode: run.ModePlan, Variables: vars()})
	if actions(t, gone.PlanJSON) != `delete kubernetes_manifest.database["main"]` {
		t.Errorf("delete plan: %s", gone.PlanJSON)
	}
	removed := step(run.ModeApply, run.Work{Plan: "resourcePlans/dddddddddddd", Mode: run.ModeApply, Variables: vars(), PlanFile: gone.PlanFile})
	if len(r.Databases()) != 0 || len(removed.Secrets) != 0 {
		t.Errorf("after the delete: %v %v", r.Databases(), removed.Secrets)
	}

	// A run reports its result first and is marked finished a moment later, as
	// a Job is: wait for the status instead of reading it at once.
	var st run.Status
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if st, err = r.Status(ctx, "resourcePlans/dddddddddddd", run.ModeApply); err != nil || st.Done {
			break
		}
	}
	if err != nil || !st.Done || st.Failed {
		t.Errorf("status of a finished run: %+v %v", st, err)
	}
	if _, err := r.Status(ctx, "resourcePlans/none", run.ModePlan); err == nil {
		t.Error("status of an unknown run must fail")
	}
	if n := len(r.Started()); n != 7 {
		t.Errorf("%d runs started, want 7", n)
	}
}

func TestRunnerKnobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := run.Spec{Plan: "resourcePlans/aaaaaaaaaaaa", Mode: run.ModePlan, Token: "tok"}
	work := run.Work{Plan: spec.Plan, Mode: run.ModePlan, Variables: vars(run.Resource{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1})}
	waitDone := func(r *Runner) run.Status {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := r.Status(ctx, spec.Plan, spec.Mode); err == nil && st.Done {
				return st
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("the run did not end")
		return run.Status{}
	}

	t.Run("a start error", func(t *testing.T) {
		r := &Runner{API: newRunAPI(work), StartErr: errors.New("no nodes")}
		if err := r.Start(ctx, spec); err == nil || err.Error() != "no nodes" {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a failing plan reports its error", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, FailPlan: "tofu: boom"}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if res := api.last(t); res.Error != "tofu: boom" || res.PlanJSON != nil {
			t.Errorf("result: %+v", res)
		}
		if st := waitDone(r); st.Failed {
			t.Errorf("a reported failure is a clean exit: %+v", st)
		}
	})
	t.Run("a crash ends without a result", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, Crash: "OOMKilled"}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if st := waitDone(r); !st.Failed || st.Log != "OOMKilled" || len(api.results) != 0 {
			t.Errorf("status %+v, results %v", st, api.results)
		}
	})
	t.Run("a silent run ends cleanly without a result", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, Silent: true}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if st := waitDone(r); st.Failed || len(api.results) != 0 {
			t.Errorf("status %+v, results %v", st, api.results)
		}
	})
	t.Run("a hanging run ends only when stopped", func(t *testing.T) {
		r := &Runner{API: newRunAPI(work), Hang: true}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		time.Sleep(20 * time.Millisecond)
		if st, _ := r.Status(ctx, spec.Plan, spec.Mode); st.Done {
			t.Fatalf("it ended by itself: %+v", st)
		}
		if err := r.Stop(ctx, spec.Plan, spec.Mode); err != nil {
			t.Fatal(err)
		}
		if st := waitDone(r); !st.Failed {
			t.Errorf("a stopped run: %+v", st)
		}
	})
	t.Run("a given plan and storage class", func(t *testing.T) {
		api := newRunAPI(work)
		r := &Runner{API: api, PlanJSON: []byte("not json")}
		t.Cleanup(r.Close)
		_ = r.Start(ctx, spec)
		if res := api.last(t); string(res.PlanJSON) != "not json" || string(res.PlanFile) != "not json" {
			t.Errorf("result: %+v", res)
		}
		api2 := newRunAPI(work)
		r2 := &Runner{API: api2, StorageClass: "local-path"}
		t.Cleanup(r2.Close)
		_ = r2.Start(ctx, spec)
		if res := api2.last(t); !strings.Contains(string(res.PlanJSON), `"storageClass":"local-path"`) {
			t.Errorf("plan: %s", res.PlanJSON)
		}
	})
}
