package manifest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Infrastructure is forge.yaml's infrastructure: the repo holds
// infrastructure code that its own GitHub Actions deploy (phase 12). Forge
// never deploys that code; its target and environments are recorded in the
// deploy repo, not here, so editing the repo cannot move it to another
// account. With run as well, the repo also holds an app that Forge deploys
// like any service's (docs/specs/2026-11-08-app-with-infrastructure-design.md).
type Infrastructure struct {
	// Tool is what the code is written for.
	Tool string `yaml:"tool" json:"tool"`
}

// InfrastructureTools are the tools an infrastructure repo may use.
var InfrastructureTools = []string{"opentofu", "terraform", "bicep"}

// IsInfrastructure reports whether the repo is an infrastructure repo.
func (m *Manifest) IsInfrastructure() bool { return m.Infrastructure != nil }

// Runs reports whether forge.yaml has run: Forge deploys the repo's app.
// For an infrastructure repo, that is the app next to its code.
func (m *Manifest) Runs() bool { return m.Run != nil }

// ErrDeploysItself is the refusal of a Forge deploy or preview of an
// infrastructure repo whose forge.yaml has no run.
var ErrDeploysItself = errors.New("this is an infrastructure repo without run in its forge.yaml: its GitHub Actions deploy it, and Forge approves production; it has no Forge deploys or previews")

func (m *Manifest) checkInfrastructure() error {
	i := m.Infrastructure
	if i == nil {
		return nil
	}
	if !slices.Contains(InfrastructureTools, i.Tool) {
		return fmt.Errorf("infrastructure.tool %q must be one of %s", i.Tool, strings.Join(InfrastructureTools, ", "))
	}
	// run is allowed: the repo's app, deployed by Forge next to the code.
	switch {
	case m.Chart != nil:
		return errors.New("an infrastructure repo has no chart: it deploys itself")
	case len(m.Resources) > 0:
		return errors.New("an infrastructure repo has no resources: its code makes what it needs")
	}
	return nil
}
