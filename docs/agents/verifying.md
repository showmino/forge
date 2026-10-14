# Verifying Forge end to end

Run from the repo root. `$GO` means `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`:
on the owner's machine (`srv-ovo`) Go is not on the system PATH. On a CI
runner or any machine with Go on its PATH, leave `$GO` out.

Sections 2 to 4 need the owner's machine: the cluster and the LAN are not
reachable from a cloud runner. Section 1 works anywhere.

## 1. Build, test, lint (CI parity)

```sh
$GO make build test lint
```

CI resolves Go from `go 1.27` in go.mod and pins golangci-lint **v2.14.0**
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`) and
govulncheck v1.8.0. Use the same versions locally; an older golangci-lint fails
with `export data version ... greater than maximum supported` on Go 1.27.

## 2. Local login against Dex

```sh
make dex                                   # Dex on :5556 (docker compose)
bin/forge-api --oidc-issuer http://localhost:5556/dex   # in the background
unset FORGE_TOKEN
bin/forge context set local --api http://localhost:8080 --use
python3 -I docs/agents/scripts/approve-device-login.py bin/forge
```

Then check: `forge auth status` (permissions `services.read, services.write`),
`forge service register payments --team billing --owner a@b.c` then `service list`,
`forge doctor` (all ok, no version warning), `FORGE_TOKEN=nope forge service list`
exits 4 with `hint: run 'forge auth login'`.

Whole CLI against a live API (every noun and verb, output formats, exit codes,
forge.yaml defaults, logout/login): run
`bash docs/agents/scripts/cli-smoke.sh bin/forge <context> > smoke.log; tail -1 smoke.log`
and read every `FAIL` block. When behaviour changes on purpose, change the case's expected exit.

Refresh: set `expiry` in `~/.config/forge/credentials.json` to a past time, run
`forge auth token`; the token must change and the file stay mode 0600.

## 3. The cluster (k3s at 10.150.0.10-15)

**This cluster is production**, and it is shared with other real workloads
(argocd, backstage, finance...). Touch only what Forge owns. To change what
runs, follow "Rolling out to the cluster" below, never the set-up commands in
this section.

Facts that shape the deploy:

| Fact | Consequence |
| --- | --- |
| No Traefik, no IngressClass; other apps use Istio `main-gateway` on `*.showmino.com` (public via Cloudflare) | Forge is rendered with `render.sh --lb`: Dex and the API have their own LoadBalancer addresses, and the Cloudflare tunnel reaches them by name (see "Forge's own public names") |
| MetalLB v0.14.5, pool 10.150.1.200-250 | Forge uses **10.150.1.220** (Dex) and **10.150.1.221** (API); annotation is `metallb.universe.tf/loadBalancerIPs` (`metallb.io/` needs 0.15+) |
| Dev machine `srv-ovo` is not a node, no SSH to nodes | No `k3s ctr images import`; push to `ghcr.io/showmino/forge-api:<short-sha>` (private package, docker is logged in to ghcr) |
| Images pull with the `mino-ghcr` dockerconfigjson secret (in `docs` ns) | Copy it into `forge` and attach it to the default ServiceAccount |
| Istio 1.30 with Gateway API v1.6.1 (GatewayClass `istio`), no `discoverySelectors`, no PeerAuthentication | Previews use their own Gateway `forge/forge-previews` on **10.150.1.222** (`*.preview.10.150.1.222.nip.io`), LAN only |

**First set-up, kept as history and for rebuilding from nothing. It is not a
rollout:** it applies without a diff and builds an image by hand. Images now
come from the release workflow (`vX.Y.Z`), and every change goes through
"Rolling out to the cluster". Do not run this block against the live cluster.

```sh
IMG=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
docker build --build-arg COMMIT=$(git rev-parse HEAD) -t $IMG . && docker push $IMG
# GitHub App mino-forge (templates, forge create); the 1Password operator syncs its
# key from vault Kube, item forge-github-app, field github_app_private_key
# FORGE_PREVIEW_IP: preview environments (forge env) behind the Gateway on that IP
FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app FORGE_PREVIEW_IP=10.150.1.222 FORGE_API_IMAGE=$IMG \
  sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
