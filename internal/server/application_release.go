package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

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

// startTogether starts one operation that holds the lock of every target
// ("<service>/<env>"), or starts nothing. The locks are taken, the
// operation started and its name recorded under one hold of d.mu, as
// startMutation does for one target, so the work cannot free a lock before
// it is recorded. They are freed when the work ends.
func (d *Deploys) startTogether(ctx context.Context, kind, target, actor string, targets [][2]string, work Work) (*forgev1.Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, t := range targets {
		if err := d.claimIn(ctx, t[0], t[1]); err != nil {
			for _, done := range targets[:i] {
				delete(d.running, done[0]+"/"+done[1])
			}
			return nil, err
		}
	}
	op := d.o.Operations.Start(ctx, kind, target, actor, func(ctx context.Context, emit func(string, string)) error {
		defer func() {
			for _, t := range targets {
				d.unlock(t[0] + "/" + t[1])
			}
		}()
		return work(ctx, emit)
	})
	for _, t := range targets {
		d.running[t[0]+"/"+t[1]] = op.GetName()
	}
	return op, nil
}

// deployCommitOf is the deploy repo commit that last changed what svc runs
// in env: "" when it runs nothing there.
func (d *Deploys) deployCommitOf(ctx context.Context, svc, env string) (string, error) {
	_, commit, err := d.current(ctx, svc, env)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return "", nil
	}
	return commit, err
}

// unchanged checks, at the start of an application's operation and with
// its locks held, that every part still runs what it ran when the
// operation was planned: seen maps "<service>/<env>" to the deploy commit
// seen then. A deploy that ended in between would otherwise be overwritten
// by a plan that never saw it.
func (d *Deploys) unchanged(ctx context.Context, seen map[string]string) error {
	for _, target := range slices.Sorted(maps.Keys(seen)) {
		svc, env, _ := strings.Cut(target, "/")
		now, err := d.deployCommitOf(ctx, svc, env)
		if err != nil {
			return err
		}
		if now != seen[target] {
			return &OpError{Code: "changed", Message: fmt.Sprintf("%s%s changed in %s while this was being prepared; nothing was done. Run it again", servicePrefix, svc, env)}
		}
	}
	return nil
}

// productionTargets are the locks an application's operation holds: the
// production of every part that has one, changed by the operation or not,
// so that the release it records is what production runs.
func (s *applicationService) productionTargets(ctx context.Context, app *forgev1.Application) ([][2]string, error) {
	var out [][2]string
	for _, p := range app.GetParts() {
		entry, err := s.catalog.get(ctx, p.GetService())
		if err != nil {
			return nil, err
		}
		if slices.Contains(serviceEnvs(entry), finalEnv) {
			out = append(out, [2]string{strings.TrimPrefix(p.GetService(), servicePrefix), finalEnv})
		}
	}
	return out, nil
}

// sameChart reports whether two deploys run the same chart with the same
// shared values. Both nil is the same.
func sameChart(a, b *deployChart) bool {
	if a == nil || b == nil {
		return a == b
	}
	av, _ := yaml.Marshal(a.Values)
	bv, _ := yaml.Marshal(b.Values)
	return a.Repo == b.Repo && a.Name == b.Name && a.Version == b.Version && bytes.Equal(av, bv)
}

// sameRun reports whether two values of one service in one environment
// run the same thing: who deployed them and when, a restart, a scale, and
// what Forge works out again at every deploy (the database, the links) do
// not count.
func sameRun(a, b deployValues) bool {
	plain := func(v deployValues) []byte {
		v.Forge.Actor, v.Forge.Operation, v.Forge.Kind, v.Forge.Time = "", "", "", ""
		v.RestartedAt, v.Replicas, v.Autoscaling, v.Database, v.Links = "", 0, nil, nil, nil
		return v.marshal()
	}
	return bytes.Equal(plain(a), plain(b))
}

