# Next.js Template Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `forge create <name> --template nextjs` makes a server-rendered Next.js app. CI tests it, builds an image and deploys `main` to staging, and `forge env create` previews it, on the k3s cluster.

**Architecture:** Forge gives every workload a writable `/tmp` (an `emptyDir`) in the deploy chart and in preview pods; the root filesystem stays read-only. The new `nextjs` template in `showmino/forge-templates`:
- a Next.js 16 app with standalone output;
- `.next/cache` linked to `/tmp`;
- a distroless Node 24 runtime image;
- the same CI as go-grpc v1.2.1: test → image → deploy.

The templates repo's CI learns to build npm-based templates. The release is v1.3.0.

**Tech Stack:**
- Go 1.27.2 (forge), Helm chart.
- Next.js 16.4.0, React 19, TypeScript, Tailwind CSS 4.3.3, ESLint 9 flat config, Vitest 5.0.3.
- Node 24 (`mirror.gcr.io/library/node:24-slim`, `gcr.io/distroless/nodejs24-debian12:nonroot`).
- GitHub Actions.

**Spec:** `docs/specs/2026-10-09-nextjs-template-design.md`

## Global Constraints

- Writable `/tmp`: volume `tmp`, `emptyDir: {sizeLimit: 256Mi}` (disk-backed), mounted at `/tmp`, in the chart Deployment and in preview Deployments. `readOnlyRootFilesystem: true` and `runAsNonRoot` stay.
- Template `nextjs`:
  - title "Next.js web app", description "Server-rendered Next.js app (TypeScript, Tailwind) with CI, Dockerfile and forge.yaml";
  - tags `[typescript, nextjs, react, web]`, owner `platform`;
  - delimiters `["{%", "%}"]`;
  - one parameter `port` (int, default 3000, "Port the server listens on").
- `forge.yaml`: `run.port` = the parameter; `run.health: /api/healthz`.
- The `/api/healthz` route returns `200` with body `ok` and is never cached (`export const dynamic = "force-dynamic"`).
- `next.config.ts`: `output: "standalone"`. `.next/cache` in the image is a symlink to `/tmp` (not a subdirectory).
- Runtime image `gcr.io/distroless/nodejs24-debian12:nonroot`:
  - `USER 65532:65532`;
  - `ENV NODE_ENV=production HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1`;
  - `CMD ["server.js"]`; the port comes from `$PORT`.
- Build images: `mirror.gcr.io/library/node:24-slim`. Never pull from Docker Hub directly (it rate-limits CI runners).
- `package.json` scripts: `dev`, `build`, `start`, `lint`, `typecheck`, `test` (`vitest run`); `engines.node` `>=24`. `package-lock.json` is committed, with its `name` fields templated.
- CI:
  - `test` on `ubuntu-latest` (Node 24, `npm ci`, lint, typecheck, test, build);
  - `image` and `deploy` exactly as go-grpc v1.2.1, minus `make tools generate`.
- Release: forge-templates tag **v1.3.0**; go-grpc unchanged.
- Production cluster: kubectl only with `--dry-run` or read-only `get` during implementation. The controller does the rollout.
- Commit messages end with `Co-Authored-By: <the model that wrote the commit> <noreply@anthropic.com>`.

## Review Focus

1. **The image writes outside `/tmp`.** Any write Next makes at runtime must land under `/tmp`, or the pod crashes on the read-only filesystem. Pinned in Task 3: the read-only `docker run` check requests an optimised image, which makes Next write its image cache, and asserts the file appears in the mounted `/tmp`.
2. **A service name with a dash.** Lint's sample is `sample-service`. The `package.json` and lockfile `name`, the page title and the image name must all accept it. Pinned in Task 2 (the rendered sample builds) and Task 3 (the image builds).
3. **A lockfile that doesn't match `package.json`.** `npm ci` fails hard on drift. Pinned in Task 2: the templates repo's CI runs `npm ci` on the rendered template, so drift fails the PR.
4. **A non-default `port` (`--set port=8081`).** The server must listen on `$PORT`, which Forge sets from `forge.yaml`. Pinned in Task 3: the `docker run` uses `-e PORT=8081` and checks the server answers on 8081.
5. **Existing deployments when the chart gains a volume.** Re-syncing must restart them cleanly, not fail. Pinned in rollout step R1: after the re-render, every Forge Application is checked as Synced and Healthy.

