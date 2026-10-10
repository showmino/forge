// forge-runner is what a resource run's Kubernetes Job executes. It takes
// its settings from the environment:
//
//	FORGE_RUNNER_API       forge-api's runner listener, e.g. http://forge-api-runner.forge.svc:8081 (required)
//	FORGE_RUN_TOKEN_FILE   file holding the run's token (default /var/run/forge/run/token)
//	FORGE_RUN_DEADLINE     RFC 3339 time the run must have ended by (optional)
//	FORGE_MODULE_DIR       the OpenTofu module (default /opt/forge/module)
//	FORGE_WORK_DIR         a writable directory (default /tmp/forge-run)
//	FORGE_TOFU             the tofu binary (default /usr/local/bin/tofu)
//	TF_CLI_CONFIG_FILE     OpenTofu's CLI configuration (the provider mirror)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/runner"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func options() (runner.Options, error) {
	api := os.Getenv("FORGE_RUNNER_API")
	if api == "" {
		return runner.Options{}, fmt.Errorf("FORGE_RUNNER_API is required")
	}
	tokenFile := env("FORGE_RUN_TOKEN_FILE", "/var/run/forge/run/token")
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return runner.Options{}, fmt.Errorf("read the run token: %w", err)
	}
	client := &run.Client{BaseURL: api}
	o := runner.Options{
		API: client, Token: strings.TrimSpace(string(token)), StateURL: client.StateURL(),
		Tofu:      env("FORGE_TOFU", "/usr/local/bin/tofu"),
		ModuleDir: env("FORGE_MODULE_DIR", "/opt/forge/module"),
		WorkDir:   env("FORGE_WORK_DIR", "/tmp/forge-run"),
		CLIConfig: os.Getenv("TF_CLI_CONFIG_FILE"),
		Log:       slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	if o.Token == "" {
		return runner.Options{}, fmt.Errorf("the run token file %s is empty", tokenFile)
	}
	if d := os.Getenv("FORGE_RUN_DEADLINE"); d != "" {
		if o.Deadline, err = time.Parse(time.RFC3339, d); err != nil {
			return runner.Options{}, fmt.Errorf("FORGE_RUN_DEADLINE %q is not an RFC 3339 time", d)
		}
	}
	return o, nil
}

func main() { os.Exit(realMain()) }

func realMain() int {
	o, err := options()
	if err == nil {
		err = os.MkdirAll(o.WorkDir, 0o700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge-runner:", err)
		return 2
	}
	// SIGTERM (the Job was deleted) stops OpenTofu cleanly and still reports.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runner.Run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, "forge-runner:", err)
		return 1
	}
	return 0
}
