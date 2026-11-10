// Package policy checks resource plans, promotions and images against
// Forge's Rego rules. The rules are the .rego files next to this file, built
// into the binary: package forge.resources for plans, forge.deploys for
// promotions, forge.images for the image a service without a repo runs,
// forge.charts for the chart one runs, forge.targets for the target an
// infrastructure repo deploys to.
package policy

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed *.rego
var embedded embed.FS

// Forge is what a rule knows besides the plan.
type Forge struct {
	Service      string     `json:"service"`
	Environment  string     `json:"environment"`
	Resources    []Resource `json:"resources"`
	Actor        Actor      `json:"actor"`
	AllowDestroy bool       `json:"allow_destroy"`
}

// Resource is one declared resource, worked out for the environment.
type Resource struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	StorageGi int32  `json:"storage_gi"`
	Instances int32  `json:"instances"`
}

// Actor is who asked for the plan.
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

// Promotion is what the promotion rules know.
type Promotion struct {
	Service string `json:"service"`
	From    string `json:"from"`
	To      string `json:"to"`
	Source  Source `json:"source"`
	Actor   Actor  `json:"actor"`
}

// Image is what the image rules know: the image a service without a repo
// runs, written in full and in parts.
type Image struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Host    string `json:"host"`
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Digest  string `json:"digest"`
	// AllowedRegistries are the hosts images may come from; empty allows any.
	AllowedRegistries []string `json:"allowed_registries"`
	// ServiceImages is where the platform's services' CI pushes their images,
	// "<registry>/<org>/": those are not for a service without a repo.
	ServiceImages string `json:"service_images"`
	Actor         Actor  `json:"actor"`
}