# key names only, never the values: must list github_app_private_key
kubectl -n forge get secret forge-github-app -o jsonpath='{.data}' | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin)))'
# once: pull secret (copy without printing it)
kubectl -n docs get secret mino-ghcr -o json | python3 -I -c 'import json,sys;s=json.load(sys.stdin);print(json.dumps({"apiVersion":"v1","kind":"Secret","type":s["type"],"metadata":{"name":"mino-ghcr","namespace":"forge"},"data":s["data"]}))' | kubectl apply -f -
kubectl -n forge patch serviceaccount default -p '{"imagePullSecrets":[{"name":"mino-ghcr"}]}'
kubectl -n forge rollout restart deploy/forge-api && kubectl -n forge rollout status deploy/forge-api
kubectl -n forge get svc        # EXTERNAL-IP must be .220 / .221
bin/forge context set k3s --api http://forge.10.150.1.221.nip.io --use
python3 -I docs/agents/scripts/approve-device-login.py bin/forge
bin/forge doctor
```

forge-api logs `forge-api listening ... issuer=http://dex.10.150.1.220.nip.io/dex`
only after OIDC discovery from inside the pod succeeded. With the App, `bin/forge
template list` must show `go-grpc`, and `bash docs/agents/scripts/cli-smoke.sh
bin/forge k3s` must report `0 failed`. (Until the groundwork a rollout emptied
the catalog; it is kept in PostgreSQL, `forge-db`, now. Operations are still in
memory and are gone after a restart of forge-api.)

Previews (rendered with `FORGE_PREVIEW_IP`):

```sh
kubectl -n forge wait gateway/forge-previews --for=condition=Programmed --timeout=120s
kubectl -n forge get gateway forge-previews -o jsonpath='{.status.addresses[0].value}'   # 10.150.1.222
# forge-api's permissions: yes yes yes yes no yes no, then with deploys on: yes no
for v in "create namespaces" "delete namespaces" "create deployments -n x" "create httproutes.gateway.networking.k8s.io -n x" \
         "get secrets -n kube-system" "get secrets/mino-ghcr -n forge" "delete deployments -n x" \
         "get applications.argoproj.io -n argocd" "patch applications.argoproj.io -n argocd"; do
  printf '%-55s %s\n' "$v" "$(kubectl auth can-i $v --as=system:serviceaccount:forge:forge-api)"; done
```

The Phase 3 exit gate (2026-10-09, 4m37s): `time (bin/forge create gate-<x> --template go-grpc --team platform && bin/forge env create gate-<x> --service gate-<x>)`, then `/healthz` and Greet at `http://gate-<x>.preview.10.150.1.222.nip.io` (curl is blocked here; use `python3 -I` with urllib).

| Symptom | Cause |
| --- | --- |
| `ErrImagePull ... 401 Unauthorized` | Pod predates the pull-secret patch; restart the deployment |
| `CreateContainerConfigError: image has non-numeric user (nonroot)` | `runAsUser: 65532` missing from the pod securityContext |
| Service got a random pool IP | Wrong MetalLB annotation, or an existing allocation: delete the Service and re-apply |
| forge-api logs "templates are not configured" | Rendered without `FORGE_GITHUB_APP_ID` |
| forge-api pod stuck in `ContainerCreating` (secret not found) | The 1Password item or its field is missing; check `kubectl -n forge describe onepassworditem forge-github-app` |
| forge-api `ErrImagePull` after enabling previews | The `forge-api` ServiceAccount lacks the pull secret (`FORGE_PREVIEW_PULL_SECRET`, default `mino-ghcr`) |
| An env stays in `image` with "registry: ..." | The pull secret's login is wrong, or lacks read on the org's packages |
| An env is READY but its URL answers 404 | The HTTPRoute was not accepted: check `kubectl -n preview-<x> describe httproute` and the Gateway's `allowedRoutes` |
| Every command warns "older than ... 0.1.0" | Go pseudo-version not treated as dev (fixed in `internal/version`) |

