# Phase 12 · Infrastructure repos

Proposed on 2026-11-03, from a conversation with the owner. It replaces the
phase 12 of `2026-10-29-deploy-sources-design.md` (the app catalog), which
moves to phase 14. The owner gave the go-ahead and confirmed the names the
same day.

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
  `production` or both. Each environment is its own cloud account (an Azure
  subscription, an AWS account, a GCP project) with its own identity.
- A production run waits for an approval in Forge by a member of the team that
  owns the repo, not the person who started the run.
- A team of one can get an exception: they may approve their own production
  runs. Only the platform team can grant it.
- The target's values are set as environment secrets and variables, and the
  cloud side (who may sign in as what) is set up for this repo.
- It works on every GitHub plan, and Forge stays cloud-neutral: Azure, AWS and
  GCP differ only in what the platform team writes into a target.

## Non-goals

- Forge running the infrastructure code. The repo's GitHub Actions do that;
  Forge's own OpenTofu runner (phase 5) is unrelated.
- Forge making subscriptions, accounts, identities or state storage. At
  showmino the landing zone makes them; elsewhere, whatever the organization
  uses.
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

GitHub's required reviewers on an environment (and "prevent self-review") work
in private repos only on GitHub Enterprise; on Free, Pro and Team they are for
public repos only. Deployment branches, environment secrets and environment
variables do work in private repos on Pro and Team. showmino is on Team, and
Forge makes every repo private. So Forge does the approval itself, which also
makes it the same on every plan, in the audit log, and the start of issue
#52's deploy approvals.

## How it works

### Targets

A target is a set of cloud accounts, one per environment, that infrastructure
code deploys to. The platform team sets them up in the chart (value
`infrastructure.targets`, read by forge-api from a mounted file):

```yaml
infrastructure:
  namespace: forge-infrastructure     # the Crossplane objects and the targets' Secrets
  providerConfig: default-github      # the install's Crossplane GitHub ProviderConfig
  workflow: my-org/forge-workflows/.github/workflows/infrastructure.yml@refs/heads/main
  targets:
    - name: azure
      title: Azure
      teams: []                       # teams that may use it; empty: every team
      environments:
        production:                   # an environment the target does not list cannot be chosen
          secret: target-azure-production   # a Secret in the namespace above
          githubSecrets:              # environment secret name: key in that Secret
            AZURE_TENANT_ID: tenantId
            AZURE_SUBSCRIPTION_ID: subscriptionId
            AZURE_CLIENT_ID: clientId
          githubVariables:            # environment variable name: value (not secret)
            TF_STATE_RESOURCE_GROUP: rg-state
            TF_STATE_STORAGE_ACCOUNT: ststate
          maxRepos: 20
          access: |                   # Crossplane objects, one set per repo
            apiVersion: managedidentity.azure.m.upbound.io/v1beta1
            kind: FederatedIdentityCredential
            metadata:
              name: {{ .Repo }}-production
            spec:
              forProvider:
                audience: [api://AzureADTokenExchange]
                issuer: https://token.actions.githubusercontent.com
                subject: {{ .Subject }}
                parentId: /subscriptions/.../userAssignedIdentities/...
                resourceGroupName: ...
        staging: {...}                # same shape, its own account and identity
```

- The Secret holds the environment's secret values. Who makes it is up to the
  install (at showmino: from 1Password now, later from the landing zone).
  Values such as a subscription ID go to GitHub as secrets by the owner's
  choice, even if they are not secret on their own. Variables are plain values
  in the target: the Crossplane kind for an environment variable has no way to
  read a Secret.
- `access` is a Go template with `.Org`, `.Repo`, `.Environment`, `.Team` and
  `.Subject` (the OIDC subject below). For AWS it would be a role's trust; for
  GCP a workload identity binding. Forge only fills it in and writes it.
- An Azure managed identity holds at most 20 federated credentials. Each
  environment of a target has its own identity and one credential per repo,
  so `maxRepos` (20 for Azure) is counted per environment; past it, a create
  is refused with a message that says to add a target.

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
  platform team owns, and holds the jobs:
  - on a pull request: `bicep build` or `tofu validate`, and, when the repo
    has `staging`, a preview against staging (`what-if` or `tofu plan`),
    commented on the pull request;
  - on a merge to `main`: deploy to `staging` (when the repo has it), then
    ask Forge for production's approval, and deploy to `production` once
    Forge says it is approved for that commit.
  - Every job signs in with `azure/login` from the environment's secrets and
    GitHub's OIDC token. The `opentofu` template runs `tofu`; one input
    switches it to `terraform`.
- One parameter file per tool. What differs between environments is read
  from the GitHub environment (`readEnvironmentVariable()` in
  `main.bicepparam`, `TF_VAR_...` for OpenTofu).
- OpenTofu state: the `azurerm` backend in the environment's own storage
  account (the landing zone makes it in the same subscription), container
  `tfstate`, key `<repo>.tfstate`, signed in with OIDC (`use_oidc`,
  `use_azuread_auth`). The identity needs **Storage Blob Data Contributor** on
  that account; Owner alone does not give data access.
- The first templates are for Azure. Templates for other clouds are someone
  else's to write; nothing in Forge stops them.

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
6. The operation finishes when the Crossplane objects are Ready, and says
   which one is not and why.

Deleting the repo's service deletes `infrastructure/<repo>/`, so the
environments and the cloud sign-in go with it. The delete warns that what the
repo deployed stays in the cloud account.

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

The settled decision "OpenTofu jobs, not Crossplane" gets a second exception
(the first is DNS records): GitHub environment settings and the targets'
`access` are Crossplane objects. Why: they are long-lived settings that
should be put back when someone changes them by hand, and every cloud has a
Crossplane provider, so Forge stays cloud-neutral.

The install needs Crossplane, `provider-upjet-github` with a ProviderConfig
for the org (its own credential, not Forge's App key), and a provider for each
cloud its targets use (for Azure, `provider-azure-managedidentity`, whose
identity may only edit the targets' identities: Managed Identity Contributor
on them). The chart does not install them; its README and forgegate say what
is needed.

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
4. On showmino's install: the Azure provider, the production target's Secret,
   the deploy repo's ruleset, the App's new permissions (each with the owner's
   OK; details in the operator's runbook, not here).
