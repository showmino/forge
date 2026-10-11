// Package runner is what a resource run's Job executes: it fetches its work
// from forge-api with the run's token, runs OpenTofu on the module, and
// hands back the plan or the result. OpenTofu keeps its state in forge-api
// through the "http" backend, with the same token.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/showmino/forge/internal/run"
)

// API is the part of forge-api's runner API the program calls itself.
type API interface {
	Claim(ctx context.Context, token string) (*run.Work, error)
	Event(ctx context.Context, token, step, message string) error
	Complete(ctx context.Context, token string, r run.Result) error
}

// Options configure one run.
type Options struct {
	API API
	// Token is the run's token. It is never logged or put in a result.
	Token string
	// StateURL is the address of OpenTofu's "http" state backend.
	StateURL string
	// Tofu is the path of the tofu binary.
	Tofu string
	// ModuleDir holds the OpenTofu module. It may be read-only.
	ModuleDir string
	// WorkDir is a writable directory; the module is copied into it.
	WorkDir string
	// CLIConfig is the OpenTofu CLI configuration file (the provider
	// mirror); "" leaves OpenTofu's default.
	CLIConfig string
	// Deadline is when forge-api gives the run up and its token dies; zero
	// means none.
	Deadline time.Time
	// Margin is how long before Deadline OpenTofu is told to stop, so it can
	// still save its state (default 60s).
	Margin time.Duration
	// Grace is how long OpenTofu gets to stop after SIGTERM before it is
	// killed (default 45s).
	Grace time.Duration
	// ReportFor is how long the runner keeps trying to hand its result to
	// forge-api when forge-api is away (default 3m), and ReportWait the
	// first pause between two tries (default 2s, doubling up to 20s).
	ReportFor, ReportWait time.Duration
	// Env is the environment OpenTofu starts from (default os.Environ()).
	Env []string
	Log *slog.Logger
}

const (
	planFile  = "plan.bin"
	varsFile  = "forge.auto.tfvars.json"
	tailBytes = 4 << 10 // of OpenTofu's output kept for an error message
)