---

### Task 1: A writable `/tmp` in deploy and preview pods (repo `showmino/forge`)

**Files:**
- Modify: `deploy/argocd/chart/templates/deployment.yaml`
- Modify: `deploy/argocd/chart/testdata/staging.golden`, `production.golden` (regenerate)
- Modify: `internal/server/env_cluster.go` (`deployment()`)
- Test: `internal/server/env_cluster_test.go` (the preview pod spec assertions)
- Modify: `.claude/skills/forge/SKILL.md` (remove "no `/tmp` emptyDir" from the Phase 3 backlog; note `/tmp` in the Phase 4 status)

**Interfaces:**
- Produces: every Forge pod (deploy and preview) has volume `tmp` (`emptyDir`, `sizeLimit: 256Mi`) mounted at `/tmp` in its single container.

- [ ] **Step 1: Write the failing preview test**

In `internal/server/env_cluster_test.go`, in the test that deploys `payments` into `pr-1` and asserts the pod spec, add these cases to the `switch` before `case len(pod.ImagePullSecrets) ...`:

```go
	case len(pod.Volumes) != 1 || pod.Volumes[0].Name != "tmp" || pod.Volumes[0].EmptyDir == nil ||
		pod.Volumes[0].EmptyDir.SizeLimit == nil || pod.Volumes[0].EmptyDir.SizeLimit.String() != "256Mi" || pod.Volumes[0].EmptyDir.Medium != "":
		t.Errorf("volumes: %+v", pod.Volumes)
	case len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].Name != "tmp" || ctr.VolumeMounts[0].MountPath != "/tmp" || ctr.VolumeMounts[0].ReadOnly:
		t.Errorf("volume mounts: %+v", ctr.VolumeMounts)
```

- [ ] **Step 2: Run it to see it fail**

Run: `export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH && go test ./internal/server -run TestCluster -v 2>&1 | grep -E 'volumes|FAIL|ok' | head`. Use the test's real name if it isn't `TestCluster...`: `grep -n "preview-pr-1" internal/server/env_cluster_test.go` finds it.
Expected: FAIL with `volumes: []`.

- [ ] **Step 3: Add the volume to preview pods**

In `internal/server/env_cluster.go` `deployment()`:
- Before `return &appsv1.Deployment{`:

  ```go
  	tmpLimit := resource.MustParse("256Mi")
  ```

- In the `corev1.PodSpec`, after `SecurityContext: ...`:

  ```go
  					// The root filesystem is read-only; /tmp is the one writable path.
  					Volumes: []corev1.Volume{{
  						Name:         "tmp",
  						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}},
  					}},
  ```

- In the container, after `SecurityContext: ...`:

  ```go
  						VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
  ```

`resource` is `k8s.io/apimachinery/pkg/api/resource`; the file already uses it for quotas, so check the import exists.

- [ ] **Step 4: Run the test**

Run: `go test -race ./internal/server -run 'TestCluster|TestEnv|TestCreate' 2>&1 | tail -3`
Expected: `ok`.

- [ ] **Step 5: Add the volume to the chart**

In `deploy/argocd/chart/templates/deployment.yaml`:
- Under the pod `spec:`, after `securityContext:` and its two lines, at the same indentation as `containers:`:

  ```yaml
        # The root filesystem is read-only; /tmp is the one writable path.
        volumes:
          - name: tmp
            emptyDir: {sizeLimit: 256Mi}
  ```

- In the container, after its `securityContext:` block (same indentation as `securityContext:`):

  ```yaml
            volumeMounts:
              - {name: tmp, mountPath: /tmp}
  ```

Regenerate both goldens with the commands from the Makefile's `chart-test` target, writing instead of diffing:

