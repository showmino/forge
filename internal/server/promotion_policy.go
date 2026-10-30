package server

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"connectrpc.com/connect"

	"github.com/showmino/forge/policy"
)

// promotionAllowed checks a promotion of svc from one environment to the
// next against the rules of package forge.deploys, with what Argo CD says
// about the environment it takes from. A rollback is never checked.
func (d *Deploys) promotionAllowed(ctx context.Context, svc, from, to string, src deployValues, deployCommit string) error {
	if d.o.Policy == nil {
		return nil
	}
	// src and deployCommit come from two reads of the deploy repo; a deploy
	// between them would let the rules check another deploy than the one
	// promoted.
	checked, err := d.o.Repo.Read(ctx, valuesPath(svc, from), deployCommit)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo, so the promotion could not be checked: %w", err))
	}
	if v, err := parseValues(checked); err != nil || !reflect.DeepEqual(v, src) {
		return connect.NewError(connect.CodeAborted, fmt.Errorf("%s changed while the promotion was checked; try again", from))
	}
	// For a service that runs a chart, its chart's application counts too.
	st, err := d.appStatus(ctx, svc, from, src)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("argo cd, so the promotion could not be checked: %w", err))
	}
	res, err := d.o.Policy.EvaluatePromotion(ctx, policy.Promotion{
		Service: svc, From: from, To: to,
		Source: policy.Source{
			Sync: st.Sync, Health: st.Health, AtRevision: d.runsValues(ctx, svc, from, deployCommit, checked, st.Revisions),
			Commit: src.Forge.Commit, DeployCommit: shortSHA(deployCommit), Version: src.Forge.Version, Image: src.Image,
		},
		Actor: policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
	})
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("policy could not be evaluated, so %s%s was not promoted: %w", servicePrefix, svc, err))
	}
	noteAuditDetail(ctx, "policy_rules", d.o.Policy.Fingerprint())
	if res.Denied() {
		noteAuditDetail(ctx, "policy", "denied")
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("promoting %s%s to %s is denied by policy: %s", servicePrefix, svc, to, strings.Join(res.Deny, "; ")))
	}
	noteAuditDetail(ctx, "policy", "passed")
	return nil
}

// runsValues reports whether Argo CD's synced revisions hold want, env's
// values at deployCommit. Argo CD resolves the deploy repo to its head, so
// after a later commit (of any service) it reports that one: a revision then
// counts when the values file there equals want.
func (d *Deploys) runsValues(ctx context.Context, svc, env, deployCommit string, want []byte, revisions []string) bool {
	if slices.Contains(revisions, deployCommit) {
		return true
	}
	for _, rev := range revisions {
		if got, err := d.o.Repo.Read(ctx, valuesPath(svc, env), rev); err == nil && bytes.Equal(got, want) {
			return true
		}
	}
	return false
}