**Tearing Forge down removes the platform from the cluster, with its database.
It is the owner's decision alone; never run it to "clean up".** For the
record: previews first (the reaper dies with forge-api, and the ClusterRole is
cluster-scoped, so deleting namespace `forge` leaves both behind). With deploys on,
do the Deploys teardown (below) first, otherwise services lose their routes:

```sh
kubectl delete ns -l app.kubernetes.io/managed-by=forge,forge.dev/environment
kubectl delete ns forge
kubectl delete clusterrole,clusterrolebinding forge-api-previews
```

## The GitHub App

forge-api acts on GitHub as the App `mino-forge` (App ID 5239131, owned by
`showmino`, installed on the organization with access to all repositories,
installation ID 169296924). It creates service repos, reads refs, files, CI
runs and tags, writes tags and releases, commits to `showmino/forge-deploys`,
and lists teams and memberships.

- **Permissions** (2026-10-11): repository Administration, Contents and
  Workflows read and write, Metadata read; organization Members read.
- **A new permission takes two steps, both by the owner:** set it on the App
  (organization Settings, Developer settings, GitHub Apps, mino-forge,
  Permissions & events), then accept the request on the installation.
- **The private key** is `mino-forge.<date>.private-key.pem` in the repo root
  on the owner's machine (ignored by git and by Docker) and a Kubernetes
  Secret in the `forge` namespace, synced from 1Password. Never print it.
- **To see what the App may do** without printing a token: a throwaway
  `cmd/zzprobe/main.go` in this module that builds `github.App` from the
  local key and calls `internal/github`, run with `go run`, then deleted.
  Read-only calls only.
- **The login is a different thing:** Dex signs people in through a GitHub
  *OAuth* app, whose callback URL must be `<login address>/dex/callback`.

## Deploys (Phase 4)

The Phase 4 exit gate (2026-10-09): `bin/forge create gate-<x> --template go-grpc --team platform`, then `gh run watch` its CI (test → image → deploy, about 5 minutes), `bin/forge deploy status gate-<x>`, `/healthz` and Greet (`<pkg>.v1.GreeterService/Greet`, JSON over HTTP) at `http://gate-<x>.staging.10.150.1.222.nip.io` with `python3 -I` urllib, then `bin/forge deploy promote gate-<x> --yes` and the same at `.production.`. The first pod pull fails until forge-api writes `forge-pull`; that is expected.

Render with deploys on (needs previews and a commit on origin/main for the chart; an unpushed commit makes every Application fail):