```sh
go run helm.sh/helm/v3/cmd/helm@v3.21.4 template payments deploy/argocd/chart --namespace payments-staging \
  -f deploy/argocd/chart/testdata/staging.yaml > deploy/argocd/chart/testdata/staging.golden
go run helm.sh/helm/v3/cmd/helm@v3.21.4 template payments deploy/argocd/chart --namespace payments-production \
  -f deploy/argocd/chart/testdata/production.yaml > deploy/argocd/chart/testdata/production.golden
git diff --stat deploy/argocd/chart/testdata
```

The golden diff must be exactly the added `volumes` and `volumeMounts` lines in each file. Run `make chart-test`; it must show no diff.

Then a server-side dry run of the rendered staging golden against the cluster validates the schema without changing anything:
`kubectl apply --dry-run=server -n default -f <(sed -n '/kind: Deployment/,/^---/p' deploy/argocd/chart/testdata/staging.golden | sed '/^---/d' | sed '1i apiVersion: apps/v1')`.

If that one-liner is awkward, render just the Deployment with `helm template ... --show-only templates/deployment.yaml` and pipe it to `kubectl apply --dry-run=server -n default -f -`. Report the output (`created (server dry run)`).

- [ ] **Step 6: SKILL.md**

In `.claude/skills/forge/SKILL.md`:
- In the Phase 3 backlog "Pods and inputs" line, change "no `runAsUser` (images with a named USER fail) and no `/tmp` emptyDir" to "no `runAsUser` (images with a named USER fail)".
- In the Phase 4 status line, append: " Pods get a writable `/tmp` (emptyDir, 256Mi) since the Next.js template (docs/specs/2026-10-09-nextjs-template-design.md)."

- [ ] **Step 7: `make test lint`, commit**

```bash
git add deploy/argocd/chart/templates/deployment.yaml deploy/argocd/chart/testdata/staging.golden deploy/argocd/chart/testdata/production.golden internal/server/env_cluster.go internal/server/env_cluster_test.go .claude/skills/forge/SKILL.md
git commit -m "pods: a writable /tmp emptyDir in deploys and previews"
```

---

### Task 2: The `nextjs` template's app, config and template CI (repo `showmino/forge-templates`)

Work in `~/code/forge-templates` on a new branch `nextjs` from `main`. Do not push.

**Files:**
- Create: `nextjs/template.yaml`
- Create in `nextjs/skeleton/`:
  - `app/layout.tsx`, `app/page.tsx`, `app/globals.css`, `app/api/healthz/route.ts`;
  - `lib/display-name.ts`, `lib/display-name.test.ts`;
  - `public/robots.txt`;
  - `next.config.ts`, `tsconfig.json`, `eslint.config.mjs`, `postcss.config.mjs`, `vitest.config.ts`;
  - `package.json`, `package-lock.json`, `forge.yaml`, `README.md`, `.gitignore`.
- Modify: `.github/workflows/lint.yml` (build npm-based templates)
- Modify: `README.md` (repo root: list the `nextjs` template if the README lists templates)

**Interfaces:**
- Produces: a rendered app where `npm ci && npm run lint && npm run typecheck && npm test && npm run build` passes, with `.next/standalone/server.js` built. Task 3 adds the Dockerfile and the service CI.

- [ ] **Step 1: Generate a reference app for exact config files**

Next 16's ESLint flat config, `tsconfig.json` and Tailwind 4 PostCSS setup must match the version, so take them from `create-next-app` instead of writing them by hand:

```sh
cd /tmp/claude-1000 && rm -rf next-ref && npx --yes create-next-app@16.4.0 next-ref --ts --tailwind --eslint --app --no-src-dir --import-alias '@/*' --use-npm --yes
ls next-ref; cat next-ref/package.json next-ref/eslint.config.mjs next-ref/tsconfig.json next-ref/postcss.config.mjs next-ref/next.config.ts next-ref/.gitignore
```

If a flag is rejected, run `npx create-next-app@16.4.0 --help` and use the equivalents: TypeScript, Tailwind, ESLint, App Router, no `src/`, npm.

- [ ] **Step 2: Write the skeleton**

**Copied from the reference app, unchanged:**
- `tsconfig.json`
- `eslint.config.mjs`
- `postcss.config.mjs`

