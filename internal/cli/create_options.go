package cli

import (
	"strings"

	"github.com/spf13/pflag"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
)

// runFlags are forge create's run options, as forge.yaml's run section has them.
type runFlags struct {
	replicas, autoscalingMax int
	resources, env, secrets  []string
	jobs                     []string
	networkOnly              bool
	stagingNetworkOnly       bool
}

func (r *runFlags) register(fl *pflag.FlagSet) {
	fl.IntVar(&r.replicas, "replicas", 0, "run option: production's pod count (staging runs one)")
	fl.StringSliceVar(&r.resources, "resources", nil, "run option: requests.cpu=100m,requests.memory=128Mi,limits.cpu=500m,limits.memory=512Mi")
	fl.StringArrayVar(&r.env, "env", nil, "run option: an environment variable as NAME=value (repeatable)")
	fl.StringArrayVar(&r.secrets, "secret", nil, "run option: a 1Password item as name=vaults/<vault>/items/<item>:VAR=field,VAR2=field (repeatable)")
	fl.StringArrayVar(&r.jobs, "job", nil, `run option: a cron job as name=<schedule>:<command>, e.g. 'cleanup=0 3 * * *:node cleanup.js' (repeatable)`)
	fl.IntVar(&r.autoscalingMax, "autoscaling-max", 0, "run option: let production grow on CPU up to this many pods")
	fl.BoolVar(&r.networkOnly, "internal", false, "reach the service only on the platform's network, with no public address (run.domain: none)")
	fl.BoolVar(&r.stagingNetworkOnly, "internal-staging", false, "reach staging only on the platform's network; production stays public (run.staging.public: false)")
}

// options returns the run options set by the flags, nil when none is. The
// Forge API checks them by forge.yaml's rules and the template's offer.
func (r *runFlags) options() (*forgev1.RunOptions, error) {
	ro := &forgev1.RunOptions{Replicas: int32(min(max(r.replicas, -1), 1000)), AutoscalingMax: int32(min(max(r.autoscalingMax, -1), 1000)), NetworkOnly: r.networkOnly, StagingNetworkOnly: r.stagingNetworkOnly} //nolint:gosec // clamped
	set := r.replicas != 0 || r.autoscalingMax != 0 || r.networkOnly || r.stagingNetworkOnly
	if len(r.resources) > 0 {
		set = true
		ro.Resources = &forgev1.RunResources{}
		for _, kv := range r.resources {
			k, v, ok := strings.Cut(kv, "=")
			to := map[string]*string{
				"requests.cpu": &ro.Resources.RequestsCpu, "requests.memory": &ro.Resources.RequestsMemory,
				"limits.cpu": &ro.Resources.LimitsCpu, "limits.memory": &ro.Resources.LimitsMemory,
			}[k]
			if !ok || to == nil {
				return nil, exitcode.Usagef("--resources %q must be requests.cpu, requests.memory, limits.cpu or limits.memory =<quantity>", kv)
			}
			*to = v
		}
	}
	for _, kv := range r.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, exitcode.Usagef("--env %q must be NAME=value", kv)
		}
		if ro.Env == nil {
			ro.Env = map[string]string{}
		}
		ro.Env[k], set = v, true
	}
	for _, s := range r.secrets {
		name, rest, ok1 := strings.Cut(s, "=")
		item, vars, ok2 := strings.Cut(rest, ":")
		if !ok1 || !ok2 || name == "" || item == "" || vars == "" {
			return nil, exitcode.Usagef("--secret %q must be name=vaults/<vault>/items/<item>:VAR=field,VAR2=field", s)
		}
		sec := &forgev1.RunSecret{Name: name, Item: item, Env: map[string]string{}}
		for _, kv := range strings.Split(vars, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" || v == "" {
				return nil, exitcode.Usagef("--secret %q: %q must be VAR=field", s, kv)
			}
			sec.Env[k] = v
		}
		ro.Secrets, set = append(ro.Secrets, sec), true
	}
	for _, j := range r.jobs {
		name, rest, ok1 := strings.Cut(j, "=")
		schedule, command, ok2 := strings.Cut(rest, ":")
		if !ok1 || !ok2 || name == "" || len(strings.Fields(command)) == 0 {
			return nil, exitcode.Usagef("--job %q must be name=<schedule>:<command>, e.g. 'cleanup=0 3 * * *:node cleanup.js'", j)
		}
		ro.Jobs, set = append(ro.Jobs, &forgev1.RunJob{Name: name, Schedule: strings.TrimSpace(schedule), Command: strings.Fields(command)}), true
	}
	if !set {
		return nil, nil
	}
	return ro, nil
}
