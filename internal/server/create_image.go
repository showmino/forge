package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
)

// fromImage creates a service that runs an image someone else built: no
// repo, its forge.yaml (with run.image) in the deploy repo, its entry in the
// catalog. Steps: check_image, then (unless validate_only) write_manifest
// and register. Deploying it is StartDeploy's.
func (c *creator) fromImage(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	w := &sender{s: stream}
	started := func(step string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_STARTED, "")) }
	done := func(step, detail string) { w.send(stepMsg(step, forgev1.CreateStep_STATE_DONE, detail)) }
	id := req.GetServiceId()
	if err := c.checkFree(ctx, id); err != nil {
		return nil, err
	}

	started("check_image")
	p, err := c.deploys.pinImage(ctx, id, req.GetImage())
	if err != nil {
		return nil, err
	}
	data, m, err := imageManifest(id, req, p.full, c.domains)
	if err != nil {
		return nil, err
	}
	svc := serviceFromManifest(m)
	if _, err := c.catalog.register(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc, ValidateOnly: true}), false); err != nil {
		return nil, err
	}
	done("check_image", p.full+" is "+shortDigest(p.digest))

	if req.GetValidateOnly() {
		final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{Files: []string{manifestPath(id)}}}}
		w.send(final)
		return final, w.err
	}

	// From here on the work outlives the client, as a template's create does.
	work := context.WithoutCancel(ctx)
	quiet := func(string, string) {}
	started("write_manifest")
	if err := c.deploys.writeManifest(work, id, data, fmt.Sprintf("create %s from %s (%s)", id, p.full, actorOf(ctx)), quiet); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("write %s in the deploy repo: %w", manifestPath(id), err))
	}
	done("write_manifest", "deploy repo: "+manifestPath(id))

	started("register")
	resp, err := c.catalog.register(work, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: id, Service: svc}), false)
	if err != nil {
		delErr := c.deploys.removeManifest(work, id, quiet)
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
	c.log.InfoContext(ctx, "service created", "service", id, "image", p.full, "by", pr.Subject)
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Service{Service: resp.Msg.GetService()}}
	w.send(final)
	return final, nil
}

// imageManifest is the forge.yaml of a new service that runs image (in
// full), with the request's run options, checked as forge.yaml's rules
// check it.
func imageManifest(id string, req *forgev1.CreateServiceRequest, image string, domains []string) ([]byte, *manifest.Manifest, error) {
	ro := req.GetRunOptions()
	if err := checkDomain(ro, domains); err != nil {
		return nil, nil, err
	}
	run := runFromProto(ro)
	run.Image, run.Port, run.Health = image, int(ro.GetPort()), ro.GetHealth()
	switch {
	case ro.GetNetworkOnly():
		run.Domain = manifest.DomainNone
	case ro.GetDomain() != "":
		run.Domain = ro.GetDomain()
	}
	if ro.GetStagingNetworkOnly() {
		public := false
		run.Staging = &manifest.StagingRun{Public: &public}
	}
	m := &manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Service:    manifest.Service{Name: id, Team: req.GetOwnerTeam(), Owners: req.GetOwners()},
		Run:        run,
	}
	data, err := m.Marshal()
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	if m, err = manifest.Parse(data); err != nil {
		return nil, nil, invalid("run options: " + err.Error())
	}
	return data, m, nil
}

// checkDomain checks the public domain a create asks for: one of the
// platform's, and not with network_only.
func checkDomain(ro *forgev1.RunOptions, domains []string) error {
	d := ro.GetDomain()
	switch {
	case d == "":
		return nil
	case ro.GetNetworkOnly():
		return invalid(fmt.Sprintf("domain %s is a public address and network_only is none: ask for one, not both", d))
	case len(domains) == 0:
		return invalid(fmt.Sprintf("domain %s: this Forge API offers no public domains", d))
	case !slices.Contains(domains, d):
		return invalid(fmt.Sprintf("domain %s is not a domain of this platform: use %s", d, strings.Join(domains, " or ")))
	}
	return nil
}

// imageCreateReady says why this Forge API cannot create services from an
// image, or nil when it can.
func (c *creator) imageCreateReady() error {
	if c.deploys == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("deploys are not configured on this Forge API, so it cannot run an image"))
	}
	return c.deploys.ready()
}
