package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
)

const (
	kindApplicationPromote  = "application.promote"
	kindApplicationRollback = "application.rollback"
	// releasesPageSize is how many releases ListApplicationReleases returns
	// when it is not asked for a number, and maxReleases the most.
	releasesPageSize = 20
	maxReleases      = 200
)

// applicationFor is the application named by name ("applications/<id>"),
// for a write: deploys must be on, and the caller a member of its team.
func (s *applicationService) applicationFor(ctx context.Context, name, action string) (*forgev1.Application, string, error) {
	if s.deploys == nil {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge has no deploys"))
	}
	if err := s.deploys.ready(); err != nil {
		return nil, "", err
	}
	id, ok := strings.CutPrefix(name, applicationPrefix)
	if !ok || !manifest.ValidID(id) {
		return nil, "", invalid(fmt.Sprintf("name %q must be an application name like applications/shop", name))
	}
	apps, err := s.catalog.applications(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if len(apps) == 0 {
		// Who may act on an application is its team's to say, and there is
		// no team to ask: only an admin learns that it does not exist.
		if err := authorizeTeam(ctx, action, ""); err != nil {
			return nil, "", err
		}
		return nil, "", notFound(name)
	}
	if err := authorizeTeam(ctx, action, apps[0].GetOwnerTeam()); err != nil {
		return nil, "", err
	}
	return apps[0], id, nil
}

// partErr is err, which refused a part of an application, saying which.
func partErr(service string, err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return connect.NewError(ce.Code(), fmt.Errorf("%s: %s", service, ce.Message()))
	}
	return fmt.Errorf("%s: %w", service, err)
}

// shownVersionOf is what people call what v runs: its version, or its ref.
func shownVersionOf(v *deployValues) string {
	switch {
	case v == nil:
		return ""
	case v.Forge.Version != "":
		return v.Forge.Version
	default:
		return v.Forge.Ref
	}
}

// linkOrder sorts plans so that a part comes after the parts it links to;
// parts with no order between them (and parts that link in a circle) go by
// name.
func linkOrder(plans []mutation) []mutation {
	slices.SortFunc(plans, func(a, b mutation) int { return strings.Compare(a.svc, b.svc) })
	bySvc := map[string]mutation{}
	for _, m := range plans {
		bySvc[m.svc] = m
	}
	var out []mutation
	state := map[string]int{} // 1 being visited, 2 done
	var visit func(m mutation)
	visit = func(m mutation) {
		if state[m.svc] != 0 {
			return
		}
		state[m.svc] = 1
		for _, l := range m.v.Forge.Links {
			p, _ := manifest.SplitLink(l)
			if dep, ok := bySvc[p]; ok {
				visit(dep)
			}
		}
		state[m.svc] = 2
		out = append(out, m)
	}
	for _, m := range plans {
		visit(m)
	}
	return out
}

// claimAll locks every plan's service and environment for one operation,
// or none of them. The caller records the operation with hold and frees
// them with d.unlock.
func (d *Deploys) claimAll(ctx context.Context, plans []mutation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, m := range plans {
		if err := d.claimIn(ctx, m.svc, m.env); err != nil {
			for _, done := range plans[:i] {
				delete(d.running, done.svc+"/"+done.env)
			}
			return err
		}
	}
	return nil
}

// hold names the operation that has the plans' locks.
func (d *Deploys) hold(plans []mutation, op string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range plans {
		d.running[m.svc+"/"+m.env] = op
	}
}

// forPart is emit for one part of an application's operation.
func forPart(emit func(string, string), svc string) func(string, string) {
	return func(step, message string) { emit(step, svc+": "+message) }
}

// run applies one planned change of a part inside an application's operation.
func (d *Deploys) run(ctx context.Context, m mutation, emit func(string, string)) error {
	emit = forPart(emit, m.svc)
	if m.moved != "" {
		emit("address", m.moved)
	}
	if m.cut != nil {
		version, err := m.cut(ctx, emit)
		if err != nil {
			return err
		}
		m.v.Forge.Version = version
	}
	return d.apply(ctx, m.svc, m.env, m.v, emit)
}