```sh
FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse origin/main) FORGE_PREVIEW_IP=<gateway-ip> FORGE_GITHUB_APP_ID=<app-id> ... \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

Once deploys are on, every re-render must include `FORGE_DEPLOYS=1` and the current `FORGE_DEPLOY_CHART_REVISION` (read it with `kubectl -n argocd get applicationset forge-services -o jsonpath='{.spec.template.spec.sources[0].targetRevision}'`); a render without them removes the staging and production routes. Changing the revision re-renders every deployed service.

Check the ApplicationSet generated its Applications:

```sh
kubectl -n argocd get applicationset forge-services -o jsonpath='{.status.conditions}'
```

Tear down a service: delete the `services/<svc>` directory in `forge-deploys`,
wait for the Applications to disappear, then delete the `<svc>-staging` and
`<svc>-production` namespaces by name (`kubectl delete ns <svc>-staging <svc>-production`);
the finalizer does not remove namespaces.

Remove the Argo objects and forge-api's access. This deletes every deployed
service: the ApplicationSet cascades to its Applications, and their finalizer
deletes the workloads. Delete the ApplicationSet, wait until
`kubectl -n argocd get app -l app.kubernetes.io/managed-by=forge` is empty, then
the rest:

```sh
kubectl -n argocd delete applicationset forge-services
kubectl -n argocd get app -l app.kubernetes.io/managed-by=forge   # repeat until empty
kubectl -n argocd delete appproject forge-services
kubectl -n argocd delete role,rolebinding forge-api-applications
kubectl delete ns -l app.kubernetes.io/managed-by=forge,forge.dev/deploy-env   # the finalizer does not remove namespaces
```

## The web dashboard (Phase 6)

### Rolling out to the cluster

The cluster is production, and every change to it needs the owner's OK
(`AGENTS.md`). The procedure, each time:

0. **The image must exist.** A change to forge-api is merged first, then
   released: push a `vX.Y.Z` tag on `main` (tagging a merged change needs no
   extra OK) and wait for the `release` workflow to finish. It publishes
   `ghcr.io/showmino/forge-api:vX.Y.Z`.
1. **Render what is live** with the command below and the image that runs now
   (`kubectl -n forge get deploy forge-api -o jsonpath='{.spec.template.spec.containers[0].image}'`),
   into a file in a temporary folder outside the repo (`mktemp -d`).
   `kubectl diff -f` on it must show nothing. If it shows something, the
   cluster has drifted or the command below is stale (the cluster is the
   truth: for example the chart revision is what the ApplicationSet
   `forge-services` in `argocd` says). Stop and find out which before changing
   anything, and correct this file.
2. **Render the new manifests** (the new image, or the new setting) and read
   `kubectl diff -f` on them. It should show what you mean to change and
   nothing else.
3. **Show the owner the diff and wait for the OK.**
4. `kubectl apply --dry-run=server -f`, then `kubectl apply -f`. Dex reads its
   config at start: `kubectl -n forge rollout restart deploy/dex` after a
   change to it. Wait for `rollout status`.
5. **Verify** with the checks in this file for what changed, and keep the
   render from step 1 as the rollback file (`kubectl apply -f` it to go back).
6. **Update this file** in a pull request: the image, the command below, and
   what was checked.

### Secrets Forge uses (names and keys only)

| Secret (namespace `forge`) | Keys | From |
| --- | --- | --- |
| `forge-github-app` | `github_app_private_key` | 1Password, item `vaults/Kube/items/forge-github-app` (`OnePasswordItem`) |
| `dex-github` | `client_id`, `client_secret` | 1Password, item `vaults/Kube/items/forge-dex-github`: the GitHub OAuth app Dex signs in with |
| `forge-db-app` | `uri`, `password` (and more) | written by CloudNativePG for the database `forge-db` |
| `mino-ghcr` | a dockerconfigjson | copied from the `docs` namespace: pulls from `ghcr.io` |

To check one, list its key names and nothing else:
`kubectl -n forge get secret <name> -o jsonpath='{.data}' | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin)))'`.

The render that is live (2026-10-11), to start every later rollout from; only `FORGE_API_IMAGE`, and the chart revision when the chart changes, move:

```sh
FORGE_GITHUB_APP_ID=5239131 FORGE_ONEPASSWORD_ITEM=vaults/Kube/items/forge-github-app \
FORGE_DEX_ONEPASSWORD_ITEM=vaults/Kube/items/forge-dex-github FORGE_PREVIEW_IP=10.150.1.222 FORGE_DEPLOYS=1 \
FORGE_DEPLOY_CHART_REVISION=2b84f5e1676aa92090e43737790655e6436cb070 \
FORGE_WEB_ORIGINS=https://forge.showmino.com,https://forge-staging.showmino.com \
FORGE_PUBLIC_DOMAINS=showmino.com=showmino-com,deogratias.cloud=deogratias-cloud \
FORGE_LOGIN_URL=https://forge-login.showmino.com \
FORGE_ARGOCD_URL=https://argocd.showmino.com \
FORGE_SECRET_VAULTS=Kube \
FORGE_SECRET_DENIED_ITEMS=forge-github-app,forge-dex-github,ghcr-secret,arc-runners,argocd-repo-kubernetes,cert-manager,Cloudflare,crossplane-cloudflare,crossplane-github-provider-creds,grafana-admin-credentials,renovate \
FORGE_API_IMAGE=ghcr.io/showmino/forge-api:v0.10.0 sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221
```

Forge's own public names (live since 2026-10-11, forge-api `v0.4.0`):

