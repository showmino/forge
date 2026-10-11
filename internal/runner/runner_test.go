package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/run"
)

// fakeTofu is a shell script that plays the tofu binary: it logs each call,
// writes a plan file on "plan", and prints what the real commands print.
// FAKE_FAIL names a subcommand that fails; FAKE_SLOW one that waits until it
// gets SIGTERM and then "saves its state" before exiting.
const fakeTofu = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/calls"
env | grep '^TF_' | sort > "$FAKE_DIR/env.$1"
if [ "$1" = "$FAKE_SLOW" ]; then
  trap 'echo flushed > "$FAKE_DIR/flushed"; echo "Interrupt received" >&2; exit 1' TERM
  echo started > "$FAKE_DIR/started"
  while :; do sleep 0.05; done
fi
if [ "$1" = "$FAKE_FAIL" ]; then
  echo "working on it"
  echo "Error: no capacity for $TF_HTTP_PASSWORD" >&2
  exit 1
fi
case "$1" in
  init) echo "OpenTofu has been successfully initialized!" ;;
  plan) cp forge.auto.tfvars.json "$FAKE_DIR/vars.json"; printf PLANFILE > plan.bin; echo "Plan: 1 to add, 0 to change, 0 to destroy." ;;
  show) echo "a warning" >&2; printf '{"resource_changes":[]}' ;;
  apply) cp plan.bin "$FAKE_DIR/applied"; ls > "$FAKE_DIR/files"; echo "Apply complete! Resources: 1 added, 0 changed, 0 destroyed." ;;
  output) printf '{"secrets":{"sensitive":false,"type":["object",{"main":"string"}],"value":{"main":"main-app"}}}' ;;
