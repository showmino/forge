# A service's environments: staging, production or both

Status: proposed, waiting for the owner's go-ahead.

## Why

An infrastructure repo is made for staging, production or both
(`--environments`, phase 12). Every other service always has both: CI deploys
`main` to staging and a person promotes it. The owner wants the same choice
for every service: a tool that only ever runs in production, or a test
service that never leaves staging.

## What the owner chose (2026-11-09)

- **Production only: a person deploys.** CI builds and pushes the image, and
  deploys nothing. A team member runs `forge deploy start <svc> --env
  production` (or the dashboard's Deploy). CI never touches production: the
  rule "production stays with people" holds.
- **Every way of creating a service** offers the choice: a template, `--image`,
  `--chart`, and connecting an existing repo.
- **The owning team can change it later.**

## Goals

- `forge create ... --environments staging|production|staging,production`
  for every kind of service; the default stays both.
- The service remembers its environments in the catalog. Forge refuses a
  deploy, a promote or a rollback in an environment the service does not
  have, with a message that says what to do.
- What a template, an image or a chart service runs is the same as today in
  the environments it has.
- The dashboard's create form offers the choice for every kind of service,
  and a service's page shows only its environments.

## Non-goals

- CI deploying to production. Unchanged: a CI token deploys only to staging.
- Previews. A service of any environments can still have previews.
- More environments than staging and production, or other names.
- Changing the environments of an infrastructure repo after create (its
  GitHub environments and cloud access are made per environment; unchanged).

## Design

### The catalog

`Service` gets `repeated string environments = 14`, in deploy order. Empty
means both, so every existing service keeps what it has and no data
migrates. Forge stores both explicitly for a new service with both.

`UpdateService` accepts the path `environments` (the team's role, as other
service settings): `staging`, `production` or both. It refuses:

- dropping an environment that still has a deploy: "production still runs
  v0.3.1: take it down first with `forge deploy remove shop --env
  production`";
- any change for an infrastructure repo (its environments are its GitHub
  environments and cloud access, set at create).

### Deploys

| Call | Rule |
| --- | --- |
| `StartDeploy` | `env` must be one of the service's. A CI token still only deploys to staging, so a production-only service's CI gets a clear refusal if it tries |
| `PromoteDeploy` | needs both. For a single environment: "shop has production only: deploy to it with `forge deploy start shop --env production`" |
| `RollbackDeploy` | `env` must be one of the service's |
| `CreateEnvironment` (preview) | unchanged |
| new `DeleteDeployment` (`forge deploy remove <svc> --env <env>`) | takes one environment's deploy down (the teardown's `undeploy`), as an operation. Write role: the team. Needed so the team can drop an environment; also useful on its own |

Versions are unchanged: a production deploy cuts the final version
(`v0.3.1`) whether it comes from a promote or a direct deploy; a staging
deploy, a release candidate.

### Create

`CreateServiceRequest.environments` (today only for infrastructure templates)
is accepted for every kind: template, image, chart, connect. `target` stays
only for infrastructure templates. For an infrastructure template the list is
both its GitHub environments and, with an app, the app's.

- `--image` and `--chart` deploy right after create (`--deploy`): to staging,
  or to production for a production-only service. A person runs the create,
  so this keeps the rule.
- **Templates** get two new values, `.Service.Staging` and
  `.Service.Production` (bools), so a skeleton can leave out what does not
  apply. The `nextjs`, `go-grpc` and `nextjs-infra` CI keep the `deploy` job
  only when the service has staging. Lint renders with both true.
- **Connect** (`forge service connect`) writes the build workflow without its
  `deploy` job for a production-only service.

A template that uses `.Service.Staging` needs a forge-api that sets it: the
templates are tagged only after that API is live.

### CLI

- `forge create --environments` for every kind (the flag exists; its help
  drops "with an infrastructure template").
- `forge service update <svc> --environments production`.
- `forge deploy remove <svc> --env <env>`.
- `forge service get` shows `environments`; `forge deploy status` lists only
  the service's environments.

### Dashboard (forge-web)

- Create: the environment checkboxes the infrastructure form has, for every
  kind of service (both ticked by default).
- Service page: only the service's environments; Promote only with both;
  Deploy goes to the one environment a single-environment service has.
- Settings: change the environments (team); Remove a deploy in the danger
  zone.

### Documentation (forgegate)

Create a service, Deploy and Release (production-only and staging-only
services, `forge deploy remove`), forge.yaml unchanged, the command
reference.

## Order of work

1. forge: catalog field, deploy rules, `DeleteDeployment`, create, template
   values, connect workflow, CLI; tests first. One independent review: it
   changes what may deploy to production.
2. forge-templates: CI deploy job by `.Service.Staging`; tagged after the API
   is live.
3. forge-web (after the proto sync) and forgegate, alongside.
4. A forge release; the rollout waits for the owner's OK.

## Exit check

A writer creates a production-only service from `nextjs` in the dashboard:
its CI builds the image and deploys nothing; they deploy it to production
from the service's page, and it gets a final version. A staging-only service
from `--image` runs in staging and cannot be promoted. The team adds
production to it, promotes, then drops staging after `forge deploy remove`.