func (s *applicationService) PromoteApplication(ctx context.Context, req *connect.Request[forgev1.PromoteApplicationRequest]) (*connect.Response[forgev1.PromoteApplicationResponse], error) {
	app, id, err := s.applicationFor(ctx, req.Msg.GetName(), ActDeployPromote)
	if err != nil {
		return nil, err
	}
	d := s.deploys
	const from, to = "staging", finalEnv
	var plans []mutation
	// What each part ran in production before, by its deploy commit: ""
	// for a part that was not there.
	before := map[string]string{}
	var planned []*forgev1.PlannedPart
	for _, p := range app.GetParts() {
		svc := strings.TrimPrefix(p.GetService(), servicePrefix)
		entry, err := s.catalog.get(ctx, p.GetService())
		if err != nil {
			return nil, err
		}
		if i := entry.GetInfrastructure(); (i != nil && !i.GetRuns()) || len(serviceEnvs(entry)) != len(deployEnvs) {
			continue // nothing of it goes from staging to production
		}
		staged, _, err := d.current(ctx, svc, from)
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue // not in staging yet
		}
		if err != nil {
			return nil, err
		}
		prod, prodCommit, err := d.current(ctx, svc, to)
		switch {
		case connect.CodeOf(err) == connect.CodeNotFound:
		case err != nil:
			return nil, err
		case prod.Forge.Commit == staged.Forge.Commit && prod.Image == staged.Image:
			continue // production runs it already
		default:
			before[svc] = prodCommit
		}
		m, err := d.planPromote(ctx, svc, from, to, req.Msg.GetBump())
		if err != nil {
			return nil, partErr(p.GetService(), err)
		}
		plans = append(plans, m)
	}
	if len(plans) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has nothing to promote: production runs what staging runs", app.GetName()))
	}
	plans = linkOrder(plans)
	for _, m := range plans {
		pp := &forgev1.PlannedPart{Service: servicePrefix + m.svc, ToVersion: shownVersionOf(&m.v)}
		if before[m.svc] != "" {
			pp.FromVersion = shownVersionOf(d.currentValues(ctx, m.svc, to))
		}
		planned = append(planned, pp)
	}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.PromoteApplicationResponse{Parts: planned}), nil
	}
	if err := d.claimAll(ctx, plans); err != nil {
		return nil, err
	}
	actor := actorOf(ctx)
	op := d.o.Operations.Start(ctx, kindApplicationPromote, app.GetName(), actor, func(ctx context.Context, emit func(string, string)) error {
		defer func() {
			for _, m := range plans {
				d.unlock(m.svc + "/" + m.env)
			}
		}()
		for i, m := range plans {
			err := d.run(ctx, m, emit)
			if err == nil {
				continue
			}
			// Production is not left half promoted: the part that failed
			// and the parts before it go back to what they ran.
			emit("failed", fmt.Sprintf("%s: %v", m.svc, err))
			back := s.putBack(ctx, plans[:i+1], before, emit)
			return &OpError{Code: "promote_failed", Message: fmt.Sprintf("%s%s could not be promoted (%v); %s", servicePrefix, m.svc, err, back)}
		}
		n, err := s.record(ctx, id, actor, "")
		if err != nil {
			return &OpError{Code: "release_not_recorded", Message: fmt.Sprintf("every part is promoted, but the release could not be recorded: %v", err)}
		}
		emit("release", fmt.Sprintf("%s release %d", app.GetName(), n))
		return nil
	})
	d.hold(plans, op.GetName())
	return connect.NewResponse(&forgev1.PromoteApplicationResponse{Operation: op, Parts: planned}), nil
}