esac
`

// api records what the runner asks of forge-api.
type api struct {
	mu          sync.Mutex
	work        run.Work
	claimErr    error
	completeErr error
	// completeErrs are what the first reports answer, one each.
	completeErrs []error
	events       []string
	results      []run.Result
}

func (a *api) Claim(context.Context, string) (*run.Work, error) {
	if a.claimErr != nil {
		return nil, a.claimErr
	}
	w := a.work
	return &w, nil
}

func (a *api) Event(_ context.Context, _, step, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, step)
	return nil
}

func (a *api) Complete(ctx context.Context, _ string, r run.Result) error {
	if ctx.Err() != nil {
		return ctx.Err() // the report must not use a cancelled context
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results = append(a.results, r)
	if len(a.completeErrs) > 0 {
		err := a.completeErrs[0]
		a.completeErrs = a.completeErrs[1:]
		return err
	}
	return a.completeErr
}

const token = "tok-0123456789abcdef"

// setup returns options for a run against the fake tofu, and the directory
// the fake writes its notes to.
func setup(t *testing.T, a *api, env ...string) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	module := filepath.Join(dir, "module")
	for _, d := range []string{fake, module} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "tofu")
	if err := os.WriteFile(bin, []byte(fakeTofu), 0o755); err != nil { //nolint:gosec // a test script must be executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "main.tf"), []byte("# module\n"), 0o444); err != nil { //nolint:gosec // read-only on purpose
		t.Fatal(err)
	}
	return Options{
		API: a, Token: token, StateURL: "http://forge-api-runner:8081/runner/v1/state",
		Tofu: bin, ModuleDir: module, WorkDir: filepath.Join(dir, "work"), CLIConfig: "/opt/forge/tofurc",
		Env:   append([]string{"PATH=" + os.Getenv("PATH"), "FAKE_DIR=" + fake}, env...),
		Grace: 5 * time.Second, ReportFor: 300 * time.Millisecond, ReportWait: 5 * time.Millisecond,
	}, fake
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func variables() run.Variables {
	return run.Variables{Service: "payments", Environment: "staging", Namespace: "payments-staging",
		Resources: []run.Resource{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}}
}

func TestPlanRun(t *testing.T) {
	t.Parallel()
	a := &api{work: run.Work{Plan: "resourcePlans/0123456789ab", Mode: run.ModePlan, Variables: variables()}}
	o, fake := setup(t, a)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(a.results) != 1 {
		t.Fatalf("%d results, want 1", len(a.results))
	}
	res := a.results[0]
	// Only stdout of `show -json` is the plan JSON; its warnings are not.
	if res.Error != "" || string(res.PlanFile) != "PLANFILE" || string(res.PlanJSON) != `{"resource_changes":[]}` || res.Secrets != nil {
		t.Errorf("result: %+v", res)
	}
	wantCalls := "init -input=false -no-color\nplan -input=false -no-color -lock-timeout=60s -out=plan.bin\nshow -json plan.bin\n"
	if got := read(t, filepath.Join(fake, "calls")); got != wantCalls {
		t.Errorf("tofu calls:\n%s\nwant:\n%s", got, wantCalls)
	}
	if got := read(t, filepath.Join(fake, "vars.json")); got != `{"service":"payments","environment":"staging","namespace":"payments-staging","resources":[{"name":"main","type":"postgres","storage_gi":5,"instances":1}]}` {
		t.Errorf("variables: %s", got)
	}
	env := read(t, filepath.Join(fake, "env.plan"))
	for _, want := range []string{
		"TF_HTTP_ADDRESS=http://forge-api-runner:8081/runner/v1/state\n", "TF_HTTP_LOCK_ADDRESS=http://forge-api-runner:8081/runner/v1/state\n",
		"TF_HTTP_UNLOCK_ADDRESS=http://forge-api-runner:8081/runner/v1/state\n", "TF_HTTP_USERNAME=forge-runner\n", "TF_HTTP_PASSWORD=" + token + "\n",
		"TF_IN_AUTOMATION=1\n", "TF_INPUT=0\n", "TF_CLI_CONFIG_FILE=/opt/forge/tofurc\n",
		"TF_HTTP_RETRY_MAX=8\n", "TF_HTTP_RETRY_WAIT_MIN=2\n", "TF_HTTP_RETRY_WAIT_MAX=20\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("OpenTofu's environment lacks %q:\n%s", want, env)
		}
	}
	if !slices.Equal(a.events, []string{"init", "plan"}) {
		t.Errorf("events: %v", a.events)
	}
	// The module directory was copied, not used in place: it may be read-only.
	if _, err := os.Stat(filepath.Join(o.ModuleDir, ".terraform")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the module directory was written to: %v", err)
	}
	if got := read(t, filepath.Join(o.WorkDir, "module", "main.tf")); got != "# module\n" {
		t.Errorf("copied module: %q", got)
	}
}

func TestApplyRun(t *testing.T) {
	t.Parallel()
	a := &api{work: run.Work{Plan: "resourcePlans/0123456789ab", Mode: run.ModeApply, Variables: variables(), PlanFile: []byte("SAVED")}}
	o, fake := setup(t, a)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	res := a.results[0]
	if res.Error != "" || res.Secrets["main"] != "main-app" || len(res.Secrets) != 1 || res.PlanFile != nil || res.PlanJSON != nil {
		t.Errorf("result: %+v", res)
	}
	if got := read(t, filepath.Join(fake, "calls")); got != "init -input=false -no-color\napply -input=false -no-color -lock-timeout=60s plan.bin\noutput -json\n" {
		t.Errorf("tofu calls:\n%s", got)
	}
	// Exactly the saved plan is applied, and no variables are written beside it.
	if got := read(t, filepath.Join(fake, "applied")); got != "SAVED" {
		t.Errorf("applied plan file: %q", got)
	}
	if files := read(t, filepath.Join(fake, "files")); strings.Contains(files, "tfvars") {
		t.Errorf("an apply wrote variables:\n%s", files)
	}
	if !slices.Equal(a.events, []string{"init", "apply"}) {
		t.Errorf("events: %v", a.events)
	}
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	plan := run.Work{Plan: "resourcePlans/0123456789ab", Mode: run.ModePlan, Variables: variables()}
	apply := run.Work{Plan: "resourcePlans/0123456789ab", Mode: run.ModeApply, PlanFile: []byte("SAVED")}
	for name, tc := range map[string]struct {
		work run.Work
		fail string
		want string
	}{
		"init fails":   {plan, "init", "init failed\n"},
		"plan fails":   {plan, "plan", "plan failed\n"},
		"show fails":   {plan, "show", "show failed\n"},
		"apply fails":  {apply, "apply", "apply failed\n"},
		"output fails": {apply, "output", "output failed\n"},
		"unknown mode": {run.Work{Mode: "destroy"}, "", `unknown mode "destroy"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &api{work: tc.work}
			o, _ := setup(t, a, "FAKE_FAIL="+tc.fail)
			// A failed run is still a reported result: Run itself succeeds.
			if err := Run(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if len(a.results) != 1 {
				t.Fatalf("%d results, want 1", len(a.results))
			}
			res := a.results[0]
			if !strings.HasPrefix(res.Error, tc.want) || res.PlanFile != nil || res.Secrets != nil {
				t.Errorf("result: %+v, want an error starting %q", res, tc.want)
			}
			if tc.fail != "" && !strings.Contains(res.Error, "Error: no capacity for [run token]") {
				t.Errorf("the error lacks the end of OpenTofu's output: %q", res.Error)
			}
			if strings.Contains(res.Error, token) {
				t.Errorf("the error holds the token: %q", res.Error)
			}
		})
	}
}

