// Package policy checks resource plans, promotions and images against
// Forge's Rego rules. The rules are the .rego files next to this file, built
// into the binary: package forge.resources for plans, forge.deploys for
// promotions, forge.images for the image a service without a repo runs.
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
	Actor             Actor    `json:"actor"`
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

// Engine evaluates a compiled rule set.
type Engine struct {
	query       rego.PreparedEvalQuery
	promotion   rego.PreparedEvalQuery
	image       rego.PreparedEvalQuery
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
	pq, err := prepare("deny = data.forge.deploys.deny")
	if err != nil {
		return nil, err
	}
	iq, err := prepare("deny = data.forge.images.deny")
	if err != nil {
		return nil, err
	}
	return &Engine{query: q, promotion: pq, image: iq, fingerprint: hex.EncodeToString(sum.Sum(nil))[:12]}, nil
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
	return results(e.query.Eval(ctx, rego.EvalInput(map[string]any{"plan": plan, "forge": f})))
}

// EvaluatePromotion checks a promotion. An error means it could not be
// checked; it must then not go ahead. Promotions have no warnings.
func (e *Engine) EvaluatePromotion(ctx context.Context, p Promotion) (Result, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Result{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(b, &input); err != nil {
		return Result{}, err
	}
	return results(e.promotion.Eval(ctx, rego.EvalInput(input)))
}

// EvaluateImage checks the image a service without a repo is to run. An
// error means it could not be checked; it must then not run. Images have no
// warnings.
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
	return results(e.image.Eval(ctx, rego.EvalInput(input)))
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