// Run does one run and reports its result to forge-api. A run that fails
// (OpenTofu errors, a timeout) is still a reported result: Run returns an
// error only when there was nothing to do or the result could not be handed back.
func Run(ctx context.Context, o Options) error {
	if o.Margin <= 0 {
		o.Margin = time.Minute
	}
	if o.Grace <= 0 {
		o.Grace = 45 * time.Second
	}
	if o.ReportFor <= 0 {
		o.ReportFor = 3 * time.Minute
	}
	if o.ReportWait <= 0 {
		o.ReportWait = 2 * time.Second
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	work, err := o.API.Claim(ctx, o.Token)
	if err != nil {
		return fmt.Errorf("claim the run: %w", err)
	}
	o.Log.Info("run claimed", "plan", work.Plan, "mode", work.Mode)
	res := o.execute(ctx, work)
	if res.Error != "" {
		o.Log.Error("run failed", "plan", work.Plan, "mode", work.Mode, "error", res.Error)
	}
	// The result is reported also when the run was told to stop.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ReportFor)
	defer cancel()
	if err := o.report(rctx, res); err != nil {
		return fmt.Errorf("report the run: %w", err)
	}
	o.Log.Info("run reported", "plan", work.Plan, "mode", work.Mode)
	return nil
}

// report hands res to forge-api. It tries again while forge-api cannot be
// reached or answers with a server error, so a restart of forge-api does not
// lose the result of an apply; an answer that refuses the result ends it.
func (o Options) report(ctx context.Context, res run.Result) error {
	for wait := o.ReportWait; ; wait = min(2*wait, 20*time.Second) {
		err := o.API.Complete(ctx, o.Token, res)
		var refused *run.HTTPError
		if err == nil || (errors.As(err, &refused) && refused.Status < http.StatusInternalServerError) {
			return err
		}
		o.Log.Warn("the result could not be reported; trying again", "error", err, "in", wait)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// execute runs OpenTofu for work. Failures end up in Result.Error.
func (o Options) execute(ctx context.Context, work *run.Work) run.Result {
	fail := func(format string, a ...any) run.Result {
		return run.Result{Error: o.scrub(fmt.Sprintf(format, a...))}
	}
	if !o.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, o.Deadline.Add(-o.Margin))
		defer cancel()
	}
	dir := filepath.Join(o.WorkDir, "module")
	if err := os.RemoveAll(dir); err != nil {
		return fail("prepare the work directory: %v", err)
	}
	if err := os.CopyFS(dir, os.DirFS(o.ModuleDir)); err != nil {
		return fail("copy the module: %v", err)
	}
	t := &tofu{o: o, dir: dir}

	o.event(ctx, "init", "initialising OpenTofu")
	if _, err := t.run(ctx, false, "init", "-input=false", "-no-color"); err != nil {
		return fail("%s", t.failure(ctx, "init", err))
	}
	switch work.Mode {
	case run.ModePlan:
		vars, err := json.Marshal(work.Variables)
		if err != nil {
			return fail("encode the variables: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, varsFile), vars, 0o600); err != nil {
			return fail("write the variables: %v", err)
		}
		o.event(ctx, "plan", "planning")
		if _, err := t.run(ctx, false, "plan", "-input=false", "-no-color", "-lock-timeout=60s", "-out="+planFile); err != nil {
			return fail("%s", t.failure(ctx, "plan", err))
		}
		planJSON, err := t.run(ctx, true, "show", "-json", planFile)
		if err != nil {
			return fail("%s", t.failure(ctx, "show", err))
		}
		plan, err := os.ReadFile(filepath.Join(dir, planFile))
		if err != nil {
			return fail("read the plan file: %v", err)
		}
		return run.Result{PlanFile: plan, PlanJSON: planJSON}
	case run.ModeApply:
		// The saved plan carries its own variables: none are written.
		if err := os.WriteFile(filepath.Join(dir, planFile), work.PlanFile, 0o600); err != nil {
			return fail("write the plan file: %v", err)
		}
		o.event(ctx, "apply", "applying the saved plan")
		if _, err := t.run(ctx, false, "apply", "-input=false", "-no-color", "-lock-timeout=60s", planFile); err != nil {
			return fail("%s", t.failure(ctx, "apply", err))
		}
		out, err := t.run(ctx, true, "output", "-json")
		if err != nil {
			return fail("%s", t.failure(ctx, "output", err))
		}
		secrets, err := secretsOf(out)
		if err != nil {
			return fail("read the outputs: %v", err)
		}
		return run.Result{Secrets: secrets}
	default:
		return fail("unknown mode %q", work.Mode)
	}
}

// secretsOf reads the module's "secrets" output from `tofu output -json`.
func secretsOf(out []byte) (map[string]string, error) {
	var outputs struct {
		Secrets struct {
			Value map[string]string `json:"value"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		return nil, err
	}
	return outputs.Secrets.Value, nil
}

// event sends a progress line; a failure to send it only gets logged.
func (o Options) event(ctx context.Context, step, message string) {
	if err := o.API.Event(ctx, o.Token, step, message); err != nil {
		o.Log.Warn("progress not sent", "step", step, "error", err)
	}
}

// scrub removes the token from text, should OpenTofu ever echo it.
func (o Options) scrub(s string) string {
	if o.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, o.Token, "[run token]")
}

// tofu runs OpenTofu in dir and keeps the end of its output.
type tofu struct {
	o   Options
	dir string

	mu   sync.Mutex
	tail []byte
}

// Write keeps the last tailBytes written.
func (t *tofu) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tail = append(t.tail, p...)
	if len(t.tail) > tailBytes {
		t.tail = t.tail[len(t.tail)-tailBytes:]
	}
	return len(p), nil
}

// run runs one OpenTofu command. With capture it returns stdout; all other
// output goes to the tail. When ctx ends, OpenTofu gets SIGTERM and Grace to
// stop, so an interrupted apply still saves its state.
func (t *tofu) run(ctx context.Context, capture bool, args ...string) ([]byte, error) {
	t.mu.Lock()
	t.tail = t.tail[:0]
	t.mu.Unlock()
	cmd := exec.CommandContext(ctx, t.o.Tofu, args...) //nolint:gosec // the binary and arguments are the runner's own
	cmd.Dir = t.dir
	cmd.Env = append(append([]string{}, t.o.Env...),
		"HOME="+t.o.WorkDir, "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		// The state backend is forge-api: ride out a restart of it (about
		// two minutes of tries) instead of losing the state of an apply.
		"TF_HTTP_RETRY_MAX=8", "TF_HTTP_RETRY_WAIT_MIN=2", "TF_HTTP_RETRY_WAIT_MAX=20",
		"TF_HTTP_ADDRESS="+t.o.StateURL, "TF_HTTP_LOCK_ADDRESS="+t.o.StateURL, "TF_HTTP_UNLOCK_ADDRESS="+t.o.StateURL,
		"TF_HTTP_USERNAME="+run.StateUser, "TF_HTTP_PASSWORD="+t.o.Token)
	if t.o.CLIConfig != "" {
		cmd.Env = append(cmd.Env, "TF_CLI_CONFIG_FILE="+t.o.CLIConfig)
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = t.o.Grace
	var stdout bytes.Buffer
	cmd.Stderr = t
	if capture {
		cmd.Stdout = &stdout
	} else {
		cmd.Stdout = t
	}
	err := cmd.Run()
	return stdout.Bytes(), err
}

// failure describes a failed step: why, and the end of OpenTofu's output.
func (t *tofu) failure(ctx context.Context, step string, err error) string {
	t.mu.Lock()
	tail := strings.TrimSpace(string(t.tail))
	t.mu.Unlock()
	why := step + " failed"
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		why = step + " was stopped: the run was about to pass its deadline"
	case ctx.Err() != nil:
		why = step + " was stopped"
	}
	if tail == "" {
		return fmt.Sprintf("%s: %v", why, err)
	}
	return why + "\n" + tail
}
