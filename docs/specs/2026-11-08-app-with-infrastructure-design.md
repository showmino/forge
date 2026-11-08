# A web app and its infrastructure in one repo

Status: approved by the owner on 2026-11-08, with the recommended answers to
the open questions below. It extends phase 12
(`2026-11-03-phase-12-infrastructure-repos-design.md`); it is not a new phase.

## Why

A team that runs a web app on Forge and needs cloud resources for it (a
storage account, a Key Vault, a database in Azure) needs two repos today: a
service made from `nextjs`, and an infrastructure repo made from `opentofu` or
`bicep`. They want one repo: the app and the resources it needs, one owner,
one history, one place to review.

Forge refuses this today. A `forge.yaml` cannot have both `run` and
`infrastructure` (`internal/manifest/infrastructure.go`), an infrastructure
template cannot offer run options, and a service with `infrastructure` has no
Forge deploys or previews (`internal/server/source.go`).

## What the owner chose (2026-11-08)

- The app runs **on the Forge cluster**, as an ordinary service. The
  infrastructure code is applied by the repo's GitHub Actions, as in phase 12.
- **One template** for Bicep and OpenTofu: a parameter picks the tool at create.
- The app is **Next.js**, the same app as the `nextjs` template.

## Goals

- A new template in `forge-templates`, `nextjs-infra`, makes one
  repo: the Next.js app at the root, the infrastructure code in
  `infrastructure/`.
- The app gets what every service gets: deploys to staging from CI, previews,
  promote, rollback, versions and the run options of the create form.
- The infrastructure code gets what every infrastructure repo gets: a target,
  its GitHub environments, plan or what-if checks required on every pull
  request, staging applied on merge, production approved in Forge by the team.
- Deleting the service takes down both: its deploys and its infrastructure
  folder in the deploy repo.

## Non-goals

- **Giving the app the infrastructure's outputs.** The app reads them from
  `run.env` or `run.secrets`, which a person sets after the first apply.
  Connecting the parts of an application is phase 13 (application builder).
- **Infrastructure for previews.** A preview runs the app only. If the app
  needs a resource, it uses staging's.
- **Ordering the two.** On create, the app's first deploy and the first
  infrastructure apply run side by side. The starter app uses no cloud
  resource, so neither waits for the other.
- Turning an existing service or infrastructure repo into a combined one.

## Design

No new template type. A combined repo is an infrastructure repo whose
`forge.yaml` also has `run`. Everything of phase 12 already keys on
`service.infrastructure` (create, approvals, repository rules, exceptions,
teardown, reconcile), and every deploy path already keys on `run`; a new type
would copy both.

### forge.yaml

```yaml
apiVersion: forge.dev/v1
service:
  name: shop
  team: web
  repo: https://github.com/my-org/shop
run:
  port: 3000
  health: /api/healthz
infrastructure:
  tool: opentofu
```

`checkInfrastructure` allows `run`. It still refuses `chart` (a chart is
another way of running, with its own ApplicationSet) and `resources` (the
repo's own code makes its cloud resources; Forge's resource runner would be a
second way).

### Forge (this repo)

| Where | Today | Change |
| --- | --- | --- |
| `internal/manifest/infrastructure.go` | `run` refused with `infrastructure` | allowed |
| `internal/template/template.go` (`Validate`) | `options` refused on an infrastructure template | allowed |
| `internal/server/create.go` | run options refused for an infrastructure template | applied as for a service, when the template lists `options` |
| `internal/server/source.go` | every deploy and preview of a service with `infrastructure` refused | refused only when the `forge.yaml` it reads has no `run`; `ErrDeploysItself` says so |
| `InfrastructureRepo` (proto) | tool, target, environments | adds `bool runs = 4`, output only, set at create from `forge.yaml`, so clients know to show deploys too |
| `forge service get` | an infrastructure row | an infrastructure row and the service's run rows |
| `forge template lint` | renders each template once, with defaults | also renders every other value of each enum parameter, so the other tool is checked too (chosen over a hidden `--param` flag while building: no CI change, nothing to learn) |