| What | Address | How it gets there |
| --- | --- | --- |
| Dashboard, production | `https://forge.showmino.com` | a Forge service (`forge-web` with `run.host: forge`): its DNS record comes with the deployment, the tunnel's wildcard sends it to the Gateway |
| Dashboard, staging | `https://forge-staging.showmino.com` | the same |
| API | `https://forge-api.showmino.com`, and still `http://forge.10.150.1.221.nip.io` on the LAN (CI and the org variable `FORGE_API_URL` use the LAN one) | a record and a tunnel route in `showmino/kubernetes` (#124) to the LoadBalancer `10.150.1.221` |
| Login (Dex) | `https://forge-login.showmino.com/dex`, the issuer | a record and a tunnel route in `showmino/kubernetes` to `10.150.1.220`; `FORGE_LOGIN_URL` in the render |

The GitHub OAuth app Dex signs in with has the callback `https://forge-login.showmino.com/dex/callback` (changed by the
owner at the switch; a login fails with a redirect_uri mismatch when the two disagree). The dashboard's image is built
with the API's HTTPS address from the repo variable `FORGE_WEB_API_URL` on `showmino/forge-web`. The two old LAN origins
left `FORGE_WEB_ORIGINS` the same evening: the API and the login refuse them now. Moving the issuer does not end Forge sessions: the CLI
stayed logged in through the switch. To check from outside: the dashboard's `Content-Security-Policy` names exactly the
API and the login in `connect-src`; a preflight to the API from a dashboard origin answers 204 with that origin, from any
other 403; Dex answers a form POST to `/dex/token` with `access-control-allow-origin` for a dashboard origin only (a
browser sends no preflight for that call, and Dex answers a made-up one with 403). Live operation logs through
Cloudflare are cut after 100 seconds without an event; the dashboard reconnects.

Public addresses (live since forge-api `v0.3.0`, chart `e7f0b8e`): services are reached at `https://<svc>.showmino.com`
and `https://<svc>-staging.showmino.com` (or `deogratias.cloud` with `run.domain`). The pieces: the tunnel's two
wildcard routes to `10.150.1.222` (`showmino/kubernetes` #123), the Gateway listeners `public-showmino-com` and
`public-deogratias-cloud`, and a `CloudflareRecordsApplication` per deployment in the service's namespace
(`kubectl -n <svc>-<env> get cloudflarerecordsapplications.kro.run`; its Record is in `crossplane-system`).
`forge-web` said `run.domain: none` from 17:56 UTC until it moved to `forge.showmino.com` the same evening; a rollback
of it to a deploy from that window puts it back on its LAN address. Proven with a throwaway `hello-forge` on
2026-10-11: staging and production on `showmino.com`, staging moved to `deogratias.cloud` by a commit (the old
record went, the operation named both addresses), HTTPS 200 through Cloudflare with the zone's certificate, and
the delete removed both records and namespaces. To check a name from outside without a cached "does not exist",
ask Cloudflare's DNS over HTTPS (`https://cloudflare-dns.com/dns-query?name=<host>&type=A`, `accept:
application/dns-json`): a resolver that was asked before the record existed, as Forge's own free-name check
does, may say NXDOMAIN for a while. A first production deploy showed one `ErrImagePull` before the pull secret
was in the new namespace, and recovered by itself.

Live on k3s since 2026-10-14: forge-api `v0.10.0` (file templates, forge#73; early `no_image`, forge#71 and #72; the render above, changed only in its image). Checked: no drift before (the cluster matched the v0.9.0 render), the diff was the image line alone, server dry run, no drift after; `forge version` says server 0.10.0; forge.showmino.com, forge-staging.showmino.com and deogratias.cloud answer 200; after forge-templates `v1.7.0` was tagged, `forge template list` shows `landing-zone` (the API reuses its tag list for a minute), and `forge create time-for-teams --template landing-zone ... --dry-run` plans `landingZones/Corp/lz-corp-time-for-teams.bicepparam` in showmino/landingzone (nothing created). Not checked: a real pull request in showmino/landingzone, and the dashboard's form against this API. Rollback file: the v0.9.0 render (the same command with `v0.9.0`).

Before it, since 2026-10-13: forge-api `v0.9.0` (run options in the create form, forge#66; the render above, changed only in its image). Checked: no drift before (the cluster matched the v0.8.1 render), the diff was the image line alone, no drift after; `forge version` says server 0.9.0; `forge template get nextjs` shows `run options none`; `forge create probe-x --template nextjs --replicas 2 --dry-run` is refused with "does not offer the run option replicas" (nothing created); forge.showmino.com and deogratias.cloud answer 200. The same day, before this rollout, an old setup block from this file was run by mistake and took the Gateway's public listeners away for about ten minutes; the v0.8.1 render was applied again (see AGENTS.md, "Mistakes").

Before it, since 2026-10-13: forge-api `v0.8.1` (run options, forge#63 and #64; the render above). It moved the chart revision to `2b84f5e` and added `FORGE_SECRET_VAULTS=Kube` with `FORGE_SECRET_DENIED_ITEMS` (the platform's own items in `Kube`; a new platform credential there must be added), and to the AppProject the kinds OnePasswordItem, CronJob, ScaledObject (and Job, HPA for the tree), and to the ApplicationSet `ignoreDifferences` on `spec.replicas` of Deployments annotated `forge.dev/autoscaled` with `RespectIgnoreDifferences`. Checked: no drift before; the diff was exactly that plus the image; server dry run; no drift after; `forge version` says server 0.8.1; deogratias and forge-web, staging and production, Synced and Healthy on the new revision, their addresses answer 200. Not checked: a service using the options (none does yet): a OnePasswordItem from `Kube`, a CronJob, a KEDA ScaledObject through Argo CD, and the replica ignore rule in Argo CD. Rollback file: the `v0.7.0` render (forge `v0.7.0`'s `render.sh`, chart `e7f0b8e`, with `FORGE_ARGOCD_URL`).

Before it, from 2026-10-13: forge-api `v0.7.0` (Argo CD links, `FORGE_ARGOCD_URL`), and before that `v0.6.0`: forge-api `v0.6.0` (the address setting, `SetAddress`, forge#60; the render above, from `render.sh` at that version, which adds a root listener per public domain). Checked: no drift before; the diff was the image and the listeners `public-showmino-com-root` (`showmino.com`) and `public-deogratias-cloud-root` (`deogratias.cloud`) alone; no drift after; `forge version` says server 0.6.0; all seven Gateway listeners Programmed; `forge service address deogratias` shows the forge.yaml address and `--host @ --domain deogratias.cloud --dry-run` plans `https://staging.deogratias.cloud` and `https://deogratias.cloud` (in use, to claim); forge-web's deployments and `https://forge.showmino.com` unchanged. Not checked: a root address end to end (no service has one yet; `deogratias.cloud` needs the tunnel route in showmino/kubernetes first). Rollback file: the same render with `v0.5.0` and the `render.sh` of forge `e5bcf0c^1`.

Before it, since 2026-10-12: forge-api `v0.5.0` (TechDocs milestone 1, `DocsService`; the render above, changed only in its image; checked: no drift before, the diff was the image line alone, `forge version` says server 0.5.0, `forge docs list` answers, `forge docs get forge-web` says "its repo has no docs/index.md" (so the head, root tree and docs/ tree are read from real GitHub). After forge-web got `docs/` (forge-web#20): `forge docs list` shows forge-web with 4 pages, `forge docs get forge-web --pages` gives the order of its `docs:` list, `forge docs get forge-web runbook.md` prints the page, and `-o json` has the commit, the change time and the edit link on `main`, so blobs, the default branch and the last change are read from real GitHub too. A list answers from the cache and refreshes behind it: a service's new docs appear on the next list after about a minute., `forge team list`, `forge deploy status forge-web` and `/healthz` as before). Before it, since 2026-10-11: forge-api `v0.4.0` (`run.host`; before it `v0.3.0`, public addresses, and `v0.2.0`, which keeps sessions). Earlier: forge-api `v0.2.0` (`ghcr.io/showmino/forge-api:v0.2.0`; before it `a846a98`, then `57a118e` with `TeamService`, then `3f2c8d3`, which makes `DeleteService` take down what a service runs, then `v0.1.0`, the first image built by the release workflow, which cuts versions; `v0.2.0` keeps sessions and applied migration 0004, the `sessions` table) and Dex with the client `forge-web`. Check teams with `bin/forge team list` (mino, platform-engineering, test-team on 2026-10-11). Every render must carry the settings of the block above: one that leaves a setting out removes what it configured
(without `FORGE_WEB_ORIGINS`, the dashboard's login client and its CORS access). Restart Dex after a change to its config
(`kubectl -n forge rollout restart deploy/dex`): it reads the config only at start.

Check the login path without a browser, against local Dex and `make api` (or a throwaway Dex on another port):

```sh
python3 -I docs/agents/scripts/web-login-check.py                 # http://localhost:8080, origin http://localhost:3000
```

It walks discovery, the PKCE code flow with the static password, the token exchange without a client secret,
the preflight and a call with the dashboard's token: 14 lines, all `ok`. On k3s logins go through GitHub, so
there only the first part can be checked by script (GetServerInfo, the preflight and Dex's discovery with an
`Origin` header); the rest needs a person in a browser.

The dashboard's repo is `showmino/forge-web` (on the owner's machine it is checked out next to this one, `../forge-web`).
After an API change, run `sh scripts/sync-proto.sh ../forge` in that repo: the script lives there.

Releasing Forge itself: push a `vX.Y.Z` tag on `main` of `showmino/forge`. `release.yml` (goreleaser) publishes the
GitHub release with the CLI for Linux, macOS and Windows (checksums signed with cosign, SBOMs) and
`ghcr.io/showmino/forge-api:vX.Y.Z` (also `latest`), which is the image to roll out. The Homebrew cask is skipped
until there is a `showmino/homebrew-tap` and the secret `HOMEBREW_TAP_GITHUB_TOKEN`. Release builds of the CLI link
`version.APIURL=https://forge-api.showmino.com` as the lowest-precedence API (`forge doctor` shows it as
`(built-in)`); change it in `.goreleaser.yaml` if the API's address changes. The image package must let the
repo's workflows write: package settings, "Manage Actions access", `forge` with role Write (set on 2026-10-11; the
first run failed with `permission_denied: write_package` without it). A run that failed before publishing can be
re-run on the same tag.

Sessions (live since forge-api `v0.2.0`): `GetServerInfo` answers `sessions: true` (forge-api has the GitHub App). After
`bin/forge auth login`, `bin/forge auth status` shows a `session` row and `bin/forge auth session list` the sessions of
every machine and browser; the credentials file holds a `session_token` and no ID or refresh token. The dashboard keeps
its session in the browser's `localStorage` (`forge.session`) for 7 days. To see the membership check, look for
`checking a session's memberships` (a failure) or `session ended` in forge-api's log; a session forge-api ended itself
is `forge audit list --action auth.session.end`. If a login is refused with "does not say which GitHub user it is",
Dex's ID token lacks `preferred_username` (scope `profile`). Why sessions exist: Dex's log showed
`failed to refresh identity ... Bad credentials` each time ten newer GitHub sign-ins had pushed out the CLI's token.

Versions (live since forge-api `v0.1.0`): `bin/forge deploy start <svc> --env staging --dry-run` and
`bin/forge deploy promote <svc> --dry-run [--bump minor|major]` print the version they would cut and change nothing.
A real staging deploy shows a `release` step ("release candidate v0.1.0-rc.1 for <sha7>"); then check, in the
service's repo, the tag, the pre-release by `mino-forge[bot]`, and a `release` workflow run that adds the tag to the
image (`docker buildx imagetools inspect ghcr.io/showmino/<svc>:<version>` lists the digest of `:<sha7>`). First
cut live on 2026-10-11: `forge-web` `v0.1.0-rc.1` at `4dad38d`. Deployments from before the rollout show no version
until they are deployed again. A service made before templates `v1.4.0` has no `release.yml`: it gets tags and
releases but no version tag on its image until the file is added.

Deleting a service (live since forge-api `3f2c8d3`): `bin/forge service delete <svc> --dry-run` prints what would be
taken down and changes nothing; forge-api's own account must be allowed both of
`kubectl auth can-i delete namespaces --as=system:serviceaccount:forge:forge-api` and
`kubectl auth can-i get applications.argoproj.io -n argocd --as=system:serviceaccount:forge:forge-api` (both yes
on 2026-10-11). A real delete waits for Argo CD's ApplicationSet (it looks at the deploy repo every 30s) to remove
each application, so expect about a minute per environment.