// partsText says what an application's operation does to each part, for
// the audit log.
func partsText(parts []*forgev1.PlannedPart) string {
	said := make([]string, len(parts))
	for i, p := range parts {
		from := p.GetFromVersion()
		if from == "" {
			from = "nothing"
		}
		said[i] = fmt.Sprintf("%s %s to %s", strings.TrimPrefix(p.GetService(), servicePrefix), from, p.GetToVersion())
	}
	return strings.Join(said, "; ")
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
	// What each planned part ran in production when it was planned, by its
	// deploy commit: "" for a part that was not there. seen has that, and
	// staging's, to check again once the locks are held.
	before, seen := map[string]string{}, map[string]string{}
	fromVersion := map[string]string{}
	// Each part's promotion policy notes its outcome on the call's audit
	// event under the same keys: they are collected per part.
	policyKeys := []string{"policy", "policy_warnings", "policy_modes"}
	policy := map[string][]string{}
	for _, p := range app.GetParts() {
		svc := strings.TrimPrefix(p.GetService(), servicePrefix)
		entry, err := s.catalog.get(ctx, p.GetService())
		if err != nil {
			return nil, err
		}
		if i := entry.GetInfrastructure(); (i != nil && !i.GetRuns()) || len(serviceEnvs(entry)) != len(deployEnvs) {
			continue // nothing of it goes from staging to production
		}
		staged, stagedCommit, err := d.current(ctx, svc, from)
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue // not in staging yet
		}
		if err != nil {
			return nil, err
		}
		prod, prodCommit, err := d.current(ctx, svc, to)
		switch {
		case connect.CodeOf(err) == connect.CodeNotFound:
			prodCommit = ""
		case err != nil:
			return nil, err
		case prod.Forge.Commit == staged.Forge.Commit && prod.Image == staged.Image && sameChart(prod.Chart, staged.Chart):
			continue // production runs it already
		default:
			fromVersion[svc] = shownVersionOf(&prod)
		}
		m, err := d.planPromote(ctx, svc, from, to, req.Msg.GetBump())
		for _, k := range policyKeys {
			if v := takeAuditDetail(ctx, k); v != "" {
				policy[k] = append(policy[k], svc+": "+v)
			}
		}
		if err != nil {
			return nil, partErr(p.GetService(), err)
		}
		plans = append(plans, m)
		before[svc] = prodCommit
		seen[svc+"/"+to], seen[svc+"/"+from] = prodCommit, stagedCommit
	}
	for _, k := range policyKeys {
		noteAuditDetail(ctx, k, strings.Join(policy[k], "; "))
	}
	if len(plans) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has nothing to promote: production runs what staging runs", app.GetName()))
	}
	plans = linkOrder(plans)
	var planned []*forgev1.PlannedPart
	for _, m := range plans {
		planned = append(planned, &forgev1.PlannedPart{Service: servicePrefix + m.svc, FromVersion: fromVersion[m.svc], ToVersion: shownVersionOf(&m.v)})
	}
	noteAuditDetail(ctx, "parts", partsText(planned))
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.PromoteApplicationResponse{Parts: planned}), nil
	}
	targets, err := s.productionTargets(ctx, app)
	if err != nil {
		return nil, err
	}
	actor := actorOf(ctx)
	op, err := d.startTogether(ctx, kindApplicationPromote, app.GetName(), actor, targets, func(ctx context.Context, emit func(string, string)) error {
		if err := d.unchanged(ctx, seen); err != nil {
			return err
		}
		for i, m := range plans {
			err := d.run(ctx, m, emit)
			if err == nil {
				continue
			}
			// Production is not left half promoted: the part that failed
			// and the parts before it go back to what they ran.
			code := "promote_failed"
			if ctx.Err() != nil {
				code = OpCodeCancelled
			}
			emit("failed", fmt.Sprintf("%s: %v", m.svc, err))
			back := s.putBack(ctx, plans[:i+1], before, emit)
			return &OpError{Code: code, Message: fmt.Sprintf("%s%s could not be promoted (%v); %s", servicePrefix, m.svc, err, back)}
		}
		n, err := s.record(ctx, id, actor, "")
		if err != nil {
			return &OpError{Code: "release_not_recorded", Message: fmt.Sprintf("every part is promoted, but the release could not be recorded: %v", err)}
		}
		emit("release", fmt.Sprintf("%s release %d", app.GetName(), n))
		return nil
	})
	if err != nil {
		return nil, err
	}
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
		rb, err := d.planRestore(ctx, m.svc, m.env, commit)
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
	n, err := s.releases.AddRelease(ctx, r)
	if err != nil {
		// Another writer may have taken the number: once more.
		n, err = s.releases.AddRelease(ctx, r)
	}
	return n, err
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

