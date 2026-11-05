# Phase 12 · Infrastructure repos

Proposed on 2026-11-03, from a conversation with the owner. It replaces the
phase 12 of `2026-10-29-deploy-sources-design.md` (the app catalog), which
moves to phase 14. The owner gave the go-ahead and confirmed the names the
same day.

**Done.** The exit gate passed on the owner's install on 2026-11-05, and the
phase closed with Forge 0.44.0. See "Exit gate".

## Why

A developer who needs infrastructure code (Bicep, OpenTofu or Terraform)
today asks the platform team, who make a repo by hand and set it up: GitHub
environments, an approval on production, the cloud credentials as secrets, the
cloud side that lets the repo's workflow sign in. It is slow, it differs every
time, and the credentials pass through people's hands.

Forge does it in one step: the developer picks a template, a target and the
environments they want, and gets a repo that deploys on merge, with
production waiting for their team's approval in Forge. Nobody types or copies
a credential.

## Goals

- `forge create` (and the dashboard's Create page) makes an infrastructure
  repo from an infrastructure template, for a target the platform team set up.
- The repo gets the GitHub environments the developer chose, `staging`,
  `production` or both. The provider integration configures access for each
  selected environment using the scope and isolation model chosen by the
  platform team.
- A production run waits for an approval in Forge by a member of the team that
  owns the repo, not the person who started the run.
- A team of one can get an exception: they may approve their own production
  runs. Only the platform team can grant it.
- The target's values are set as environment secrets and variables, and the
  provider integration configures the workflow's cloud access for this repo.
- Forge coordinates GitHub-side settings and approvals; provider integrations
  and workflows supply cloud-specific access and deployment behavior.

## Non-goals

- Forge running the infrastructure code. The repo's GitHub Actions do that;
  Forge's own OpenTofu runner (phase 5) is unrelated.
- Forge choosing a cloud resource hierarchy or managing a target's shared
  state storage. The platform team chooses the target integration, which may
  use existing scope or provision service-specific scope and access.
- Deploying the repo to the cluster. An infrastructure repo has no Forge
  deploy; it is in the catalog for its owner, its docs and its audit trail.
- Adopting existing, hand-made repos, or moving a repo to another target.
  Later, if asked for.
- Drift detection of the infrastructure itself.

## Anti-goals

- A credential that passes through Forge, the CLI, the dashboard or a person.
  Forge never reads the target's secret values; Crossplane copies them from a
  Kubernetes Secret into GitHub.
- A repo's own files deciding who approves production or which cloud it can
  reach. `forge.yaml` and the workflows are in the developer's hands; access
  lives in the deploy repo and in Forge.
- A production deploy that skipped the approval. Removing the approval step
  from a repo's workflow must not reach production.
- Cloud code in Forge (an Azure SDK, an AWS SDK).

## Why the approval is in Forge, not in GitHub

GitHub's environment reviewer features vary by repository visibility and
GitHub plan. Forge enforces production approval itself, giving infrastructure
repos the same approval flow and audit record across supported plans.

## How it works

### Targets

A target names the destinations available to developers and configures each
environment's GitHub settings and provider access. The platform team defines
targets in `infrastructure.targets`. Its provider integration may use an
existing scope or provision a new one; the deployment boundary can be a
resource group, subscription, account, project or another provider-specific
scope. Forge does not require resource groups.

Each environment's Secret holds values supplied by the install's chosen
secret manager. Its `access` template creates provider-specific Crossplane
resources for the repository and environment. The template receives `.Org`,
`.Repo`, `.RepoID`, `.Environment`, `.Team`, `.Subject` and `.PlanSubject`.
The provider integration and shared workflow define identity, permissions,
state storage and deployment behavior. Configure a separate, read-only
identity for pull-request plans; never give unreviewed PR code deployment
permissions or state write access.

### Templates

Two new templates in the templates repo, both `type: infrastructure`:

```
.github/workflows/deploy.yml
.gitignore
CODEOWNERS                  * @<org>/<team>
README.md
forge.yaml
infrastructure/bicep/       main.bicep, main.bicepparam                           (template "bicep")
infrastructure/terraform/   main.tf, variables.tf, providers.tf, terraform.tfvars  (template "opentofu")
```

- `type: infrastructure` in `template.yaml` (next to `service` and `file`):
  make a repo, register it, and set up its environments for a target.
- `deploy.yml` is short: it calls the shared workflow (`infrastructure.workflow`
  above) with the tool and the folder. The shared workflow lives in a repo the
  platform team owns, and holds the jobs. The initial templates and shared
  workflow target Azure; other cloud providers need their own templates,
  provider access resources and shared workflow:
  - on every pull request: `bicep build` or `tofu validate`, plus a required
    `what-if` or `tofu plan` for every chosen account, including production
    when that is the repo's only account. These are plans, not preview
    environments: no GitHub environment or deployed resources are created;
  - on a merge to `main`: deploy to `staging` (when the repo has it), then
    ask Forge for production's approval, and deploy to `production` once
    Forge says it is approved for that commit.
  - Azure deployment jobs sign in with `azure/login` from the environment's
    secrets and GitHub's OIDC token. PR plans use separate read-only
    identities, delivered as repository secrets, without naming an environment.
    The `opentofu` template runs `tofu`; one input switches it to `terraform`.
  - Bicep what-if and deployments use `Azure/bicep-deploy@v2.5.0`.
    Deployment stacks delete unmanaged resources and resource groups and
    use `deny-settings-mode: denyDelete`. A what-if with a read-only identity
    uses `type: deployment` and `validation-level: providerNoRbac`. Version
    2.5.0 accepts this validation level only for deployments, not stacks.
    This what-if does not report stack unmanage deletions; removals must also
    be reviewed in the template diff.
- One parameter file per tool. What differs between environments is read
  from the GitHub environment (`readEnvironmentVariable()` in
  `main.bicepparam`, `TF_VAR_...` for OpenTofu).
- For the initial Azure OpenTofu template, state uses the `azurerm` backend in
  the target's storage account,
  isolated to a private container per repository and environment, named
  `forge-<repo-id>-<environment>`. The state key is `<repo>.tfstate`, signed
  in with OIDC (`use_oidc`, `use_azuread_auth`). The deploy identity gets
  **Storage Blob Data Contributor** and the plan identity gets **Storage Blob
  Data Reader** on that container only; Owner alone does not give data access.
- Other providers use their own state backend and provider-specific access
  model. Forge does not require the Azure account or resource layout.

### Pull requests and plan identities (owner's amendment, 2026-11-04)

- All repositories created by Forge, existing and future, require a PR for
  `main`. Crossplane's namespaced GitHub `RepositoryRuleset` enforces it
  without administrator or App bypasses, and prevents force pushes and deletion.
  Bootstrap is the sole exception: Forge writes the scaffold before creating
  the ruleset. Connected repositories are outside this scope.
- `repositories.enabled`, with its namespace and GitHub provider config,
  enables the chart's `forge-repositories` ApplicationSet. Rules are stored
  at `repositories/<repo>/rules.yaml`, independently of infrastructure access.
  Startup reconciliation backfills successful template creates recorded in
  the audit log; a dry run, a failed create or a connected repo is not proof.
  Audit records omit false flags, so an absent `validate_only` means a real
  create, as does explicit `false`; `true` and unknown values never qualify.
  Catalog deletion does not remove protection. Rulesets use
  `managementPolicies: [Observe, Create, Update, LateInitialize]`, excluding
  `Delete`, so removing their Kubernetes objects does not open `main`.
  The namespaced provider does not support the older `deletionPolicy` field.
- Infrastructure rules require `infrastructure / pr-checks` from GitHub
  Actions and an up-to-date branch. That check succeeds only if validation
  and every account's plan/what-if succeed; missing configuration fails.
- A valid, active team-of-one production exception for an infrastructure
  service waives approving reviews for that service's pull requests. The
  pull-request rule and all required checks remain. Granting or revoking the
  exception changes its review rules in the same deploy-repo pull request;
  startup reconciliation applies the current exception state to existing
  services.
- Each target environment has a `plan` block: `secret`, `githubSecrets`,
  `githubVariables`, `access`. It uses a different identity and Secret from
  deployment. Crossplane copies its settings into repository-level
  `FORGE_PLAN_<ENV>_*` secrets/variables. `FORGE_PLAN_ENVIRONMENTS` is a JSON
  array of the repo's chosen accounts. No new GitHub environments are needed.
- The plan access template uses `.Environment` for naming/account selection
  and `.Subject` for the shared workflow's PR subject, ending in
  `:pull_request`, with immutable repository IDs. Deployment's `:environment:`
  trust and production's main-only branch policy remain unchanged.
- A plan identity must have only the read permissions and state access needed
  to calculate a plan. The provider integration and workflow must prevent
  pull-request code from deploying resources or writing deployment state.
  Merge-time deployment uses deployment access and plans again.
- Existing targets without `plan` remain readable for migration, but creating
  a repo against one fails with a configuration error. Roll out the plan
  settings and updated shared workflow alongside enforcement, not afterward.

### forge.yaml

```yaml
apiVersion: forge/v1
service:
  name: infra-network
  team: platform
infrastructure:
  tool: opentofu       # or bicep
```

`infrastructure` marks the repo: no `run`, no `chart`, deploys and previews
refused with a message that says the repo deploys itself. It names neither the
target nor the environments: Forge records those in the deploy repo, so
editing `forge.yaml` cannot move a repo to another account.

### What Forge writes

`forge create infra-network --template opentofu --target azure --environments
production --team platform` (`--environments` defaults to every environment
the target has):

1. Checks the caller may create for the team, the team may use the target,
   and each chosen environment has room (`maxRepos`).
2. Creates the repo from the template, as for a service, and registers it.
3. Sets the repo's OIDC subject claims (GitHub's per-repo customization, with
   the App, before the first push) to `job_workflow_ref`, `repo`, `context`,
   so a token says which workflow asked for it. The subject of the
   production credential is then
   `job_workflow_ref:<infrastructure.workflow>:repo:<org>/<repo>:environment:production`.
   A repo whose own workflow skips the shared one cannot sign in to
   production, and since GitHub's default subject starts with `repo:`, a
   reset setting fails closed whatever an environment is named. (The
   installed Crossplane GitHub provider has no kind for this setting, so it
   is not put back if changed; it fails closed instead.)