**Copied and extended:** `.gitignore` from the reference, plus these lines if missing:

```
/node_modules
/.next/
/out/
next-env.d.ts
```

**`next.config.ts`:**

```ts
import type { NextConfig } from "next";

// Standalone output: the image ships only .next/standalone, .next/static and public.
const nextConfig: NextConfig = {
  output: "standalone",
};

export default nextConfig;
```

**`package.json`:** start from the reference's dependency and devDependency versions, exactly as it pinned them. Then:
- add `"vitest": "5.0.3"` to `devDependencies`;
- set `name`, `private`, `engines` and `scripts`:

```json
{
  "name": "{% .Service.Name %}",
  "version": "0.1.0",
  "private": true,
  "engines": { "node": ">=24" },
  "scripts": {
    "dev": "next dev",
    "build": "next build",
    "start": "next start",
    "lint": "eslint .",
    "typecheck": "next typegen && tsc --noEmit",
    "test": "vitest run"
  },
  "dependencies": { "...": "from the reference" },
  "devDependencies": { "...": "from the reference, plus vitest" }
}
```

Replace the two `"..."` lines with the real maps. If `next typegen` does not exist in 16.4.0, use `"typecheck": "tsc --noEmit"` after making sure `next-env.d.ts` is generated by `next build`, and say so in the report.

**`vitest.config.ts`:**

```ts
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    include: ["**/*.test.ts"],
    exclude: ["node_modules/**", ".next/**"],
  },
});
```

**`lib/display-name.ts`:**

```ts
// displayName turns a service name such as "billing-web" into "Billing Web".
export function displayName(service: string): string {
  return service
    .split(/[-_]+/)
    .filter(Boolean)
    .map((word) => word[0].toUpperCase() + word.slice(1))
    .join(" ");
}
```

**`lib/display-name.test.ts`:**

```ts
import { describe, expect, it } from "vitest";
import { displayName } from "./display-name";

describe("displayName", () => {
  it.each([
    ["web", "Web"],
    ["billing-web", "Billing Web"],
    ["a--b_c", "A B C"],
    ["", ""],
  ])("%s → %s", (service, want) => {
    expect(displayName(service)).toBe(want);
  });
});
```

**`app/globals.css`:**

```css
@import "tailwindcss";
```

**`app/layout.tsx`:**

```tsx
import type { Metadata } from "next";
import "./globals.css";
import { displayName } from "@/lib/display-name";

export const metadata: Metadata = {
  title: displayName("{% .Service.Name %}"),
  description: "{% .Service.Name %}, owned by {% .Service.Team %}",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-slate-50 text-slate-900 antialiased">{children}</body>
    </html>
  );
}
```

**`app/page.tsx`:**

```tsx
import { displayName } from "@/lib/display-name";

const service = "{% .Service.Name %}";
const team = "{% .Service.Team %}";

export default function Home() {
  return (
    <main className="mx-auto flex max-w-2xl flex-col gap-6 px-6 py-24">
      <p className="text-sm font-medium uppercase tracking-wide text-indigo-600">Forge · Next.js</p>
      <h1 className="text-4xl font-semibold">{displayName(service)}</h1>
      <p className="text-lg text-slate-600">
        Owned by <span className="font-medium text-slate-900">{team}</span>. Edit{" "}
        <code className="rounded bg-slate-200 px-1.5 py-0.5 text-sm">app/page.tsx</code> and push to{" "}
        <code className="rounded bg-slate-200 px-1.5 py-0.5 text-sm">main</code>: CI tests it, builds the image and
        deploys it to staging.
      </p>
      <ul className="list-disc space-y-1 pl-6 text-slate-600">
        <li>
          <code>forge deploy status {service}</code>: what runs where
        </li>
        <li>
          <code>forge env create &lt;name&gt; --service {service}</code>: a preview of any branch
        </li>
        <li>
          <code>forge deploy promote {service}</code>: staging to production
        </li>
      </ul>
    </main>
  );
}
```

**`app/api/healthz/route.ts`:**

