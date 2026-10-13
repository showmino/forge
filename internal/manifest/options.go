package manifest

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The run options the deploy chart turns into Kubernetes objects. Each is
// optional: without them a service runs one pod with the namespace's default
// resources, in both environments.

// Limits of the run options.
const (
	MaxReplicas = 10
	MaxSecrets  = 10
	MaxJobs     = 5
)

// The namespace's ResourceQuota for the app's limits (deploy/argocd/chart/
// templates/limits.yaml), and the LimitRange's default limits per container.
var (
	quotaCPU      = resource.MustParse("4")
	quotaMemory   = resource.MustParse("4Gi")
	defaultCPU    = resource.MustParse("500m")
	defaultMemory = resource.MustParse("512Mi")
)

// Resources are the app container's requests and limits.
type Resources struct {
	Requests *Quantities `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *Quantities `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// Quantities are Kubernetes quantities such as "500m" and "512Mi".
type Quantities struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// Secret is a 1Password item synced into the namespace; Env maps an
// environment variable to a field of the item.
type Secret struct {
	Name string            `yaml:"name" json:"name"`
	Item string            `yaml:"item" json:"item"`
	Env  map[string]string `yaml:"env" json:"env"`
}

// Vault is the vault of the item path vaults/<vault>/items/<item>.
func (s Secret) Vault() string {
	parts := strings.Split(s.Item, "/")
	if len(parts) != 4 {
		return ""
	}
	return parts[1]
}

// Job is a cron job: the service's image running Command on Schedule.
type Job struct {
	Name     string   `yaml:"name" json:"name"`
	Schedule string   `yaml:"schedule" json:"schedule"`
	Command  []string `yaml:"command" json:"command"`
}

// Autoscaling lets production grow from replicas to Max pods on CPU.
type Autoscaling struct {
	Max int `yaml:"max" json:"max"`
}

var (
	envName    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	itemPath   = regexp.MustCompile(`^vaults/[A-Za-z0-9._-]+/items/[A-Za-z0-9._-]+$`)
	fieldName  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	cronField  = regexp.MustCompile(`^[0-9A-Za-z*/,?-]+$`)
	vaultName  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	cronMacros = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"}
	// Set by Forge itself.
	reservedEnv = []string{"PORT", "DATABASE_URL"}
)

// ValidVault reports whether s is a 1Password vault name run.secrets can use.
func ValidVault(s string) bool { return vaultName.MatchString(s) }

// ProductionReplicas is run.replicas, or 1. Staging always runs one.
func (m *Manifest) ProductionReplicas() int {
	if m.Run != nil && m.Run.Replicas > 0 {
		return m.Run.Replicas
	}
	return 1
}

// validateOptions checks the run options on their own; which vaults are
// allowed is the platform's to say, at deploy time.
func (r *Run) validateOptions() error {
	if r.Replicas < 0 || r.Replicas > MaxReplicas {
		return fmt.Errorf("run.replicas %d must be 1 to %d", r.Replicas, MaxReplicas)
	}
	pods := max(r.Replicas, 1)
	if a := r.Autoscaling; a != nil {
		if a.Max > MaxReplicas || a.Max <= pods {
			return fmt.Errorf("run.autoscaling.max %d must be more than run.replicas (%d) and at most %d", a.Max, pods, MaxReplicas)
		}
		pods = a.Max
	}
	cpu, memory, err := r.Resources.check()
	if err != nil {
		return err
	}
	used := map[string]string{}
	use := func(name, where string) error {
		if !envName.MatchString(name) {
			return fmt.Errorf("%s: %q must be an environment variable name in capitals, such as LOG_LEVEL", where, name)
		}
		if slices.Contains(reservedEnv, name) {
			return fmt.Errorf("%s: %s is set by Forge", where, name)
		}
		if prev, ok := used[name]; ok {
			return fmt.Errorf("%s: %s is already set by %s", where, name, prev)
		}
		used[name] = where
		return nil
	}
	for _, k := range sortedKeys(r.Env) {
		if err := use(k, "run.env"); err != nil {
			return err
		}
	}
	if len(r.Secrets) > MaxSecrets {
		return fmt.Errorf("run.secrets has %d items; at most %d", len(r.Secrets), MaxSecrets)
	}
	names := map[string]bool{}
	for i, s := range r.Secrets {
		where := fmt.Sprintf("run.secrets[%d]", i)
		if !ValidID(s.Name) || names[s.Name] {
			return fmt.Errorf("%s.name %q must be a lowercase DNS label, used once", where, s.Name)
		}
		names[s.Name] = true
		if !itemPath.MatchString(s.Item) {
			return fmt.Errorf("%s.item %q must be vaults/<vault>/items/<item> (letters, digits, '.', '_', '-')", where, s.Item)
		}
		if len(s.Env) == 0 {
			return fmt.Errorf("%s.env must name at least one variable and the item's field it gets", where)
		}
		for _, k := range sortedKeys(s.Env) {
			if err := use(k, where+".env"); err != nil {
				return err
			}
			if !fieldName.MatchString(s.Env[k]) {
				return fmt.Errorf("%s.env.%s: %q must be a field of the item", where, k, s.Env[k])
			}
		}
	}
	if len(r.Jobs) > MaxJobs {
		return fmt.Errorf("run.jobs has %d jobs; at most %d", len(r.Jobs), MaxJobs)
	}
	names = map[string]bool{}
	for i, j := range r.Jobs {
		where := fmt.Sprintf("run.jobs[%d]", i)
		// The CronJob is <service>-<name>, and its Jobs get 11 more characters.
		if !ValidID(j.Name) || len(j.Name) > 30 || names[j.Name] {
			return fmt.Errorf("%s.name %q must be a lowercase DNS label of at most 30 characters, used once", where, j.Name)
		}
		names[j.Name] = true
		if !validSchedule(j.Schedule) {
			return fmt.Errorf("%s.schedule %q must be five cron fields, such as \"0 3 * * *\", or a macro such as @daily", where, j.Schedule)
		}
		if len(j.Command) == 0 || j.Command[0] == "" {
			return fmt.Errorf("%s.command must be the program and its arguments, such as [node, scripts/cleanup.js]", where)
		}
	}
	// Every pod the app may run at once must fit the namespace's quota.
	pods += len(r.Jobs)
	for _, c := range []struct {
		what        string
		each, quota resource.Quantity
	}{{"cpu", cpu, quotaCPU}, {"memory", memory, quotaMemory}} {
		total := c.each.DeepCopy()
		total.Mul(int64(pods))
		if total.Cmp(c.quota) > 0 {
			return fmt.Errorf("%d pods (replicas, autoscaling and jobs) of %s %s each need %s, more than the namespace's %s",
				pods, c.each.String(), c.what, total.String(), c.quota.String())
		}
	}
	return nil
}

// check parses the quantities and returns the limits each pod counts
// against the quota: the ones set, or the namespace's defaults.
func (rs *Resources) check() (cpu, memory resource.Quantity, err error) {
	cpu, memory = defaultCPU, defaultMemory
	if rs == nil {
		return cpu, memory, nil
	}
	parse := func(where, s string) (*resource.Quantity, error) {
		if s == "" {
			return nil, nil
		}
		q, err := resource.ParseQuantity(s)
		if err != nil || q.Sign() <= 0 {
			return nil, fmt.Errorf("run.resources.%s %q must be a quantity such as 500m (cpu) or 512Mi (memory)", where, s)
		}
		return &q, nil
	}
	var req, lim [2]*resource.Quantity
	for i, kind := range []string{"cpu", "memory"} {
		pick := func(q *Quantities) string {
			if q == nil {
				return ""
			}
			return []string{q.CPU, q.Memory}[i]
		}
		if req[i], err = parse("requests."+kind, pick(rs.Requests)); err != nil {
			return cpu, memory, err
		}
		if lim[i], err = parse("limits."+kind, pick(rs.Limits)); err != nil {
			return cpu, memory, err
		}
		if req[i] != nil && lim[i] != nil && req[i].Cmp(*lim[i]) > 0 {
			return cpu, memory, fmt.Errorf("run.resources.requests.%s is more than run.resources.limits.%s", kind, kind)
		}
	}
	if lim[0] != nil {
		cpu = *lim[0]
	}
	if lim[1] != nil {
		memory = *lim[1]
	}
	return cpu, memory, nil
}

func validSchedule(s string) bool {
	if slices.Contains(cronMacros, s) {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return false
	}
	for _, f := range fields {
		if !cronField.MatchString(f) {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
