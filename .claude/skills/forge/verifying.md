# Verifying Forge end to end

Run from the repo root. `$GO` means `PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH`
(Go is not on the system PATH on the dev machine).

## 1. Build, test, lint (CI parity)

```sh
$GO GOTOOLCHAIN=go1.24.13 make build test lint
```

CI resolves Go from `go 1.24` in go.mod and pins golangci-lint **v2.5.0**
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0`).
With a newer Go you get `typecheck ... export data version 4 is greater than
maximum supported version 2`; with a newer golangci-lint you get gosec G117 hits
on `internal/auth/store.go` that CI does not report. Neither is a code problem.

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

```sh
IMG=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
docker build --build-arg COMMIT=$(git rev-parse HEAD) -t $IMG . && docker push $IMG
FORGE_API_IMAGE=$IMG sh deploy/k3s/render.sh --lb 10.150.1.220 10.150.1.221 | kubectl apply -f -
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
only after OIDC discovery from inside the pod succeeded.

| Symptom | Cause |
| --- | --- |
| `ErrImagePull ... 401 Unauthorized` | Pod predates the pull-secret patch; restart the deployment |
| `CreateContainerConfigError: image has non-numeric user (nonroot)` | `runAsUser: 65532` missing from the pod securityContext |
| Service got a random pool IP | Wrong MetalLB annotation, or an existing allocation: delete the Service and re-apply |
| Every command warns "older than ... 0.1.0" | Go pseudo-version not treated as dev (fixed in `internal/version`) |

Tear down: `kubectl delete ns forge`.