```ts
// Liveness and readiness probe target (forge.yaml run.health). Never cached.
export const dynamic = "force-dynamic";

export function GET(): Response {
  return new Response("ok\n", { headers: { "content-type": "text/plain; charset=utf-8" } });
}
```

**`public/robots.txt`:**

```
User-agent: *
Disallow: /
```

**`forge.yaml`:** the same as go-grpc's `skeleton/forge.yaml` (copy it), with `health: /api/healthz` instead of `/healthz`.

**`README.md`:**

````markdown
# {% .Service.Name %}

Owned by **{% .Service.Team %}**. Scaffolded by `forge create` from the
`{% .Template.Name %}` template ({% .Template.Version %}): a server-rendered
Next.js app with TypeScript and Tailwind CSS.

## Develop

```sh
npm ci
npm run dev        # http://localhost:3000
npm run lint && npm run typecheck && npm test
npm run build      # standalone server in .next/standalone
```

## Ship

CI tests every push. On `main` it pushes
`ghcr.io/{% .Repo.Owner %}/{% .Service.Name %}:<short-sha>`, then a `deploy`
job deploys that commit to staging through Forge. It logs in with the job's
GitHub OIDC token, and needs the organization variable `FORGE_API_URL`.

- `forge deploy status {% .Service.Name %}`: what runs where.
- `forge deploy promote {% .Service.Name %}`: staging to production.
- `forge env create <name> --service {% .Service.Name %} --ref <branch>`: a
  preview with its own URL.

The server listens on `$PORT` (`run:` in `forge.yaml`). In Forge the root
filesystem is read-only, and Next's cache (`.next/cache`) is a link to the
writable `/tmp`.
````

**`template.yaml`:**

```yaml
apiVersion: forge.dev/v1
kind: Template
name: nextjs
title: Next.js web app
description: Server-rendered Next.js app (TypeScript, Tailwind) with CI, Dockerfile and forge.yaml
tags: [typescript, nextjs, react, web]
owner: platform
# {% %} because JSX's style={{...}} clashes with [[ ]]/{{ }}, and GitHub Actions uses ${{ }}.
delimiters: ["{%", "%}"]
parameters:
  - name: port
    type: int
    default: 3000
    description: Port the server listens on
```

- [ ] **Step 3: Generate the lockfile from a sample render**

```sh
cd ~/code/forge && export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
rm -rf /tmp/claude-1000/nextjs-render && go run ./cmd/forge template lint ~/code/forge-templates --render-dir /tmp/claude-1000/nextjs-render
cd /tmp/claude-1000/nextjs-render/nextjs && npm install --package-lock-only --no-audit --no-fund
```

The skeleton has no lockfile yet, so lint may warn about nothing; if it reports a problem, fix it first. Then copy the lockfile into the skeleton with the sample name templated back:

```sh
sed 's/"name": "sample-service"/"name": "{% .Service.Name %}"/g' package-lock.json > ~/code/forge-templates/nextjs/skeleton/package-lock.json
grep -c '{% .Service.Name %}' ~/code/forge-templates/nextjs/skeleton/package-lock.json   # expect 2
grep -c 'sample-service' ~/code/forge-templates/nextjs/skeleton/package-lock.json        # expect 0
grep -c '{%' ~/code/forge-templates/nextjs/skeleton/package-lock.json                    # expect 2 (no other delimiter in the lockfile)
```

- [ ] **Step 4: Check the rendered app**

```sh
cd ~/code/forge && rm -rf /tmp/claude-1000/nextjs-render && go run ./cmd/forge template lint ~/code/forge-templates --render-dir /tmp/claude-1000/nextjs-render
cd /tmp/claude-1000/nextjs-render/nextjs && npm ci && npm run lint && npm run typecheck && npm test && npm run build && ls .next/standalone/server.js
```

