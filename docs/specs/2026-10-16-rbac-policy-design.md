# RBAC and policy

Oct 16, 2026 · Showmino · Approved

Today every member of the organization can read everything, and the members of
one writer team (`showmino:platform-engineering`) can change every service;
nobody else can change anything. A service's team (`owner_team`) is shown in
the catalog but grants nothing. Policy (Rego) checks only resource plans.

This milestone makes a service's team the people who work on it, keeps the
platform-wide actions with platform-engineering, adds a first deploy policy,
and lets every client ask what the caller may do.

## Decisions

The owner's (2026-10-16):

| Question | Decision |
| --- | --- |
| Who may change a service | **Team ownership.** The members of a service's owner team work on it; platform-engineering may do everything, on every service. |
| Who may promote to production and roll back | Any member of the owner team. |
| What stays platform-engineering only | Deleting a service; changing its address or claiming a domain (`forge service address`, `--host`, `--claim`); registering an existing repo; creating a service for a team you are not in; moving a service to another team; applying resource plans in production. |
| Policy beyond resources | A promotion goes ahead only when staging is synced and healthy. |
| Seeing your permissions | `forge auth can <action> [service]`, and per-service permissions in the CLI and the dashboard. |

By convention (say if you want otherwise):

| Question | Decision |
| --- | --- |
| Roles | Three, fixed in code: **reader** (every member), **team** (member of the service's owner team), **admin** (a member of an admin group). No role bindings to store or manage; GitHub teams are the source of truth. |
| Where the role table lives | Go (`internal/server/rbac.go`): one table action → lowest role, so a handler calls `authorize(ctx, action, service)`. Rego stays for decisions about *content* (plans, promotions), not for who someone is. |
| Admin groups | New flag `--admin-groups` / `FORGE_ADMIN_GROUPS`; when unset it takes `--writer-groups`, so the live config keeps working. `--writer-groups` is then deprecated. |
| A person in no owning team | A reader: they may read and nothing more, as today for non-writers. They can still create a service for a team they are in. |
| Promotion of an unhealthy staging | Refused for everyone, admins too, with the reasons (exit 5, like every policy denial). No override flag in this milestone: the fix is to repair staging, and a **rollback is never gated**, so an incident in production can always be undone. |
| CI workloads | Unchanged: a repo's own CI may deploy only its own service, only to staging (`workloadProcedures`, `ci_scope.go`). |

## Who may do what

| Action | reader | team | admin |
| --- | --- | --- | --- |
| Read the catalog, deploys, operations, docs, audit log, templates | ✓ | ✓ | ✓ |
| `forge create` for a team you are in | | ✓ | ✓ |
| `forge create` for any team | | | ✓ |
| `service update` (display name, owners, labels, lifecycle, deps, repo) | | ✓ | ✓ |
| `service update --team` (move to another team) | | | ✓ |
| `service register`, `service delete`, `service address` | | | ✓ |
| `deploy start`, `deploy promote`, `deploy rollback` | | ✓ | ✓ |
| `env create/extend/destroy` (previews) | | ✓ | ✓ |
| `op cancel` (of an operation on the service) | | ✓ | ✓ |
| `resource plan`; `resource apply` in staging | | ✓ | ✓ |
| `resource apply` in production | | | ✓ |

"team" means a member of the service's `owner_team`, as GitHub says
(`showmino:<team slug>`). An operation or preview belongs to the service it
was made for: forge-api works it out from the operation's target
(`services/<svc>`, `deployments/<svc>/<env>`, or the preview
`environments/<name>` and its service). The person who started an operation
may always cancel it.

## How it works

- **Actions.** Each write RPC names its action (`service.update`,
  `deploy.promote`, ...: the names the audit log already uses, plus
  `service.update.team`, `resource.apply.production` and `service.create.any_team`).
  `authorize(ctx, action, service)` replaces `require(ctx, PermServicesWrite)`
  in every handler; it loads the service's owner team and compares it with the
  caller's groups. A refusal is `PERMISSION_DENIED` naming the action and the
  team that may do it ("deploy.promote on services/payments needs the team
  showmino:payments-team"), and is written to the audit log, as refusals are
  today.
- **Sessions keep the right teams current.** Sessions recheck the person's
  membership hourly, but only of the writer groups. They will recheck the
  admin groups **and every team that owns a service** (the catalog's distinct
  `owner_team`s), so a person removed from a team loses its rights within the
  hour, not at their next login.
- **`TestPermissions`.** A new read RPC on `AuthService`:
  `TestPermissions(service, actions[]) → allowed[]` (the shape of Google's
  `testIamPermissions`). The CLI's `forge auth can deploy.promote payments`
  prints yes/no and exits 0 or 5; `forge auth status` adds the caller's role
  (`admin`, or the teams they are in); `forge service get` shows a "you can"
  line. The dashboard uses the same RPC to hide or disable what the caller may
  not do. Classified in `auditReads`.
- **Promotion policy.** A new Rego package `forge.deploys`
  (`policy/deploys.rego`), built into the binary like `forge.resources`. Input:
  the service, `from`/`to`, staging's deployment (Argo CD sync and health,
  commit, version) and the actor. Rules: deny when staging is not `Synced`, deny
  when it is not `Healthy`, and deny when Argo CD has not yet synced
  staging's latest deploy (it can still report Synced and Healthy about the
  one before; added while building M2). `PromoteDeploy` evaluates it after `authorize`;
  `--dry-run` shows the result too. Every rule gets a passing and a failing
  case in `policy/policy_test.go`.

## Milestones

1. **RBAC in the API and the CLI.** Roles and `authorize` in every write
   handler; `--admin-groups`; sessions recheck owning teams;
   `TestPermissions`, `forge auth can`, roles in `auth status` and
   `service get`. Tests: the harness gets a third static token, `t-team`
   (member of one team), next to `t-writer` (admin) and `t-reader`, and a test
   per row of the table above.
2. **Promotion policy.** `policy/deploys.rego`, wired into `PromoteDeploy`.
3. **Dashboard and docs.** forge-web hides or disables actions with
   `TestPermissions`; the techdocs pages (who can do what, `forge auth can`,
   the promotion rule) and the README.

## Rollout

forge-api first (the API is backward compatible: with no `FORGE_ADMIN_GROUPS`
the writer groups are the admins, and members of a service's team gain
rights they did not have). Then forge-web. `live.env` adds
`FORGE_ADMIN_GROUPS=showmino:platform-engineering`; a rollout as in
`docs/agents/verifying.md`, with the owner's OK.

## Exit gate

On k3s, with a second account that is in `showmino:test-team` and not in
platform-engineering, and a throwaway service of `test-team`:

- it deploys, promotes and rolls back that service, and makes a preview of it;
- it is refused (exit 5) on a platform-engineering service, on deleting its
  own service, on `service address` and on moving it to another team;
- `forge auth can` and the dashboard agree with what the API did;
- a promotion while staging is unhealthy is refused with the reason, and the
  rollback still works;
- the audit log shows each refusal with its actor and action;
- after the account is removed from `test-team`, it loses the rights within
  the hour without logging in again.

## Not in this milestone

- Roles within a team (GitHub team maintainers), per-service role bindings,
  custom roles.
- Deploy freeze windows; moving the secret and quota rules into Rego.
- Restricting public exposure to platform-engineering.
