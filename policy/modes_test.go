package policy

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type modes map[string]string

func (m modes) RuleModes(context.Context) (map[string]string, error) { return m, nil }

type brokenModes struct{}

func (brokenModes) RuleModes(context.Context) (map[string]string, error) {
	return nil, errors.New("the database is away")
}

// The switchable rules in Rules are exactly the ones modes.rego knows, with
// the same defaults, and each is a violation in some rule file.
func TestRulesMatchTheModes(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("modes.rego")
	if err != nil {
		t.Fatal(err)
	}
	inRego := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z]+\.[a-z-]+)": "(enforce|warn|off)"`).FindAllStringSubmatch(string(src), -1) {
		inRego[m[1]] = m[2]
	}
	var all []byte
	for _, f := range []string{"images.rego", "charts.rego", "deploys.rego", "resources.rego", "targets.rego"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	ids := map[string]bool{}
	for _, r := range Rules {
		if ids[r.ID] {
			t.Errorf("%s is listed twice", r.ID)
		}
		ids[r.ID] = true
		if r.Title == "" || r.Description == "" || r.Kind == "" {
			t.Errorf("%s has no title, description or kind", r.ID)
		}
		if !r.Switchable {
			if _, ok := inRego[r.ID]; ok || r.Default != "" {
				t.Errorf("%s is always enforced but has a mode", r.ID)
			}
			continue
		}
		if inRego[r.ID] != r.Default {
			t.Errorf("%s: modes.rego says %q, Rules %q", r.ID, inRego[r.ID], r.Default)
		}
		if !strings.Contains(string(all), `violation contains {"rule": "`+r.ID+`"`) {
			t.Errorf("%s is no rule's violation", r.ID)
		}
		delete(inRego, r.ID)
	}
	if len(inRego) > 0 {
		t.Errorf("modes.rego knows rules Rules does not: %v", inRego)
	}
}

// Each mode of a switchable rule: enforce refuses, warn lets it through with
// the message, off says nothing. A rule that guards Forge cannot be switched.
func TestRuleModes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	latest := Image{Service: "web", Image: "docker.io/library/nginx:latest", Host: "docker.io", Repo: "library/nginx", Tag: "latest"}
	internal := Image{Service: "web", Image: "registry:5000/web:1", Host: "registry:5000", Repo: "web", Tag: "1"}
	for _, tc := range []struct {
		name       string
		modes      modes
		img        Image
		deny, warn bool
	}{
		{"enforced by default", nil, latest, true, false},
		{"warn lets it through", modes{"images.pinned-version": "warn"}, latest, false, true},
		{"off says nothing", modes{"images.pinned-version": "off"}, latest, false, false},
		{"an unknown mode is the default", modes{"images.pinned-version": "maybe"}, latest, true, false},
		{"a rule that guards Forge cannot be switched", modes{"images.no-internal-hosts": "off", "images.allowed-registries": "off"}, internal, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := *e
			if tc.modes != nil {
				eng.Modes = tc.modes
			}
			res, err := eng.EvaluateImage(ctx, tc.img)
			if err != nil {
				t.Fatal(err)
			}
			if (len(res.Deny) > 0) != tc.deny || (len(res.Warn) > 0) != tc.warn {
				t.Errorf("deny %q warn %q", res.Deny, res.Warn)
			}
		})
	}

	// A promotion of an unhealthy staging, warned: it goes ahead. The deploy
	// that was checked is still required.
	p := Promotion{Service: "payments", From: "staging", To: "production", Source: Source{Sync: "Synced", Health: "Degraded", AtRevision: false}}
	eng := *e
	eng.Modes = modes{"promotion.healthy-staging": "warn", "promotion.checked-deploy": "off"}
	res, err := eng.EvaluatePromotion(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Warn, []string{"staging is Degraded; it must be Healthy to be promoted"}) || len(res.Deny) != 1 || !strings.Contains(res.Deny[0], "latest deploy") {
		t.Errorf("promotion: deny %q warn %q", res.Deny, res.Warn)
	}

	// Staging out of sync and behind its latest deploy: whatever the mode of
	// healthy-staging, the deploy that was checked is still required.
	behind := Promotion{Service: "payments", From: "staging", To: "production", Source: Source{Sync: "OutOfSync", Health: "Healthy", AtRevision: false, DeployCommit: "9f8e7d6"}}
	for _, m := range []string{"enforce", "warn", "off"} {
		eng := *e
		eng.Modes = modes{"promotion.healthy-staging": m}
		res, err := eng.EvaluatePromotion(ctx, behind)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(res.Deny, func(d string) bool { return strings.Contains(d, "does not run its latest deploy repo commit") }) {
			t.Errorf("healthy-staging %s: deny %q warn %q, want the checked deploy refused", m, res.Deny, res.Warn)
		}
	}

	// The staging warning can be made a refusal.
	eng = *e
	eng.Modes = modes{"resources.staging-single": "enforce"}
	stg := Forge{Service: "payments", Environment: "staging"}
	two := plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 2, "longhorn-retain"))
	res, err = eng.Evaluate(ctx, two, declaring(stg, two))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Deny, []string{"staging has 2 instances; 1 is usually enough"}) || len(res.Warn) != 0 {
		t.Errorf("staging-single enforced: deny %q warn %q", res.Deny, res.Warn)
	}
}

// When the modes cannot be read, every switchable rule is enforced, even one
// whose default is warn.
func TestRuleModesFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var told error
	e.Modes = brokenModes{}
	e.OnModesError = func(err error) { told = err }
	res, err := e.EvaluateImage(ctx, Image{Service: "web", Image: "docker.io/library/nginx:latest", Host: "docker.io", Repo: "library/nginx", Tag: "latest"})
	if err != nil || len(res.Deny) != 1 || told == nil {
		t.Errorf("deny %q, err %v, told %v", res.Deny, err, told)
	}
	two := plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 2, "longhorn-retain"))
	res, err = e.Evaluate(ctx, two, declaring(Forge{Service: "payments", Environment: "staging"}, two))
	if err != nil || len(res.Deny) != 1 || len(res.Warn) != 0 || !strings.Contains(res.Modes, "resources.staging-single=enforce") {
		t.Errorf("staging-single when the modes are away: deny %q warn %q modes %q", res.Deny, res.Warn, res.Modes)
	}
}

// A result says which modes, other than the defaults, decided it.
func TestRuleModesInTheResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.Modes = modes{"images.pinned-version": "warn", "charts.allowed-repos": "enforce", "images.no-internal-hosts": "off"}
	res, err := e.EvaluateImage(ctx, Image{Service: "web", Image: "docker.io/library/nginx:1.27", Host: "docker.io", Repo: "library/nginx", Tag: "1.27"})
	if err != nil || res.Modes != "images.pinned-version=warn" {
		t.Errorf("modes %q, %v", res.Modes, err)
	}
}