Expected: lint "2 templates checked, no problems." (or the repo's wording); every npm step passes; `server.js` exists. Then a smoke run:

```sh
PORT=3123 node .next/standalone/server.js & pid=$!; sleep 3
python3 -I -c 'import urllib.request as u; print(u.urlopen("http://127.0.0.1:3123/api/healthz").read()); print(b"Sample Service" in u.urlopen("http://127.0.0.1:3123/").read())'
kill $pid
```

Expected: `b'ok\n'` and `True`. (`.next/static` isn't copied in this smoke run; HTML still renders.)

- [ ] **Step 5: Teach the templates CI to build npm templates**

In `~/code/forge-templates/.github/workflows/lint.yml`:
- after the `actions/setup-go` step, add:

  ```yaml
      - uses: actions/setup-node@v4
        with:
          node-version: 24
  ```

- replace the last step's `run:` with:

  ```yaml
        run: |
          for dir in "$RUNNER_TEMP"/render/*/; do
            echo "::group::$dir"
            if [ -f "$dir/package.json" ]; then
              (cd "$dir" && npm ci && npm run lint && npm run typecheck && npm test && npm run build)
            else
              (cd "$dir" && make tools test lint)
            fi
            echo "::endgroup::"
          done
  ```

  and rename the step to `Build and test each rendered template`. It keeps that name.

Every command fails the step (`bash -e` is the Actions default), so a failing template fails the run.

- [ ] **Step 6: Commit**

```bash
cd ~/code/forge-templates && git checkout -b nextjs   # if not already on it
git add nextjs .github/workflows/lint.yml README.md
git commit -m "nextjs: a server-rendered Next.js template (app, config, lockfile); CI builds npm templates"
```

Remove `/tmp/claude-1000/next-ref` and `/tmp/claude-1000/nextjs-render` when done.

---

### Task 3: The `nextjs` image and the service CI (repo `showmino/forge-templates`, branch `nextjs`)

**Files:**
- Create: `nextjs/skeleton/Dockerfile`, `nextjs/skeleton/.dockerignore`, `nextjs/skeleton/.github/workflows/ci.yml`

**Interfaces:**
- Consumes: Task 2's skeleton. `npm run build` produces `.next/standalone/server.js`.
- Produces: an image that serves on `$PORT` as uid 65532 with a read-only root filesystem. Next writes only under `/tmp`.

- [ ] **Step 1: `Dockerfile`**

```dockerfile
# Built by CI and run by Forge with a read-only root filesystem: only /tmp is
# writable, so .next/cache is a link to /tmp. Images come through Google's
# mirror of Docker Hub, which does not rate-limit CI runners.
FROM mirror.gcr.io/library/node:24-slim AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

FROM mirror.gcr.io/library/node:24-slim AS build
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build \
 && rm -rf .next/standalone/.next/cache \
 && ln -s /tmp .next/standalone/.next/cache

FROM gcr.io/distroless/nodejs24-debian12:nonroot
WORKDIR /app
ENV NODE_ENV=production HOSTNAME=0.0.0.0 NEXT_TELEMETRY_DISABLED=1 PORT={% .Params.port %}
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public
USER 65532:65532
EXPOSE {% .Params.port %}
CMD ["server.js"]
```

**`.dockerignore`:**

```
node_modules
.next
.git
.github
*.md
```

- [ ] **Step 2: `.github/workflows/ci.yml`**

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 24
          cache: npm
      - run: npm ci
      - run: npm run lint
      - run: npm run typecheck
      - run: npm test
      - run: npm run build
```

Then append the `image` and `deploy` jobs copied verbatim from `go-grpc/skeleton/.github/workflows/ci.yml` (v1.2.1). Delete the `image` job's `actions/setup-go` step and its `- run: make tools generate` step; Next builds inside the Dockerfile. Everything else stays identical, including the `deploy` job's `runs-on: kube-runners`, `timeout-minutes: 20`, `concurrency`, `permissions` and both steps.

- [ ] **Step 3: Build the image from a sample render**

```sh
cd ~/code/forge && export PATH=$HOME/.local/go/bin:$HOME/go/bin:$PATH
rm -rf /tmp/claude-1000/nextjs-render && go run ./cmd/forge template lint ~/code/forge-templates --render-dir /tmp/claude-1000/nextjs-render
cd /tmp/claude-1000/nextjs-render/nextjs
python3 -I -c 'import zlib,struct
def chunk(t,d): return struct.pack(">I",len(d))+t+d+struct.pack(">I",zlib.crc32(t+d)&0xffffffff)
raw=b"".join(b"\x00"+b"\xff\x00\x00"*8 for _ in range(8))
open("public/check.png","wb").write(b"\x89PNG\r\n\x1a\n"+chunk(b"IHDR",struct.pack(">IIBBBBB",8,8,8,2,0,0,0))+chunk(b"IDAT",zlib.compress(raw))+chunk(b"IEND",b""))'
docker build -q -t nextjs-check:local .
```

`public/check.png` exists only in this check render, never in the skeleton. It lets the check trigger an image-cache write.

- [ ] **Step 4: Run it as Forge does, and prove writes land in `/tmp`**

```sh
tmp=$(mktemp -d /tmp/claude-1000/nextjs-tmp.XXXX) && chmod 777 "$tmp"
docker run -d --name nextjs-check --read-only --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
  -v "$tmp:/tmp" -e PORT=8081 -p 127.0.0.1:8081:8081 nextjs-check:local
sleep 4
python3 -I - <<'EOF'
import urllib.request as u
print("healthz", u.urlopen("http://127.0.0.1:8081/api/healthz").read())
print("page", b"Sample Service" in u.urlopen("http://127.0.0.1:8081/").read())
r = u.urlopen("http://127.0.0.1:8081/_next/image?url=%2Fcheck.png&w=64&q=75")
print("image", r.status, r.headers.get("content-type"))
EOF
ls -R "$tmp" | head
docker logs nextjs-check 2>&1 | tail -5
docker rm -f nextjs-check; rm -rf "$tmp"
```

Expected:
- `healthz b'ok\n'`, `page True`, `image 200 image/...`;
- `ls` shows an `images/` directory under the mounted `/tmp`, which proves the cache write went through the link;
- the logs show no `EROFS` or `read-only file system` errors;
- the server listened on 8081 from `-e PORT=8081`.

If the image cache lands elsewhere, or the logs show a read-only error, stop and report it. Do not loosen the read-only flag.

Clean up: `docker rmi nextjs-check:local`, and `rm -rf /tmp/claude-1000/nextjs-render`.

- [ ] **Step 5: Lint and commit**

`go run ./cmd/forge template lint ~/code/forge-templates` reports no problems. Then:

```bash
cd ~/code/forge-templates
git add nextjs/skeleton/Dockerfile nextjs/skeleton/.dockerignore nextjs/skeleton/.github/workflows/ci.yml
git commit -m "nextjs: distroless image with .next/cache in /tmp; CI test, image and deploy"
```

---

## Rollout (controller, after review)

- **R1, forge.**
  1. Merge the forge PR (Task 1).
  2. Build and push `ghcr.io/showmino/forge-api:<sha7>` from a clean `main`.
  3. Render with `FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=<that main sha>` and the same settings as the Phase 4 rollout. Also write a rollback render with the current image (`c31fba9`) and chart revision (`c31fba9f956238468689e7a38fb52097a57ad1e6`).
  4. `kubectl diff`: expect only the forge-api image and the ApplicationSet's `targetRevision`. Then apply and `rollout status`.
  5. Check that every `app.kubernetes.io/managed-by=forge` Application is Synced and Healthy after re-sync, and that its pods have the `tmp` volume.
- **R2, template release.**
  1. Push branch `nextjs`, open the PR, and wait for CI. CI now builds the rendered `nextjs` with npm.
  2. Merge, then `git tag -a v1.3.0 -m "nextjs template"` and push the tag.
  3. Check that `forge template list` shows `nextjs v1.3.0` and `go-grpc v1.3.0`.
- **R3, live check.**
  1. `forge create web-<MMDDhhmm> --template nextjs --team platform`.
  2. `gh run watch`: test → image → deploy, green.
  3. Staging: `/api/healthz` returns 200, and `/` contains the display name.
  4. `forge env create web-<x>-pr --service web-<x>` reaches READY, and its URL serves the page.
  5. Clean up: `forge env destroy`, the deploy-repo directory, Applications gone, namespaces, `forge service delete`, `gh repo delete`.
