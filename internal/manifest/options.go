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

// The namespace's ResourceQuota (deploy/argocd/chart/templates/limits.yaml),
// the LimitRange's defaults for a container that sets nothing, and one
// database instance (deploy/runner/module: requests 50m/256Mi, a 512Mi
// limit and the default CPU limit).
var (
	quota      = podSize{q("2"), q("2Gi"), q("4"), q("4Gi")}
	quotaPods  = 15
	appDefault = podSize{q("50m"), q("64Mi"), q("500m"), q("512Mi")}
	dbInstance = podSize{q("50m"), q("256Mi"), q("500m"), q("512Mi")}
)

func q(s string) resource.Quantity { return resource.MustParse(s) }

// podSize is a pod's requests and limits as the quota counts them.
type podSize struct{ reqCPU, reqMemory, limCPU, limMemory resource.Quantity }

// add adds n pods of size p to s.
func (s *podSize) add(p podSize, n int) {
	for _, x := range []struct{ to, each *resource.Quantity }{
		{&s.reqCPU, &p.reqCPU}, {&s.reqMemory, &p.reqMemory}, {&s.limCPU, &p.limCPU}, {&s.limMemory, &p.limMemory},
	} {
		each := x.each.DeepCopy()
		each.Mul(int64(n))
		x.to.Add(each)
	}
}

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

// Secret is synced into the namespace by whichever backend the running
// Forge instance configures (1Password by default); Env maps an
// environment variable to a field of the item.
type Secret struct {
	Name string            `yaml:"name" json:"name"`
	Item string            `yaml:"item" json:"item"`
	Env  map[string]string `yaml:"env" json:"env"`
}

// Group is the governed group of the item: the 1Password vault for a
// vaults/<vault>/items/<item> reference, or the first segment of a plain
// <group>/<item> reference for another secret backend.
func (s Secret) Group() string {
	parts := strings.Split(s.Item, "/")
	switch len(parts) {
	case 4:
		if parts[0] != "vaults" || parts[2] != "items" {
			return ""
		}
		return parts[1]
	case 2:
		return parts[0]
	default:
		return ""
	}
}

// ItemName is the item name of s.Item, in either the legacy
// vaults/<vault>/items/<item> shape or the generic <group>/<item> shape.
func (s Secret) ItemName() string {
	parts := strings.Split(s.Item, "/")
	switch len(parts) {
	case 4:
		if parts[0] != "vaults" || parts[2] != "items" {
			return ""
		}
		return parts[3]
	case 2:
		return parts[1]
	default:
		return ""
	}
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
	itemPath   = regexp.MustCompile(`^(vaults/[A-Za-z0-9._-]+/items/[A-Za-z0-9._-]+|[A-Za-z0-9._-]+/[A-Za-z0-9._-]+)$`)
	fieldName  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	cronField  = regexp.MustCompile(`^[0-9A-Za-z*/,?-]+$`)
	vaultName  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	cronMacros = []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"}
	// Set by Forge itself.
	reservedEnv = []string{"PORT", "DATABASE_URL"}
)

// ValidGroup reports whether s is a secret group name run.secrets can use.
func ValidGroup(s string) bool { return vaultName.MatchString(s) }

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
	if a := r.Autoscaling; a != nil {
		if a.Max > MaxReplicas || a.Max <= max(r.Replicas, 1) {
			return fmt.Errorf("run.autoscaling.max %d must be more than run.replicas (%d) and at most %d", a.Max, max(r.Replicas, 1), MaxReplicas)
		}
	}
	if _, err := r.Resources.size(); err != nil {
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
		// The Secret is <name>-secret: no name CloudNativePG or Forge gives
		// a Secret ends that way, so it can never be one of theirs.
		if !ValidID(s.Name) || len(s.Name) > 40 || names[s.Name] {
			return fmt.Errorf("%s.name %q must be a lowercase DNS label of at most 40 characters, used once", where, s.Name)
		}
		names[s.Name] = true
		if !itemPath.MatchString(s.Item) {
			return fmt.Errorf("%s.item %q must be vaults/<vault>/items/<item> (1Password) or <group>/<item> (letters, digits, '.', '_', '-')", where, s.Item)
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
		if !ValidID(j.Name) || names[j.Name] {
			return fmt.Errorf("%s.name %q must be a lowercase DNS label, used once", where, j.Name)
		}
		names[j.Name] = true
		if !validSchedule(j.Schedule) {
			return fmt.Errorf("%s.schedule %q must be five cron fields, such as \"0 3 * * *\", or a macro such as @daily", where, j.Schedule)
		}
		if len(j.Command) == 0 || j.Command[0] == "" {
			return fmt.Errorf("%s.command must be the program and its arguments, such as [node, scripts/cleanup.js]", where)
		}
	}
	return nil
}

