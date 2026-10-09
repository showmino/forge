# Next.js template

Oct 9, 2026 · Showmino · Draft for review

A second Forge template, `nextjs`, in `showmino/forge-templates`, next to `go-grpc`. A team runs `forge create <name> --template nextjs`. The new repo's CI then tests, builds an image and deploys `main` to staging, as go-grpc services do since Phase 4. `forge env create` previews it as well.

## Decisions

| Question | Decision |
| --- | --- |
| What teams build | Server-rendered web frontends: pages rendered on the server, API routes, server-side calls to other Forge services. The Node server runs in the pod. |
| Writable space | Forge gives every workload a writable `/tmp`. The deploy chart and preview pods mount an `emptyDir` there, and the root filesystem stays read-only. The template points Next's `.next/cache` at `/tmp`. |
| Starter stack | TypeScript, App Router, Tailwind CSS 4, ESLint, Vitest, npm, Node 24 LTS, Next's standalone output. No end-to-end browser tests. |
| Release | The template repo is tagged as a whole, so this ships as **v1.3.0**. go-grpc is unchanged. |

Versions current on 2026-10-09: Next.js 16.4.0, Tailwind CSS 4.3.3, Vitest 5.0.3, Node 24 (`mirror.gcr.io/library/node:24-slim`, `gcr.io/distroless/nodejs24-debian12:nonroot`).

## 1. Forge: a writable `/tmp` for every workload

**Deploy chart** (`deploy/argocd/chart/templates/deployment.yaml`):
- **Pod volume:** `- name: tmp` with `emptyDir: {sizeLimit: 256Mi}`. It is disk-backed, so it does not count against the 512Mi memory limit.
- **Container mount:** `- {name: tmp, mountPath: /tmp}`.
- **Unchanged:** `readOnlyRootFilesystem: true`, `runAsNonRoot` and the rest.
- **Goldens:** the staging and production goldens show the new volume and mount.

**Preview pods** (`internal/server/env_cluster.go`): the preview Deployment gets the same volume and mount, so previews and deploys behave alike. A test asserts the pod spec carries them.

**Rollout:**
1. A new forge-api image.
2. One re-render with `FORGE_DEPLOYS=1` and the new forge commit as `FORGE_DEPLOY_CHART_REVISION` (a commit on `origin/main`).
3. Argo re-syncs every deployed service; each restarts once with the extra volume.
4. The AppProject needs no change, since a volume is not a new resource kind.

The usual order applies: a server dry run first, then `kubectl diff`, then apply, with a rollback file kept to hand.

This also closes the Phase 3 backlog item "no `/tmp` emptyDir".

## 2. The `nextjs` template

**`template.yaml`**:
- name `nextjs`, title "Next.js web app";
- description "Server-rendered Next.js app (TypeScript, Tailwind) with CI, Dockerfile and forge.yaml";
- tags `[typescript, nextjs, react, web]`, owner `platform`;
- delimiters `["{%", "%}"]`, because JSX's `style={{…}}` clashes with Go's default `{{ }}`;
- one parameter, `port` (int, default 3000, "Port the server listens on").

**`skeleton/`**:

| File | Purpose |
| --- | --- |
| `app/layout.tsx`, `app/page.tsx` | Root layout and a Tailwind-styled starter page showing the service name, its team, and pointers to `forge deploy status` and the README. |
| `app/api/healthz/route.ts` | `GET` returns `200 ok` as text; `export const dynamic = "force-dynamic"` so it is never cached. |
| `app/globals.css` | Tailwind 4 (`@import "tailwindcss"`) through `@tailwindcss/turbopack`, configured in `next.config.ts`; there is no `postcss.config.mjs`. |
| `lib/display-name.ts`, `lib/display-name.test.ts` | A small function the page uses, and its Vitest test, so `npm test` tests something real. |
| `next.config.ts` | `output: "standalone"`, `experimental.isrFlushToDisk: false` (the root filesystem is read-only, so revalidated pages stay in memory), and the Turbopack Tailwind loader. |
| `tsconfig.json`, `eslint.config.mjs`, `vitest.config.mts` | Strict TypeScript; Next's ESLint config; Vitest. |
| `package.json` | `name` is the service name. Scripts: `dev`, `build`, `start`, `lint`, `typecheck` (`tsc --noEmit`), `test` (`vitest run`). `engines.node` `>=24`. |
| `package-lock.json` | Committed for reproducible `npm ci`. Its `name` fields are templated with the service name. Generated with npm from the rendered `package.json`. |
| `forge.yaml` | Service, team, owners and repo, as go-grpc renders them. `run.port` is the parameter; `run.health: /api/healthz`. |
| `Dockerfile` | See below. |
| `.github/workflows/ci.yml` | See below. |
| `README.md`, `.gitignore`, `.dockerignore` | How to develop, test, deploy and preview; ignores `node_modules`, `.next`. |