4. Writes `infrastructure/<repo>/` in the deploy repo with one commit:
   `target.yaml` (target, environments) and the Crossplane objects:
   - an `Environment` (`repo.github.m.upbound.io`) per chosen environment,
     `canAdminsBypass: false`; production with a deployment branch policy of
     `main` only;
   - an `EnvironmentSecret` per `githubSecrets` entry (value from the
     environment's Secret, `plaintextValueSecretRef`) and an
     `EnvironmentVariable` per `githubVariables` entry;
   - the environment's `access`.
5. An ApplicationSet `forge-infrastructure` (in the `forge` chart, like
   `forge-charts`) makes one Argo CD Application per `infrastructure/*`, into
   `infrastructure.namespace`, in an AppProject that allows only the kinds
   above and the kinds the targets' `access` use. Crossplane does the rest,
   and puts back what someone changes by hand.
6. The environment's access template creates provider-specific Crossplane
   resources for the repository and environment. The provider integration
   grants the required deploy and read-only plan access, and configures the
   appropriate target scope, identity trust and state storage. It may use an
   existing scope or provision one; the cloud resource hierarchy is not fixed
   by Forge.
7. The operation finishes when the Crossplane objects are Ready, and says
   which one is not and why.

Deleting the repo's service deletes `infrastructure/<repo>/`, so its GitHub
environments and access objects are removed according to their configured
lifecycle. Cloud resources and state are not implicitly deleted with the repo;
the platform's provider integration defines their lifecycle.

### The approval

- **Asking:** the shared workflow's production step calls Forge with the job's
  OIDC token (a CI workload, as for deploys from CI; the procedures are in
  `workloadProcedures`). Forge checks the token is this repo's, from the
  default branch, from the shared workflow, from a `push` run (so its actor
  is who pushed the commit, not someone commenting on an issue), and that the
  commit is the branch's head (a re-run of an old push cannot ask). It
  records an approval request: repo, commit, run, who started it (`actor`)
  and who started this attempt (`triggering_actor`). A newer commit's
  request supersedes one still waiting, or approved but not claimed.
- **Approving:** a member of the owning team approves or rejects in the
  dashboard (the repo's page, and a list of what waits on you) or with
  `forge approval approve|reject <id>`. Forge refuses an approval by the
  person who started the run or re-ran it, unless the repo has the
  exception. Membership is the login's groups. Logins are compared, which a
  GitHub user can rename; storing user IDs is a later hardening.
- **Deploying:** an approval can take a day; no runner waits for it. The
  production job ends after asking; on approval Forge starts the repo's
  workflow (`infrastructure.repoWorkflow`, default `deploy.yml`) on its
  default branch with the input `approval` (the App's Actions: write). That
  run claims the approval once, only as a `workflow_dispatch` run of the
  shared workflow at the approved commit: if the branch moved on, the claim
  is refused and the newest commit asks for its own approval. A start that
  failed is kept on the approval; approving again retries it.
- Each step (asked, approved, rejected, claimed) is in the audit log.

### The exception

A team of one cannot get a second approver. The platform team can let a
repo's team approve its own production runs:

- **Request:** `forge approval exception request <repo> --reason ...` opens a
  pull request in the deploy repo adding `exceptions/<repo>.yaml` (who asked,
  why, when, and how many people the team has).
- **Grant:** only the platform team, the install's admin groups
  (`--admin-groups`), can approve it. The operator gives the deploy repo
  `CODEOWNERS` for `exceptions/` and a ruleset that requires a code owner's
  review. forge-api never commits to `exceptions/` itself, and honours a
  file only if its last change on `main` is the squash or rebase merge of a
  pull request whose last commit a member of an admin team (the admin groups
  that are `<org>:<team>`) other than the requester approved, and the file
  names the service's current team and was asked for after the service was
  made. It checks that with the GitHub API each time a self-approval is
  asked, so the file alone is not enough: a direct push, a merge commit that
  brings back a revoked version, or another team's or an earlier service's
  file does not count. Not covered: a force-push of `main` back to before a
  revoke, which needs a bypass of the ruleset.
- **Revoke:** `forge approval exception revoke <repo>` opens the pull request
  that removes it; when it merges, the exception ends.
- `forge approval exception list`, the repo's page and `forge service get`
  show an exception, who granted it and since when.

### Crossplane

GitHub environment settings and the targets' `access` are Crossplane objects.
They are long-lived settings that should be reconciled if changed outside
Forge. An installation needs Crossplane, its GitHub provider, and provider
integrations and compositions for the targets it offers. The cloud resources,
scope boundaries, identities, roles and lifecycle rules are defined by those
integrations. The chart does not install Crossplane providers.

## Permissions

- Creating an infrastructure repo: as `forge create` (the team role on the
  owning team), and the team must be in the target's `teams`. A Rego rule
  checks the target, so it is tested like the others.
- Approving: a member of the owning team; admins as for every service.
- The GitHub App gains Actions: write (to start the production run) and the
  OIDC customization of its repos; a settings change on GitHub, with the
  owner's OK.

## Dashboard

- Create: the infrastructure templates show next to the others; picking one
  asks for a target (only the team's) and its environments instead of run
  options.
- The repo's page: target, tool, environments, the exception if any, the
  approvals (waiting, approved, rejected), a link to the repo's Actions. No
  deploy buttons.
- An "Approvals" list of what waits for the signed-in user's teams.
- Docs in forgegate: a page "Infrastructure repos" (for developers) and a
  section in the install guide (targets, the shared workflow, Crossplane, the
  exception's ruleset).

## Exit gate

On a real install: a developer creates an `opentofu` repo from the dashboard
for a target, with production only, merges a change, sees production wait in
Forge, cannot approve it themselves, a teammate approves, and it is applied.
A repo whose workflow is changed to skip the shared one cannot sign in to
production. A second repo of a one-person team gets the exception through a
pull request the platform team merges, and its developer approves their own
production run. Every step is in the audit log. Changing an environment by
hand in GitHub is put back.

**Passed (2026-11-05).** The owner ran the exit gate on their own install
with a new `opentofu` repo, and its audit log holds every step: the
`service.create` from the template; production runs requested by the
repo's workflow (`approval.request`), approved in Forge by a teammate
(`approval.approve`) and then claimed and applied (`approval.claim`); the
team-of-one exception requested through a pull request to the deploy repo
(`approval.exception.request`); and, once it was granted, the developer
approving their own production runs (`approval.approve` with
`self_approved`). Pull requests in an OpenTofu and a Bicep repo also passed
their plan and what-if checks and the required `infrastructure / pr-checks`
aggregate, and since 0.44.0 an active exception also drops the required
reviews on the repo's pull requests.

## Known limits (accepted)

From the independent reviews of the parts, kept as they are for now:

- A pull request's preview runs its code with staging's identity (staging is
  where unreviewed code may run). Production is reached only after an
  approval.
- A claimed approval is used once: when a production apply fails, the fix is
  a new commit with its own approval.
- The self-approval check compares GitHub logins, which a user can rename;
  user IDs would be firmer.
- An exception brought back by force-pushing `main` to before its revoke is
  not caught (it needs a bypass of the deploy repo's ruleset).
- Members of a child team of an admin team count as that team's members for
  granting exceptions (GitHub's team membership), and each exception check
  makes a few GitHub calls, without a cache.
- Provider and module versions are whatever the repo pins: commit
  `.terraform.lock.hcl` to pin providers.
- When a bot pushes the commit or re-runs the job (the scaffold's first
  commit, a merge queue, an auto-merge, a re-run through the API), the
  approval names the bot, so the person behind it may approve it. Accepted
  by the owner, 2026-11-04.
- GitHub's OIDC tokens carry no `triggering_actor`: Forge reads who started
  the attempt from GitHub, and refuses when it cannot (since v0.40.1).
- The very first push of a new repo runs before Forge has set
  `FORGE_ENVIRONMENTS`, and fails; re-run it once the setup is Ready.

## Left to the builder

- The Crossplane kinds' exact fields and API versions (check the installed
  providers' CRDs), and how a target declares the kinds its `access` uses.
- The proto shape, the dashboard's layout, the shared workflow's details
  (action versions, the plan comment), and where the shared workflow's repo is.

## Names (confirmed by the owner, 2026-11-03)

`--target` and `--environments` on `forge create`; `forge approval
approve | reject | list`; `forge approval exception request | list | revoke`;
the dashboard page "Approvals". `target:` already means something else in
`template.yaml` (the repo a file template writes to); users do not see that
field.

## Order

1. The API: targets, `type: infrastructure`, `infrastructure` in
   `forge.yaml`, the deploy repo writing, the ApplicationSet, the approvals,
   with harness tests. Released and live before step 2: an older forge-api
   rejects the new `forge.yaml` field.
2. The shared workflow and the templates `bicep` and `opentofu`.
3. The exception, the dashboard, the docs.
4. Configure provider integrations, targets, GitHub permissions and repository
   protection for each installation.
