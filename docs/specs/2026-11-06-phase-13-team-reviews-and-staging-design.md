# Phase 13 · Team reviews and staging verification

Approved on 2026-11-06

## Summary

Require one approval from the Forge service's owning GitHub team before any
pull request to `main` can merge. Keep the existing infrastructure flow:
merge deploys to staging when configured, then asks Forge for a separate
production approval. Verify that sequence with an isolated Azure test repo.

## Team review policy

- Crossplane's GitHub `RepositoryRuleset` requires one approving review and a
  code-owner approval, with no bypass actors.
- Forge writes an effective `CODEOWNERS` file for each new repo before
  enabling its ruleset. It names only the owning team for every file. For an
  existing repo, Forge proposes a change replacing its effective CODEOWNERS
  file; its earlier per-path owners are not retained, because they could
  satisfy GitHub's code-owner requirement without an owning-team approval.
- An exception names exactly one alternate reviewer team and a reason in a
  separate `review-exceptions/<service>.yaml` record in the deploy repo. It
  must be merged by PR with approval from an authorized platform team, using
  the existing production-exception review validation. The alternate team
  replaces the owning team for CODEOWNERS review; the one-approval and
  code-owner requirements remain enabled. The alternate team must already
  have `maintain` access; Forge does not grant or revoke repository access as
  a side effect of an exception. Revocation removes the deploy-repo record
  and proposes a CODEOWNERS restoration PR. Since the alternate team is still
  the current code owner on `main`, it must approve that PR; this preserves
  the no-bypass and sole-codeowner rules.
- For existing repos, submit CODEOWNERS changes as repo PRs and enable the
  stricter rules only after those PRs merge. Adopt only repos with confirmed
  Forge provenance. For `forge-web`, use a one-time owner-confirmed mapping
  to its immutable GitHub repo ID; do not broaden the automatic provenance
  heuristic.

## Staging and production verification

The shared workflow already deploys to staging on `main` and requests
production approval only after staging succeeds (or is skipped). Preserve
that behavior. Create a test repo with staging and production environments
and a Bicep template that deploys a non-billable resource into a dedicated,
pre-created Azure staging resource group.

Use test-only identities: staging deployment access limited to that resource
group, PR-plan access limited to that same test scope, and no Azure role for
the test repo's production deployment identity. Create no production
resources and do not approve or dispatch a production deployment. Verify that
the staging apply succeeds, Forge records a pending production approval, and
no production deployment job or Azure write runs.

## Acceptance criteria

- A new and an existing eligible Forge repo cannot merge a `main` PR without
  an approval from its owning team; an unrelated team's approval alone does
  not satisfy the rule.
- A valid exception permits the named alternate team to provide that
  approval, without reducing the one-approval requirement.
- The test repo's staging deployment succeeds, its production approval
  remains pending, and no production deployment is started.
- Tests cover generated rules, CODEOWNERS preservation, exception grant and
  revocation ordering, and the staging/approval workflow.

## Gate

The owner approved this design before implementation. Any live-install
rollout or Azure resource changes remain subject to the install runbook and
explicit owner approval.