**`Dockerfile`**, three stages:
1. **deps:** `FROM mirror.gcr.io/library/node:24-slim`, `npm ci`.
2. **build:** the same base; `npm run build`; then `ln -s /tmp .next/standalone/.next/cache`. The runtime image has no shell, so the link is made here. It points at `/tmp` itself, which always exists in Forge pods, rather than a subdirectory: a dangling link would break Next's recursive mkdir. Next then writes `/tmp/images` and `/tmp/fetch-cache`.
3. **runtime:** `FROM gcr.io/distroless/nodejs24-debian12:nonroot`.
   - It copies `.next/standalone`, `.next/static` and `public` into `/app`.
   - It sets `ENV NODE_ENV=production HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1`.
   - It runs as `USER 65532:65532`, `EXPOSE {% .Params.port %}`, `CMD ["server.js"]`; the distroless entrypoint is `node`.
   - The port comes from `$PORT`, which Forge sets in both deploys and previews.

**`ci.yml`**:
- **`test`:** on `ubuntu-latest`: `actions/setup-node` with Node 24 and npm cache, `npm ci`, `npm run lint`, `npm run typecheck`, `npm test`, `npm run build`.
- **`image`:** pushes to `main` only, after `test`, on `ubuntu-latest`. It logs in to GHCR with `GITHUB_TOKEN` and pushes `ghcr.io/{% .Repo.Owner %}/{% .Service.Name %}:${GITHUB_SHA::7}`, as go-grpc does.
- **`deploy`:** identical to go-grpc v1.2.1:
  - `runs-on: kube-runners`, `timeout-minutes: 20`, `concurrency: forge-deploy-staging`, `id-token: write`;
  - the CLI is downloaded from `$FORGE_API_URL/cli/forge-linux-<arch>`;
  - the job runs `forge deploy start --env staging`.

## 3. Verification

**Forge:**
- the chart goldens include the `tmp` volume and mount;
- a preview test asserts the pod spec;
- `make chart-test` and `make test lint` pass;
- on the cluster, the rollout order from section 1, after which every existing Forge service is Synced and Healthy.

**Template, before tagging:**
- `forge template lint` reports no problems.
- The skeleton renders with sample values; then `npm ci && npm run lint && npm run typecheck && npm test && npm run build` passes.
- Run the image with `--read-only --tmpfs /tmp --user 65532 -e PORT=3000`. The page and `/api/healthz` must answer, and a response that triggers a cache write must work. The image optimiser on a `public/` image does that: it writes under `.next/cache/images`.

**Live on k3s, after v1.3.0:**
1. `forge create web-<date> --template nextjs`.
2. Its CI runs test → image → deploy, green, with no human action.
3. The staging URL serves the starter page, and `/api/healthz` returns 200.
4. `forge env create` previews it, which exercises `/tmp` in preview pods.
5. Clean up as in the Phase 4 gate: the deploy-repo directory, the Applications, the namespaces, the catalog entry and the repo.

## Risks

- **Next's standalone server in a read-only container:** it must listen on `0.0.0.0:$PORT` and write only under `/tmp`. The read-only `docker run` check proves it before release.
- **`runAsNonRoot`:** the distroless `nonroot` image declares numeric uid 65532, which satisfies the chart.
- **Lockfile drift:** the committed `package-lock.json` must match `package.json`. `npm ci` in the template check and in every generated repo's CI fails loudly if it doesn't. Only the `name` fields vary per service, which npm accepts.
- **Image size and memory:** a standalone Next.js server fits comfortably in the 512Mi default limit for a starter app.

## Out of scope

- Static export, Playwright, a pnpm or Bun variant, and calling a backend service from the starter page.
- Public URLs and TLS, which remain out of scope as in Phase 4.