// putBack returns the parts in tried, newest first, to the deploy commits
// they ran before a promotion of the application failed, and says what it
// did. It goes on after a cancel: a half promoted production is what the
// promotion must not leave.
func (s *applicationService) putBack(ctx context.Context, tried []mutation, before map[string]string, emit func(string, string)) string {
	ctx = context.WithoutCancel(ctx)
	d := s.deploys
	var back, stuck, fresh []string
	for i := len(tried) - 1; i >= 0; i-- {
		m := tried[i]
		commit := before[m.svc]
		if commit == "" {
			fresh = append(fresh, m.svc)
			continue
		}
		if _, now, err := d.current(ctx, m.svc, m.env); err == nil && now == commit {
			continue // it failed before it changed anything
		}
		rb, err := d.planRollback(ctx, m.svc, m.env, commit)
		if err == nil {
			err = d.run(ctx, rb, emit)
		}
		if err != nil {
			emit("failed", fmt.Sprintf("%s: could not be put back: %v", m.svc, err))
			stuck = append(stuck, m.svc)
			continue
		}
		back = append(back, m.svc)
	}
	var said []string
	if len(back) > 0 {
		said = append(said, "put back to what they ran: "+strings.Join(back, ", "))
	}
	if len(fresh) > 0 {
		said = append(said, "new in production, with nothing to go back to, and left as they are: "+strings.Join(fresh, ", "))
	}
	if len(stuck) > 0 {
		said = append(said, "NOT put back, roll them back by hand with 'forge deploy rollback': "+strings.Join(stuck, ", "))
	}
	if len(said) == 0 {
		return "nothing had changed"
	}
	return strings.Join(said, "; ")
}

// record writes what every part of the application runs in production now
// as its next release.
func (s *applicationService) record(ctx context.Context, id, actor, note string) (int, error) {
	if s.releases == nil {
		return 0, errors.New("this Forge keeps no releases")
	}
	ctx = context.WithoutCancel(ctx)
	apps, err := s.catalog.applications(ctx, id)
	if err != nil {
		return 0, err
	}
	r := store.Release{Application: id, CreatedAt: s.now().UTC(), CreatedBy: actor, Note: note}
	for _, app := range apps {
		for _, p := range app.GetParts() {
			svc := strings.TrimPrefix(p.GetService(), servicePrefix)
			v, commit, err := s.deploys.current(ctx, svc, finalEnv)
			if connect.CodeOf(err) == connect.CodeNotFound {
				continue
			}
			if err != nil {
				return 0, err
			}
			r.Parts = append(r.Parts, store.ReleasePart{Service: p.GetService(), Version: shownVersionOf(&v), Commit: v.Forge.Commit, DeployCommit: commit})
		}
	}
	return s.releases.AddRelease(ctx, r)
}

func releaseProto(r store.Release) *forgev1.ApplicationRelease {
	out := &forgev1.ApplicationRelease{
		Name:   applicationPrefix + r.Application + "/releases/" + strconv.Itoa(r.Number),
		Number: int32(r.Number), CreateTime: timestamppb.New(r.CreatedAt), Creator: r.CreatedBy, Note: r.Note, //nolint:gosec // a count of releases
	}
	for _, p := range r.Parts {
		out.Parts = append(out.Parts, &forgev1.ReleasePart{Service: p.Service, Version: p.Version, Commit: p.Commit, DeployCommit: p.DeployCommit})
	}
	return out
}