Unchanged, checked:

- **CI login.** A CI token is matched by its `repository` claim
  (`ci_auth.go`), not by the OIDC subject that phase 12 customizes, so the
  app's `ci.yml` still deploys to staging. Approvals still accept only the
  shared workflow (`JobWorkflowRef`).
- **Repository rules.** The infrastructure check stays required on `main`.
- **Teardown.** It already undeploys every environment and then removes the
  infrastructure folder (`service_teardown.go`). A test covers the combined
  case.

### The shared workflow (forge-templates)

`deploy.yml` runs the shared workflow on every push to `main`. In a combined
repo, every merge that changes only the app would apply staging's
infrastructure again and ask the team to approve a production run that
changes nothing.

Change: on a push, the `staging` and `request` jobs run only when the push
changed `infrastructure/` or `.github/workflows/deploy.yml` (a diff of
`before..sha`; a push without a `before` counts as changed). Pull request
checks run as today, on every pull request, because the required check must
report.

Effects:

- This applies to every infrastructure repo, not only combined ones: a merge
  that changes only the README no longer asks for a production approval.
- A failed production apply is retried by a commit that changes
  `infrastructure/`, not by any commit.
- Production's cloud access trusts this workflow, so the change gets one
  independent review before it merges (the rule in `AGENTS.md`).

### The template (forge-templates)

```
nextjs-infra/
  template.yaml                  type: infrastructure; parameters port, iac; options as nextjs
  skeleton/
    app/ lib/ public/ package.json package-lock.json Dockerfile ...   the nextjs app, as is
    forge.yaml                   run + infrastructure.tool: {% .Params.iac %}
    infrastructure/terraform/    rendered only when iac is opentofu
    infrastructure/bicep/        rendered only when iac is bicep
    .github/workflows/ci.yml     the app: test, image, deploy to staging (as nextjs)
    .github/workflows/deploy.yml the infrastructure: the shared workflow, tool: {% .Params.iac %}
    docs/index.md docs/runbook.md
    README.md CODEOWNERS .gitignore .dockerignore
```

- `iac` is an `enum` of `opentofu` and `bicep`, default `opentofu`. The unused
  folder's name renders empty, which drops it (the template format already
  does this).
- The app files are copies of `nextjs`'s. The templates' lint CI checks they
  are identical, so the two cannot drift.
- Lint renders the template once per tool (every enum value), and CI builds
  and tests the app.
- A new template is a minor version of `forge-templates`. It is tagged only
  after the forge-api that accepts it is live (an older API refuses `options`
  on an infrastructure template).

### Dashboard (forge-web)

- Create: an infrastructure template that lists `options` shows the target
  and environments **and** the run options.
- Service page: when `infrastructure.runs` is set, it shows the
  infrastructure section and approvals **and** the deploys, environments and
  previews of a service.
- After the proto change is merged here: `sh scripts/sync-proto.sh ../forge`.

### Documentation (forgegate)

The infrastructure repos page gets a section on a web app with its
infrastructure; the templates page lists the new template. No command or
visible flag changes.

## Order of work

1. forge: the changes above, test first (harness: create a combined repo,
   deploy it to staging, preview it, delete it). One independent review: it
   changes what can be deployed.
2. forge-templates: the shared workflow change (independent review) and the
   template. Its lint CI builds forge from `main`, so step 1 merges first.
3. forge-web and forgegate, alongside.
4. A forge release. Rolling it out, and creating a real repo to try the
   template, wait for the owner's OK.

## Exit check

A writer creates a repo from the dashboard, once with OpenTofu and once with
Bicep. The app runs in staging and is promoted to production. A pull request
that changes `infrastructure/` is planned; its merge applies staging and waits
for the team's approval of production. A merge that changes only the app
deploys it and asks for no approval. Deleting the service removes both.

## Decided with the go-ahead (2026-11-08)

1. **Name** of the template: `nextjs-infra`.
2. **Default tool**: OpenTofu.
3. **Infrastructure jobs are skipped on pushes that do not change it**, for
   every infrastructure repo.
