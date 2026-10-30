package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
)

// fromChart creates a service that runs a published Helm chart: no repo, its
// forge.yaml (with chart) in the deploy repo, its entry in the catalog.
// Steps: check_chart, then (unless validate_only) write_manifest and
// register. Deploying it is StartDeploy's.
func (c *creator) fromChart(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	w := &sender{s: stream}
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	release, err := c.claimName(id)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := c.checkFree(ctx, id); err != nil {
		return nil, err
	}

	started("check_chart")
	data, m, err := chartManifest(id, req, c.domains)
	if err != nil {
		return nil, err
	}
	if err := c.deploys.checkChart(ctx, id, m.Chart); err != nil {
		return nil, err
	}
	svc := serviceFromManifest(m)
	if _, err := c.catalog.register(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc, ValidateOnly: true}), false); err != nil {
		return nil, err
	}
	ref := (&deployChart{Repo: m.Chart.Repo, Name: m.Chart.Name, Version: m.Chart.Version}).ref()
	done("check_chart", ref+" is in its repository")

	if req.GetValidateOnly() {
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{Files: []string{manifestPath(id)}}}}
		w.send(final)
		return final, w.err
	}

	work := context.WithoutCancel(ctx)
	quiet := func(string, string) {}
	started("write_manifest")
	if err := c.deploys.writeManifest(work, id, data, fmt.Sprintf("create %s from chart %s (%s)", id, ref, actorOf(ctx)), quiet); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("write %s in the deploy repo: %w", manifestPath(id), err))
	}
	done("write_manifest", "deploy repo: "+manifestPath(id))

	started("register")
	resp, err := c.catalog.register(work, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}), false)
	if err != nil {
		delErr := c.deploys.removeManifestIf(work, id, data, quiet)
		c.log.WarnContext(ctx, "create failed", "service", id, "step", "register", "error", err, "rolled_back", delErr == nil, "rollback_error", delErr)
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Failure{Failure: &forgev1.CreateFailure{
			Step: "register", Message: err.Error(), RolledBack: delErr == nil,
		}}}
		noteCreateFailure(ctx, final)
		w.send(final)
		return final, nil
	}
	done("register", servicePrefix+id)

	pr, _ := PrincipalFrom(ctx)
	c.log.InfoContext(ctx, "service created", "service", id, "chart", ref, "by", pr.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Service{Service: resp.Msg.GetService()}}
	w.send(final)
	return final, nil
}

// chartManifest is the forge.yaml of a new service that runs the request's
// chart, checked as forge.yaml's rules check it.
func chartManifest(id string, req *forgev1.CreateServiceRequest, domains []string) ([]byte, *manifest.Manifest, error) {
	ro := req.GetRunOptions()
	if err := checkDomain(ro, domains); err != nil {
		return nil, nil, err
	}
	cs := req.GetChart()
	chart := &manifest.Chart{
		Repo: cs.GetRepo(), Name: cs.GetName(), Version: cs.GetVersion(),
		Expose: manifest.ChartExpose{Service: cs.GetExposeService(), Port: int(cs.GetExposePort())},
	}
	var err error
	if cs.GetValues() != "" {
		if chart.Values, err = parseChartValues("chart.values", cs.GetValues()); err != nil {
			return nil, nil, err
		}
	}
	for env, text := range map[string]string{"staging": cs.GetStagingValues(), "production": cs.GetProductionValues()} {
		if text == "" {
			continue
		}
		values, err := parseChartValues("chart."+env+"_values", text)
		if err != nil {
			return nil, nil, err
		}
		if chart.Environments == nil {
			chart.Environments = map[string]manifest.ChartEnv{}
		}
		chart.Environments[env] = manifest.ChartEnv{Values: values}
	}
	var run *manifest.Run
	switch {
	case ro.GetNetworkOnly():
		run = &manifest.Run{Domain: manifest.DomainNone}
	case ro.GetDomain() != "":
		run = &manifest.Run{Domain: ro.GetDomain()}
	}
	if ro.GetStagingNetworkOnly() {
		if run == nil {
			run = &manifest.Run{}
		}
		public := false
		run.Staging = &manifest.StagingRun{Public: &public}
	}
	m := &manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Service:    manifest.Service{Name: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners()},
		Run:        run,
		Chart:      chart,
	}
	data, err := m.Marshal()
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	if m, err = manifest.Parse(data); err != nil {
		return nil, nil, invalid("chart: " + err.Error())
	}
	return data, m, nil
}