func (s *applicationService) ListApplicationReleases(ctx context.Context, req *connect.Request[forgev1.ListApplicationReleasesRequest]) (*connect.Response[forgev1.ListApplicationReleasesResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	id, ok := strings.CutPrefix(req.Msg.GetName(), applicationPrefix)
	if !ok || !manifest.ValidID(id) {
		return nil, invalid(fmt.Sprintf("name %q must be an application name like applications/shop", req.Msg.GetName()))
	}
	size := int(req.Msg.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = releasesPageSize
	case size > maxReleases:
		size = maxReleases
	}
	resp := &forgev1.ListApplicationReleasesResponse{}
	if s.releases == nil {
		return connect.NewResponse(resp), nil
	}
	rs, err := s.releases.Releases(ctx, id, size)
	if err != nil {
		return nil, storeErr(err)
	}
	for _, r := range rs {
		resp.Releases = append(resp.Releases, releaseProto(r))
	}
	return connect.NewResponse(resp), nil
}

func (s *applicationService) RollbackApplication(ctx context.Context, req *connect.Request[forgev1.RollbackApplicationRequest]) (*connect.Response[forgev1.RollbackApplicationResponse], error) {
	app, id, err := s.applicationFor(ctx, req.Msg.GetName(), ActDeployRollback)
	if err != nil {
		return nil, err
	}
	if s.releases == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge keeps no releases"))
	}
	d := s.deploys
	rs, err := s.releases.Releases(ctx, id, maxReleases)
	if err != nil {
		return nil, storeErr(err)
	}
	var target *store.Release
	switch n := int(req.Msg.GetRelease()); {
	case n < 0:
		return nil, invalid("release must not be negative")
	case n == 0 && len(rs) < 2:
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no earlier release to roll back to", app.GetName()))
	case n == 0:
		target = &rs[1]
	default:
		for i := range rs {
			if rs[i].Number == n {
				target = &rs[i]
			}
		}
		if target == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no release %d in its last %d", app.GetName(), n, maxReleases))
		}
	}
	parts := map[string]bool{}
	for _, p := range app.GetParts() {
		parts[p.GetService()] = true
	}
	var plans []mutation
	var planned []*forgev1.PlannedPart
	for _, rp := range target.Parts {
		if !parts[rp.Service] {
			continue // it left the application since
		}
		svc := strings.TrimPrefix(rp.Service, servicePrefix)
		cur, _, err := d.current(ctx, svc, finalEnv)
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue // taken down since; a rollback does not bring it back
		}
		if err != nil {
			return nil, err
		}
		if cur.Forge.Commit == rp.Commit && shownVersionOf(&cur) == rp.Version {
			continue // it runs that already
		}
		m, err := d.planRollback(ctx, svc, finalEnv, rp.DeployCommit)
		if err != nil {
			return nil, partErr(rp.Service, err)
		}
		plans = append(plans, m)
		planned = append(planned, &forgev1.PlannedPart{Service: rp.Service, FromVersion: shownVersionOf(&cur), ToVersion: rp.Version})
	}
	resp := &forgev1.RollbackApplicationResponse{Parts: planned, Release: releaseProto(*target)}
	if len(plans) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs release %d already", app.GetName(), target.Number))
	}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(resp), nil
	}
	if err := d.claimAll(ctx, plans); err != nil {
		return nil, err
	}
	actor, number := actorOf(ctx), target.Number
	op := d.o.Operations.Start(ctx, kindApplicationRollback, app.GetName(), actor, func(ctx context.Context, emit func(string, string)) error {
		defer func() {
			for _, m := range plans {
				d.unlock(m.svc + "/" + m.env)
			}
		}()
		var done []string
		for _, m := range plans {
			if err := d.run(ctx, m, emit); err != nil {
				before := "none"
				if len(done) > 0 {
					before = strings.Join(done, ", ")
				}
				return &OpError{Code: "rollback_failed", Message: fmt.Sprintf("%s%s could not be rolled back (%v); rolled back before it: %s. Run the rollback again to finish it", servicePrefix, m.svc, err, before)}
			}
			done = append(done, m.svc)
		}
		n, err := s.record(ctx, id, actor, fmt.Sprintf("rollback to release %d", number))
		if err != nil {
			return &OpError{Code: "release_not_recorded", Message: fmt.Sprintf("every part is rolled back, but the release could not be recorded: %v", err)}
		}
		emit("release", fmt.Sprintf("%s release %d (rollback to release %d)", app.GetName(), n, number))
		return nil
	})
	d.hold(plans, op.GetName())
	resp.Operation = op
	return connect.NewResponse(resp), nil
}
