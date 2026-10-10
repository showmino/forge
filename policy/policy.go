// Package policy checks resource plans against Forge's Rego rules. The rules
// are the .rego files next to this file, built into the binary.
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
	opts := []func(*rego.Rego){rego.Query("deny = data.forge.resources.deny; warn = data.forge.resources.warn")}
	for _, n := range names {
		src, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", n, len(src))
		sum.Write(src)
		opts = append(opts, rego.Module(n, string(src)))
	}
	q, err := rego.New(opts...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("policy: compile the rules: %w", err)
	}
	return &Engine{query: q, fingerprint: hex.EncodeToString(sum.Sum(nil))[:12]}, nil
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
	rs, err := e.query.Eval(ctx, rego.EvalInput(map[string]any{"plan": plan, "forge": f}))
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
		set, ok := rs[0].Bindings[part.name].([]any)
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
