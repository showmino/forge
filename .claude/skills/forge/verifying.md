# Verifying Forge end to end

Run from the repo root. `$GO` means `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`
(Go is not on the system PATH on the dev machine).

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
python3 -I .claude/skills/forge/scripts/approve-device-login.py bin/forge
```

Then check: `forge auth status` (permissions `services.read, services.write`),
`forge service register payments --team billing --owner a@b.c` then `service list`,
`forge doctor` (all ok, no version warning), `FORGE_TOKEN=nope forge service list`
exits 4 with `hint: run 'forge auth login'`.

Whole CLI against a live API (every noun and verb, output formats, exit codes,
forge.yaml defaults, logout/login): run
`bash .claude/skills/forge/scripts/cli-smoke.sh bin/forge <context> > smoke.log; tail -1 smoke.log`
and read every `FAIL` block. When behaviour changes on purpose, change the case's expected exit.

Refresh: set `expiry` in `~/.config/forge/credentials.json` to a past time, run
`forge auth token`; the token must change and the file stay mode 0600.

## 3. The demo cluster (k3s at 10.150.0.10-15)

The cluster is shared with real workloads (argocd, backstage, finance...). Touch
only the `forge` namespace. Facts that shape the deploy:

| Fact | Consequence |
| --- | --- |
| No Traefik, no IngressClass; apps use Istio `main-gateway` on `*.showmino.com` (public via Cloudflare) | Use `render.sh --lb`, not the Ingress mode or Istio, so Dex's demo password stays on the LAN |
| MetalLB v0.14.5, pool 10.150.1.200-250 | Forge uses **10.150.1.220** (Dex) and **10.150.1.221** (API); annotation is `metallb.universe.tf/loadBalancerIPs` (`metallb.io/` needs 0.15+) |
| Dev machine `srv-ovo` is not a node, no SSH to nodes | No `k3s ctr images import`; push to `ghcr.io/showmino/forge-api:<short-sha>` (private package, docker is logged in to ghcr) |
| Images pull with the `mino-ghcr` dockerconfigjson secret (in `docs` ns) | Copy it into `forge` and attach it to the default ServiceAccount |
| Istio 1.30 with Gateway API v1.6.1 (GatewayClass `istio`), no `discoverySelectors`, no PeerAuthentication | Previews use their own Gateway `forge/forge-previews` on **10.150.1.222** (`*.preview.10.150.1.222.nip.io`), LAN only |

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
python3 -I .claude/skills/forge/scripts/approve-device-login.py bin/forge
bin/forge doctor
```

forge-api logs `forge-api listening ... issuer=http://dex.10.150.1.220.nip.io/dex`
only after OIDC discovery from inside the pod succeeded. With the App, `bin/forge
template list` must show `go-grpc`, and `bash .claude/skills/forge/scripts/cli-smoke.sh
bin/forge k3s` must report `0 failed`. Every rollout empties the in-memory catalog.

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

Tear down, previews first (the reaper dies with forge-api, and the ClusterRole is
cluster-scoped, so deleting namespace `forge` leaves both behind). With deploys on,
do the Deploys teardown (below) first, otherwise services lose their routes:

```sh
kubectl delete ns -l app.kubernetes.io/managed-by=forge,forge.dev/environment
kubectl delete ns forge
kubectl delete clusterrole,clusterrolebinding forge-api-previews
```

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

Live on k3s since 2026-10-11: forge-api `3f2c8d3` (first `a846a98`; `57a118e` added `TeamService`; `3f2c8d3` makes `DeleteService` take down what a service runs) and Dex with the client `forge-web`. Check teams with `bin/forge team list` (mino, platform-engineering, test-team on 2026-10-11). From now on every
render must also carry `FORGE_WEB_ORIGINS=http://forge-web.staging.10.150.1.222.nip.io,http://forge-web.production.10.150.1.222.nip.io`
(next to `FORGE_DEPLOYS=1`, the chart revision, the App and both 1Password items); a render without it removes
the dashboard's login client and its CORS access. Restart Dex after a change to its config
(`kubectl -n forge rollout restart deploy/dex`): it reads the config only at start.

Check the login path without a browser, against local Dex and `make api` (or a throwaway Dex on another port):

```sh
python3 -I .claude/skills/forge/scripts/web-login-check.py                 # http://localhost:8080, origin http://localhost:3000
```

It walks discovery, the PKCE code flow with the static password, the token exchange without a client secret,
the preflight and a call with the dashboard's token: 14 lines, all `ok`. On k3s logins go through GitHub, so
there only the first part can be checked by script (GetServerInfo, the preflight and Dex's discovery with an
`Origin` header); the rest needs a person in a browser.

The dashboard's repo is `showmino/forge-web` (checkout `/home/mino/code/forge-web`); after an API change run
`sh scripts/sync-proto.sh ../forge` there.

Deleting a service (live since forge-api `3f2c8d3`): `bin/forge service delete <svc> --dry-run` prints what would be
taken down and changes nothing; forge-api's own account must be allowed both of
`kubectl auth can-i delete namespaces --as=system:serviceaccount:forge:forge-api` and
`kubectl auth can-i get applications.argoproj.io -n argocd --as=system:serviceaccount:forge:forge-api` (both yes
on 2026-10-11). A real delete waits for Argo CD's ApplicationSet (it looks at the deploy repo every 30s) to remove
each application, so expect about a minute per environment.