// Chart is what the chart rules know: the Helm chart a service without a
// repo runs.
type Chart struct {
	Service string `json:"service"`
	// Repo is the chart repository as forge.yaml has it, Host its host.
	Repo    string `json:"repo"`
	Host    string `json:"host"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// AllowedRepos are the repositories charts may come from; empty allows any.
	AllowedRepos []string `json:"allowed_repos"`
	Actor        Actor    `json:"actor"`
}

// Target is what the target rules know: the target an infrastructure repo
// is to deploy to (phase 12).
type Target struct {
	Service string `json:"service"`
	Team    string `json:"team"`
	Target  string `json:"target"`
	// Teams may use the target; empty: every team.
	Teams []string `json:"teams"`
	// Environments are the ones asked for, Offered the ones the target has,
	// Full those of them with no room left.
	Environments []string `json:"environments"`
	Offered      []string `json:"offered"`
	Full         []string `json:"full"`
	Actor        Actor    `json:"actor"`
}

// Source is what runs in the environment a promotion takes from, as Argo CD
// reports it.
type Source struct {
	Sync   string `json:"sync"`
	Health string `json:"health"`
	// AtRevision: Argo CD has synced the environment's latest deploy, not an
	// earlier one.
	AtRevision   bool   `json:"at_revision"`
	Commit       string `json:"commit"`
	DeployCommit string `json:"deploy_commit"`
	Version      string `json:"version"`
	Image        string `json:"image"`
}

// Result is what the rules said about one plan. Both lists are sorted.
type Result struct {
	Deny []string
	Warn []string
}

// Denied reports whether any rule refused the plan.
func (r Result) Denied() bool { return len(r.Deny) > 0 }

// Modes, from most to least strict.
const (
	ModeEnforce = "enforce"
	ModeWarn    = "warn"
	ModeOff     = "off"
)

// Rule is one rule as people see it. A switchable rule has a mode an admin
// may change; the others guard Forge itself and are always enforced.
type Rule struct {
	ID          string
	Kind        string
	Title       string
	Description string
	Switchable  bool
	// Default is a switchable rule's mode when nobody changed it.
	Default string
}

// Rules are the rules, switchable first, by kind. modes.rego's defaults name
// the same switchable rules (TestRulesMatchTheModes).
var Rules = []Rule{
	{ID: "images.pinned-version", Kind: "Images", Title: "Images name a version", Description: "An image names a tag other than latest, or a digest: one that moves runs something else on every restart.", Switchable: true, Default: ModeEnforce},
	{ID: "images.allowed-registries", Kind: "Images", Title: "Images come from the allowed registries", Description: "A service without a repo runs images only from the registries the platform allows (--allowed-registries).", Switchable: true, Default: ModeEnforce},
	{ID: "charts.allowed-repos", Kind: "Charts", Title: "Charts come from the allowed repositories", Description: "A service runs Helm charts only from the repositories the platform allows (--allowed-chart-repos).", Switchable: true, Default: ModeEnforce},
	{ID: "promotion.healthy-staging", Kind: "Promotion", Title: "Promote only a synced, healthy staging", Description: "Argo CD reports staging Synced and Healthy before it is promoted to production.", Switchable: true, Default: ModeEnforce},
	{ID: "resources.storage-limit", Kind: "Resources", Title: "Storage within the environment's limit", Description: "A database's storage is at most the limit of its environment.", Switchable: true, Default: ModeEnforce},
	{ID: "resources.production-ha", Kind: "Resources", Title: "Production databases run two instances", Description: "A database in production runs at least 2 instances, so one can fail.", Switchable: true, Default: ModeEnforce},
	{ID: "resources.instance-limit", Kind: "Resources", Title: "Instances within the limit", Description: "A database runs at most the platform's limit of instances.", Switchable: true, Default: ModeEnforce},
	{ID: "resources.staging-single", Kind: "Resources", Title: "Staging needs one instance", Description: "A database in staging usually needs one instance; more cost without helping.", Switchable: true, Default: ModeWarn},
	{ID: "resources.declared-only", Kind: "Resources", Title: "A plan changes only the declared database", Description: "A plan changes only CloudNativePG clusters in the service's own namespace, exactly as forge.yaml declares them, with every change readable."},
	{ID: "resources.no-silent-delete", Kind: "Resources", Title: "Deletes are asked for", Description: "A plan deletes a database only with --allow-destroy, and storage never shrinks."},
	{ID: "resources.storage-class", Kind: "Resources", Title: "Retained storage", Description: "A database's volumes use the longhorn-retain storage class, and it runs at least one instance."},
	{ID: "images.no-service-ci-images", Kind: "Images", Title: "No other service's CI image", Description: "An image built by a service's CI is deployed from that service's repo, never run as another service."},
	{ID: "images.no-internal-hosts", Kind: "Images", Title: "No internal registries or chart hosts", Description: "A registry or chart host that is an address or a one-word name is inside the platform's network: only one the platform allows by name."},
	{ID: "promotion.checked-deploy", Kind: "Promotion", Title: "Promote the deploy that was checked", Description: "Staging runs its latest deploy before it is promoted, so production gets the image that was checked."},
	{ID: "targets.team-and-room", Kind: "Targets", Title: "Targets belong to the team", Description: "An infrastructure repo deploys only to a target of its team, to environments the target has and has room in."},
}

// ValidMode reports whether m is a mode.
func ValidMode(m string) bool { return m == ModeEnforce || m == ModeWarn || m == ModeOff }

// RuleByID returns the rule with id.
func RuleByID(id string) (Rule, bool) {
	for _, r := range Rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// ModeSource holds the modes admins set, by rule id; a rule not in it has
// its default. *Engine reads it before every evaluation.
type ModeSource interface {
	RuleModes(ctx context.Context) (map[string]string, error)
}

// Engine evaluates a compiled rule set.
type Engine struct {
	// Modes, when set, are the modes of the switchable rules. A mode that
	// cannot be read is its default, the stricter choice.
	Modes ModeSource
	// OnModesError is told when Modes fails; nil ignores it.
	OnModesError func(error)

	query       rego.PreparedEvalQuery
	promotion   rego.PreparedEvalQuery
	image       rego.PreparedEvalQuery
	chart       rego.PreparedEvalQuery
	target      rego.PreparedEvalQuery
	fingerprint string
}

// New compiles the rules built into the binary.
func New(ctx context.Context) (*Engine, error) { return NewFromFS(ctx, embedded) }

// NewFromFS compiles every .rego file at the root of fsys.
func NewFromFS(ctx context.Context, fsys fs.FS) (*Engine, error) {
	names, err := fs.Glob(fsys, "*.rego")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("policy: no .rego files")
	}
	slices.Sort(names)
	sum := sha256.New()
	var modules []func(*rego.Rego)
	for _, n := range names {
		src, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", n, len(src))
		sum.Write(src)
		modules = append(modules, rego.Module(n, string(src)))
	}
	prepare := func(query string) (rego.PreparedEvalQuery, error) {
		q, err := rego.New(append([]func(*rego.Rego){rego.Query(query)}, modules...)...).PrepareForEval(ctx)
		if err != nil {
			return q, fmt.Errorf("policy: compile the rules: %w", err)
		}
		return q, nil
	}
	q, err := prepare("deny = data.forge.resources.deny; warn = data.forge.resources.warn")
	if err != nil {
		return nil, err
	}
	pq, err := prepare("deny = data.forge.deploys.deny; warn = data.forge.deploys.warn")
	if err != nil {
		return nil, err
	}
	iq, err := prepare("deny = data.forge.images.deny; warn = data.forge.images.warn")
	if err != nil {
		return nil, err
	}
	cq, err := prepare("deny = data.forge.charts.deny; warn = data.forge.charts.warn")
	if err != nil {
		return nil, err
	}
	tq, err := prepare("deny = data.forge.targets.deny")
	if err != nil {
		return nil, err
	}
	return &Engine{query: q, promotion: pq, image: iq, chart: cq, target: tq, fingerprint: hex.EncodeToString(sum.Sum(nil))[:12]}, nil
}

// Fingerprint identifies the rule set: the first 12 hex characters of the
// SHA-256 of its files.
func (e *Engine) Fingerprint() string { return e.fingerprint }

// Evaluate checks planJSON, the output of `tofu show -json`, with f. An
// error means the plan could not be checked; it must then not be applied.
func (e *Engine) Evaluate(ctx context.Context, planJSON []byte, f Forge) (Result, error) {
	var plan map[string]any
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return Result{}, fmt.Errorf("policy: the plan is not a JSON object: %w", err)
	}
	if plan == nil {
		return Result{}, errors.New("policy: the plan is not a JSON object")
	}
	if f.Resources == nil {
		f.Resources = []Resource{}
	}
	return results(e.query.Eval(ctx, rego.EvalInput(map[string]any{"plan": plan, "forge": f, "modes": e.modes(ctx)})))
}

// EvaluatePromotion checks a promotion. An error means it could not be
// checked; it must then not go ahead.
func (e *Engine) EvaluatePromotion(ctx context.Context, p Promotion) (Result, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Result{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(b, &input); err != nil {
		return Result{}, err
	}
	input["modes"] = e.modes(ctx)
	return results(e.promotion.Eval(ctx, rego.EvalInput(input)))
}

// EvaluateImage checks the image a service without a repo is to run. An
// error means it could not be checked; it must then not run.
func (e *Engine) EvaluateImage(ctx context.Context, i Image) (Result, error) {
	if i.AllowedRegistries == nil {
		i.AllowedRegistries = []string{}
	}
	b, err := json.Marshal(i)
	if err != nil {
		return Result{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(b, &input); err != nil {
		return Result{}, err
	}
	input["modes"] = e.modes(ctx)
	return results(e.image.Eval(ctx, rego.EvalInput(input)))
}

// EvaluateChart checks the chart a service without a repo is to run. An
// error means it could not be checked; it must then not run.
func (e *Engine) EvaluateChart(ctx context.Context, c Chart) (Result, error) {
	if c.AllowedRepos == nil {
		c.AllowedRepos = []string{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return Result{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(b, &input); err != nil {
		return Result{}, err
	}
	input["modes"] = e.modes(ctx)
	return results(e.chart.Eval(ctx, rego.EvalInput(input)))
}

// EvaluateTarget checks the target an infrastructure repo is to deploy to.
// An error means it could not be checked; the repo must then not be made.
func (e *Engine) EvaluateTarget(ctx context.Context, t Target) (Result, error) {
	for _, l := range []*[]string{&t.Teams, &t.Environments, &t.Offered, &t.Full} {
		if *l == nil {
			*l = []string{}
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return Result{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(b, &input); err != nil {
		return Result{}, err
	}
	input["modes"] = e.modes(ctx)
	return results(e.target.Eval(ctx, rego.EvalInput(input)))
}

// modes are the modes admins set, for the rules' input.modes. Without a
// source, or when it fails, every rule has its default.
func (e *Engine) modes(ctx context.Context) map[string]any {
	out := map[string]any{}
	if e.Modes == nil {
		return out
	}
	set, err := e.Modes.RuleModes(ctx)
	if err != nil {
		if e.OnModesError != nil {
			e.OnModesError(err)
		}
		return out
	}
	for id, m := range set {
		if r, ok := RuleByID(id); ok && r.Switchable && ValidMode(m) {
			out[id] = m
		}
	}
	return out
}

// results reads the deny and, when bound, warn sets of one evaluation.
func results(rs rego.ResultSet, err error) (Result, error) {
	if err != nil {
		return Result{}, fmt.Errorf("policy: evaluate: %w", err)
	}
	if len(rs) != 1 {
		return Result{}, fmt.Errorf("policy: the rules gave %d results, want 1", len(rs))
	}
	var out Result
	for _, part := range []struct {
		name string
		dst  *[]string
	}{{"deny", &out.Deny}, {"warn", &out.Warn}} {
		bound, present := rs[0].Bindings[part.name]
		if !present && part.name == "warn" {
			continue
		}
		set, ok := bound.([]any)
		if !ok {
			return Result{}, fmt.Errorf("policy: %s is not a set of messages", part.name)
		}
		for _, m := range set {
			s, ok := m.(string)
			if !ok {
				return Result{}, fmt.Errorf("policy: a %s message is not a string", part.name)
			}
			*part.dst = append(*part.dst, s)
		}
		slices.Sort(*part.dst)
	}
	return out, nil
}
