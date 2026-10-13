# Service options: replicas, resources, env, 1Password secrets, cron jobs, autoscaling

Status: approved 2026-10-13; milestone 1 in review. The open points below
were taken as recommended: no vault is allowed until `--secret-vaults` is
set, and autoscaling is on CPU. In the chart the plain variables are `vars`
(`env` is the environment's name there).

## What the owner asked for

The create form should offer more than the defaults, per template: how many
replicas, 1Password secrets, cron jobs and the like. The options live in the
service's repo (`forge.yaml`) so they can be changed later, in the repo or from
the CLI. The owner's earlier shared chart (`platform-config/charts/app`) is the
reference for what to offer: not every value is needed, but users get the
options.

Decided with the owner on 2026-10-13:

| Question | Decision |
| --- | --- |
| Where the options live | `forge.yaml`, under `run:`; changed later in the repo or with the CLI |
| 1Password | Named items become Kubernetes Secrets given to the app as env vars, only from vaults the platform allows |
| Cron jobs | The service's own image with another command, on a schedule, in staging and production |
| Replicas | One number for production; staging stays at 1 |

## forge.yaml

Every field is optional; without them a service deploys as today.

```yaml
run:
  port: 3000
  health: /api/healthz
  replicas: 3                 # production; staging is always 1. 1 to 10
  resources:                  # per container; defaults are the LimitRange's
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: 500m, memory: 512Mi}
  env:                        # plain values, the same in both environments
    LOG_LEVEL: info
  secrets:                    # 1Password items, synced into the namespace
    - name: stripe
      item: vaults/Apps/items/stripe
      env:                    # env var: field of the item
        STRIPE_KEY: credential
  jobs:                       # Kubernetes CronJobs, same image, env and secrets
    - name: cleanup
      schedule: "0 3 * * *"
      command: ["node", "scripts/cleanup.js"]
  autoscaling:                # production only; KEDA, on request rate
    max: 6
```

Rules (forge-api checks them on every deploy, and `forge service options
check` in the repo before a push):

- `replicas` 1 to 10. With `autoscaling`, it is the minimum and `max` the most.
- `resources` must fit the namespace's ResourceQuota (requests 2 CPU / 2Gi,
  limits 4 CPU / 4Gi, 15 pods) in each environment, counting the replicas (or
  the autoscaling maximum), one more pod during a rollout, one per job and
  the database's instances. A request not set is the limit when that is set,
  as Kubernetes does; a request above its limit is refused.
- `env` names are `[A-Z_][A-Z0-9_]*`; `PORT` and `DATABASE_URL` are Forge's.
- `secrets`: at most 10; `item` must be `vaults/<vault>/items/<item>` in a vault
  forge-api allows (`--secret-vaults`); `name` is a DNS label of at most 40
  characters, the Secret is `<name>-1password` (a suffix no other Secret in
  the namespace has: not the pull secret, not CloudNativePG's `<db>-*`). An
  env var named twice is refused.
- `jobs`: at most 5; `<service>-<name>` at most 52 characters; `schedule` is
  a five-field cron expression or a macro; `command` is a list. A job runs
  with `concurrencyPolicy: Forbid`, at most an hour, the app's security
  context, env and secrets, and keeps the last 3 runs.
- `autoscaling.max` up to 10, more than `replicas`.

Like `run.host` before it, a field reaches services only after a forge-api
that knows it is live: an older one rejects the unknown field.

## The deploy chart

`deploy/argocd/chart` gets what `platform-config/charts/app` had, in Forge's
shape: `replicas` from the values (already there), `resources`, `env`,
`secrets` (one `OnePasswordItem` per item, `secretKeyRef` per env var),
`cronjob.yaml`, and `scaledobject.yaml` (KEDA is on the cluster). An
autoscaled Deployment keeps `replicas` (the minimum) and is annotated
`forge.dev/autoscaled`; the ApplicationSet ignores `spec.replicas` on those
only (`RespectIgnoreDifferences`), so Argo CD neither fights KEDA nor drops
production to one pod when autoscaling is switched on. The deploy
values carry them; the chart revision moves, so every deployed service is
re-rendered once (no change for services without options).

Staging gets the same 1Password items as production.

Left out of the reference chart: Istio (Forge uses its Gateway), the
ConfigMap (env covers it), the default `mino-ghcr` 1Password item (Forge has
its own pull secret), annotations.

## Security: which vaults

All 30 1Password items on the cluster today come from the vault `Kube`, which
also holds the platform's own secrets (the GitHub App key, the registry
credential, Dex's OAuth secret). Allowing `Kube` would let any writer put the
GitHub App key into their service. So service secrets come from a vault of
their own (proposed name `Apps`), which the 1Password operator must be able to
read: a setting in 1Password for the owner. `--secret-vaults` lists the
allowed vaults; empty turns `secrets` off.

## The create form

Templates already ask their own questions (`template.yaml` parameters). A
template now also says which of the platform options its form offers:

```yaml
options: [replicas, resources, env, secrets, jobs, autoscaling]
```

The form shows each offered option in its own section, collapsed and with
the default filled in, and Forge writes the answers into the new repo's
`forge.yaml`. A template without `options` gets the form it has today. The
CLI's `forge create` takes them as flags (`--replicas 3`, `--secret
stripe=vaults/Apps/items/stripe:STRIPE_KEY=credential`, `--job
cleanup="0 3 * * *":node,scripts/cleanup.js`).

Changing them later: edit `forge.yaml` and push (CI deploys), or `forge
service options set replicas=3` in the repo, which edits `forge.yaml` for you.

## Milestones

1. **forge.yaml and the deploy**: manifest fields and rules, deploy values,
   the chart (resources, env, secrets, cron jobs, autoscaling), `forge service
   options check`, the vault allowlist. Rollout: a new forge-api with `--secret-vaults`, then
   the chart revision.
2. **Create**: `options` in `template.yaml`, the form sections, `forge create`
   flags, `forge service options set`; the two templates offer them.
3. **Dashboard**: a service's options shown on its page (read from the last
   deploy), with a link to edit `forge.yaml` on GitHub.

## Open, for the owner

- The vault for service secrets (`Apps`?), and giving the 1Password operator
  read access to it.
- Autoscaling's metric: the reference chart used Prometheus at
  `10.150.1.203:9090` and `http_requests_total`; that needs the app to export
  it. CPU instead needs no app support.