// size parses the quantities and returns the app pod's size as Kubernetes
// will make it: a limit not set is the LimitRange's default, and a request
// not set is the limit when that is set, else the LimitRange's default.
func (rs *Resources) size() (podSize, error) {
	p := appDefault
	if rs == nil {
		return p, nil
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
	pick := func(q *Quantities, i int) string {
		if q == nil {
			return ""
		}
		return []string{q.CPU, q.Memory}[i]
	}
	for i, kind := range []string{"cpu", "memory"} {
		req, err := parse("requests."+kind, pick(rs.Requests, i))
		if err != nil {
			return p, err
		}
		lim, err := parse("limits."+kind, pick(rs.Limits, i))
		if err != nil {
			return p, err
		}
		reqTo, limTo := []*resource.Quantity{&p.reqCPU, &p.reqMemory}[i], []*resource.Quantity{&p.limCPU, &p.limMemory}[i]
		if lim != nil {
			*limTo, *reqTo = *lim, *lim
		}
		if req != nil {
			*reqTo = *req
		}
		if reqTo.Cmp(*limTo) > 0 {
			return p, fmt.Errorf("run.resources.requests.%s %s is more than the limit %s (set run.resources.limits.%s)", kind, reqTo.String(), limTo.String(), kind)
		}
	}
	return p, nil
}

// checkRun checks what the run options need of the namespace: the names
// Kubernetes builds from the service's, and the quota, in each environment.
// Every pod that may run at once counts: the replicas (or the autoscaling
// maximum), one more while a rollout replaces them, one per cron job, and
// the database's instances.
func (m *Manifest) checkRun() error {
	r := m.Run
	if r == nil {
		r = &Run{}
	}
	for i, j := range r.Jobs {
		// A CronJob's name has at most 52 characters.
		if n := len(m.Service.Name) + 1 + len(j.Name); n > 52 {
			return fmt.Errorf("run.jobs[%d].name %q: the CronJob %s-%s would be %d characters; at most 52", i, j.Name, m.Service.Name, j.Name, n)
		}
	}
	app, err := r.Resources.size()
	if err != nil {
		return err
	}
	for _, env := range []string{"staging", "production"} {
		replicas := 1
		if env == "production" {
			replicas = max(r.Replicas, 1)
			if r.Autoscaling != nil {
				replicas = r.Autoscaling.Max
			}
		}
		appPods := replicas + 1 + len(r.Jobs)
		dbPods := 0
		for _, res := range m.Resources {
			n := res.Instances
			if o, ok := res.Environments[env]; ok && o.Instances > 0 {
				n = o.Instances
			}
			if n == 0 {
				n = map[string]int{"staging": 1, "production": 2}[env]
			}
			dbPods += n
		}
		var used podSize
		used.add(app, appPods)
		used.add(dbInstance, dbPods)
		what := fmt.Sprintf("in %s, %d app pods (%d replicas, 1 more during a rollout, %d cron jobs) and %d database instances", env, appPods, replicas, len(r.Jobs), dbPods)
		if appPods+dbPods > quotaPods {
			return fmt.Errorf("%s are %d pods; the namespace allows %d", what, appPods+dbPods, quotaPods)
		}
		for _, c := range []struct {
			name        string
			used, quota resource.Quantity
		}{
			{"cpu requests", used.reqCPU, quota.reqCPU}, {"memory requests", used.reqMemory, quota.reqMemory},
			{"cpu limits", used.limCPU, quota.limCPU}, {"memory limits", used.limMemory, quota.limMemory},
		} {
			if c.used.Cmp(c.quota) > 0 {
				return fmt.Errorf("%s need %s of %s; the namespace allows %s (lower run.resources, replicas or jobs)", what, c.used.String(), c.name, c.quota.String())
			}
		}
	}
	return nil
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