// rollbackPlan is what returning an application to a release changes.
type rollbackPlan struct {
	plans   []mutation
	planned []*forgev1.PlannedPart
	// seen is the deploy commit each planned part ran in production.
	seen map[string]string
}

// planRelease plans the return of every part to what it ran in release r.
// A part that runs that already, one that left the application and one
// that runs nothing in production now are left out.
func (s *applicationService) planRelease(ctx context.Context, app *forgev1.Application, r store.Release) (rollbackPlan, error) {
	d := s.deploys
	out := rollbackPlan{seen: map[string]string{}}
	parts := map[string]bool{}
	for _, p := range app.GetParts() {
		parts[p.GetService()] = true
	}
	for _, rp := range r.Parts {
		if !parts[rp.Service] {
			continue // it left the application since
		}
		svc := strings.TrimPrefix(rp.Service, servicePrefix)
		cur, curCommit, err := d.current(ctx, svc, finalEnv)
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue // taken down since; a rollback does not bring it back
		}
		if err != nil {
			return out, err
		}
		if curCommit == rp.DeployCommit {
			continue
		}
		data, err := d.o.Repo.Read(ctx, valuesPath(svc, finalEnv), rp.DeployCommit)
		if err != nil {
			return out, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read what %s ran in release %d (%s): %w", rp.Service, r.Number, shortSHA(rp.DeployCommit), err))
		}
		then, err := parseValues(data)
		if err != nil {
			return out, connect.NewError(connect.CodeInternal, err)
		}
		if sameRun(cur, then) {
			continue // it runs that already
		}
		m, err := d.planRestore(ctx, svc, finalEnv, rp.DeployCommit)
		if err != nil {
			return out, partErr(rp.Service, err)
		}
		out.plans = append(out.plans, m)
		out.planned = append(out.planned, &forgev1.PlannedPart{Service: rp.Service, FromVersion: shownVersionOf(&cur), ToVersion: rp.Version})
		out.seen[svc+"/"+finalEnv] = curCommit
	}
	return out, nil
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
	var plan rollbackPlan
	switch n := int(req.Msg.GetRelease()); {
	case n < 0:
		return nil, invalid("release must not be negative")
	case n == 0:
		// The newest release that is not what production runs. That is the
		// one before the newest, unless production has moved since (a part
		// promoted on its own, or a promotion whose release was not
		// recorded): then it is the newest, the last state known to be good.
		for i := range rs {
			if plan, err = s.planRelease(ctx, app, rs[i]); err != nil {
				return nil, err
			}
			if len(plan.plans) > 0 {
				target = &rs[i]
				break
			}
		}
		if target == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no earlier release to roll back to", app.GetName()))
		}
	default:
		for i := range rs {
			if rs[i].Number == n {
				target = &rs[i]
			}
		}
		if target == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no release %d in its last %d", app.GetName(), n, maxReleases))
		}
		if plan, err = s.planRelease(ctx, app, *target); err != nil {
			return nil, err
		}
		if len(plan.plans) == 0 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs release %d already", app.GetName(), target.Number))
		}
	}
	plans := plan.plans
	resp := &forgev1.RollbackApplicationResponse{Parts: plan.planned, Release: releaseProto(*target)}
	noteAuditDetail(ctx, "to_release", strconv.Itoa(target.Number))
	noteAuditDetail(ctx, "parts", partsText(plan.planned))
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(resp), nil
	}
	targets, err := s.productionTargets(ctx, app)
	if err != nil {
		return nil, err
	}
	actor, number := actorOf(ctx), target.Number
	op, err := d.startTogether(ctx, kindApplicationRollback, app.GetName(), actor, targets, func(ctx context.Context, emit func(string, string)) error {
		if err := d.unchanged(ctx, plan.seen); err != nil {
			return err
		}
		var done []string
		for _, m := range plans {
			if err := d.run(ctx, m, emit); err != nil {
				before := "none"
				if len(done) > 0 {
					before = strings.Join(done, ", ")
				}
				code := "rollback_failed"
				if ctx.Err() != nil {
					code = OpCodeCancelled
				}
				return &OpError{Code: code, Message: fmt.Sprintf("%s%s could not be rolled back (%v); rolled back before it: %s. Run the rollback again to finish it", servicePrefix, m.svc, err, before)}
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
	if err != nil {
		return nil, err
	}
	resp.Operation = op
	return connect.NewResponse(resp), nil
}