func TestRunCannotReport(t *testing.T) {
	t.Parallel()
	work := run.Work{Mode: run.ModePlan, Variables: variables()}

	a := &api{claimErr: errors.New("the run token is not valid")}
	o, fake := setup(t, a)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "claim the run: the run token is not valid") {
		t.Errorf("a refused claim: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fake, "calls")); !errors.Is(err, os.ErrNotExist) || len(a.results) != 0 {
		t.Error("OpenTofu ran, or a result was sent, without a claim")
	}

	a = &api{work: work, completeErr: errors.New("forge-api is away")}
	o, _ = setup(t, a)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "report the run: forge-api is away") {
		t.Errorf("a failed report: %v", err)
	}

	// forge-api restarts while the runner reports: it tries until it is back.
	a = &api{work: work, completeErrs: []error{errors.New("connection refused"), &run.HTTPError{Status: 503, Message: "starting"}}}
	o, _ = setup(t, a)
	if err := Run(context.Background(), o); err != nil || len(a.results) != 3 {
		t.Errorf("a report during a restart: %v after %d tries", err, len(a.results))
	}
	// An answer that refuses the result is final: no second try.
	a = &api{work: work, completeErr: &run.HTTPError{Status: 401, Message: "the run token is not valid"}}
	o, _ = setup(t, a)
	if err := Run(context.Background(), o); err == nil || len(a.results) != 1 {
		t.Errorf("a refused report: %v after %d tries", err, len(a.results))
	}

	// A module directory that is missing is a reported failure, not a crash.
	a = &api{work: work}
	o, _ = setup(t, a)
	o.ModuleDir = filepath.Join(t.TempDir(), "nope")
	if err := Run(context.Background(), o); err != nil || len(a.results) != 1 || !strings.HasPrefix(a.results[0].Error, "copy the module:") {
		t.Errorf("a missing module: %v %+v", err, a.results)
	}
}

// waitFor waits until path exists.
func waitFor(t *testing.T, path string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s did not appear", path)
		}
	}
}

// Told to stop (the Job was deleted), the runner lets OpenTofu save its
// state and still reports.
func TestRunStopped(t *testing.T) {
	t.Parallel()
	a := &api{work: run.Work{Mode: run.ModeApply, PlanFile: []byte("SAVED")}}
	o, fake := setup(t, a, "FAKE_SLOW=apply")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	waitFor(t, filepath.Join(fake, "started"))
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(fake, "flushed")); got != "flushed\n" {
		t.Errorf("OpenTofu was not given the chance to save its state: %q", got)
	}
	if len(a.results) != 1 || !strings.HasPrefix(a.results[0].Error, "apply was stopped\n") || !strings.Contains(a.results[0].Error, "Interrupt received") {
		t.Errorf("results: %+v", a.results)
	}
}

// Before the run's deadline the runner stops OpenTofu itself, while its
// token still works.
func TestRunDeadline(t *testing.T) {
	t.Parallel()
	a := &api{work: run.Work{Mode: run.ModeApply, PlanFile: []byte("SAVED")}}
	o, fake := setup(t, a, "FAKE_SLOW=apply")
	o.Margin = time.Hour
	o.Deadline = time.Now().Add(time.Hour + 300*time.Millisecond) // 300ms of run time
	start := time.Now()
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping took %s", took)
	}
	if got := read(t, filepath.Join(fake, "flushed")); got != "flushed\n" {
		t.Errorf("flushed: %q", got)
	}
	if len(a.results) != 1 || !strings.HasPrefix(a.results[0].Error, "apply was stopped: the run was about to pass its deadline\n") {
		t.Errorf("results: %+v", a.results)
	}
}

func TestSecretsOf(t *testing.T) {
	t.Parallel()
	got, err := secretsOf([]byte(`{"secrets":{"sensitive":false,"value":{"main":"main-app","b":"b-app"}}}`))
	if err != nil || got["main"] != "main-app" || got["b"] != "b-app" || len(got) != 2 {
		t.Errorf("%v %v", got, err)
	}
	if got, err := secretsOf([]byte(`{}`)); err != nil || len(got) != 0 {
		t.Errorf("no outputs: %v %v", got, err)
	}
	if _, err := secretsOf([]byte(`tofu`)); err == nil {
		t.Error("not JSON: want an error")
	}
}
