# Phase 6: the web dashboard

Date: 2026-10-11. Status: design, awaiting the owner's approval.

## Goal

A browser UI on the same Forge API, so that a writer can create, preview and promote a service without a terminal, and a reader sees the same data. It is a second thin client next to the CLI: permissions, policy and the audit log stay in forge-api.

Exit gate (from `docs/index.md`): a writer creates, previews and promotes a service from the browser, and a reader sees the same data, all without a terminal.

## Decisions

| Decision | Choice |
| --- | --- |
| Where it lives | Its own repo, `showmino/forge-web`, made by `forge create --template nextjs` and deployed by Forge like any other service |
| How it talks to the API | The browser calls forge-api directly over ConnectRPC (Connect's web client). No dashboard-only endpoints and no proxy |
| Login | Authorization code flow with PKCE against Dex, public client `forge-web`. The ID token is the bearer, as for the CLI |
| UI | Tailwind 4 from the template and a few small components of our own, built from the Forge design system (https://claude.ai/artifact/9BE6LhhUayv1FtFs4GGQGq): its tokens become the app's Tailwind theme, and its seven components are the specification for ours |
| Milestones | 1: login and read-only pages, live on k3s. 2: actions. 3: polish and the exit gate |

Not in this phase: resource pages (resources are switched off on k3s; their rollout is deferred), HTTPS for the demo cluster, a production identity provider.

## Changes to forge-api (milestone 1)

1. **CORS.** A flag `--cors-origins` (env `FORGE_CORS_ORIGINS`), a comma-separated list of exact origins. Empty means off, which is today's behaviour. No wildcard and no credentials mode: the dashboard sends a bearer token, never a cookie. It applies to the main listener only, never to the runner listener.
2. **A second accepted audience.** A flag `--oidc-web-client-id` (env `FORGE_OIDC_WEB_CLIENT_ID`). A token is accepted when its audience holds the CLI's client ID or this one. Empty means only the CLI's, as today.
3. **Discovery.** `GetServerInfo` already returns the issuer, the CLI's client ID and the scopes without a login. `OIDCConfig` gains `web_client_id`, so the dashboard needs only the API's address.
4. **Dex and the manifests.** `render.sh` takes `FORGE_WEB_ORIGINS` (a comma-separated list). When set, it adds the Dex client `forge-web` (public, redirect URI `<origin>/auth/callback` for each origin), puts the origins in Dex's `web.allowedOrigins`, and sets the two forge-api variables above. Unset, the rendered output does not change.

Live progress needs nothing new: `WatchOperation` is already a Connect server stream, which a browser can read.

## Login in the browser

- The app reads the issuer, client ID and scopes from `GetServerInfo`, then Dex's discovery document.
- It makes a random state and a PKCE verifier, keeps both in `sessionStorage`, and sends the browser to Dex with the S256 challenge. Dex sends the user through GitHub and back to `/auth/callback`.
- The callback checks the state, exchanges the code and verifier at Dex's token endpoint, and keeps the ID token in `sessionStorage`. It is gone when the tab closes.
- The demo cluster is plain HTTP, where the browser's `crypto.subtle` is not available. The SHA-256 for the challenge comes from `@noble/hashes`; randomness comes from `crypto.getRandomValues`, which works everywhere.
- No refresh token in milestones 1 and 2. When the token has expired (one hour) or the API answers "unauthenticated", the app sends the user through login again and returns them to the page they were on. Milestone 3 decides whether that is good enough.
- The app never judges permissions itself. It calls `GetStatus` and uses the answer only to hide buttons a reader cannot use; the API still decides every call.
- The app sets a Content-Security-Policy that allows scripts from itself only, and connections to forge-api and Dex only.

## The app

- **Shape.** Next.js (the template's standalone build). The server serves the pages and `/api/healthz` and holds no secrets. Every page fetches its data in the browser with the user's token.
- **API client.** The protos are copied into the repo from a pinned forge commit (the forge repo is private); `buf generate` with `protoc-gen-es` writes the TypeScript client, which is committed, and CI checks it is up to date.
- **Dependencies added to the template:** `@connectrpc/connect`, `@connectrpc/connect-web`, `@bufbuild/protobuf`, `@tanstack/react-query`, `@noble/hashes`.
- **API address.** `NEXT_PUBLIC_FORGE_API_URL`, set when the image is built, from the organization variable `FORGE_API_URL` that the template's CI already uses. There is one cluster and one API, so staging and production of the dashboard talk to the same API.

### Pages

| Milestone | Pages |
| --- | --- |
| 1, read-only | Login and callback. Catalog: the list of services with a filter. Service: details, owners, repo, the state of staging and production, its previews, its recent audit events. Templates: list and one template with its parameters. Audit log: a list with filters for service, actor and action. A header with who is logged in, reader or writer, and sign out |
| 2, actions (writers) | Create a service from a template, with a form built from the template's parameters and live progress. Preview environments: create, extend, destroy. Deploys: start, promote and roll back, each with a confirmation. Operations: list, live progress, cancel |
| 3 | Empty and error states, keyboard and screen-reader pass, the refresh decision, the exit gate |

## Testing

Short and fast, no browser test suite.

- forge: unit tests for CORS (allowed origin, other origin, preflight, off by default) and for the two audiences; the golden for `render.sh` with and without `FORGE_WEB_ORIGINS`.
- forge-web: Vitest for the login helpers (challenge, state check, expiry) and one render test per page against Connect's in-memory transport.
- One check by hand on k3s at the end of each milestone.

## Rollout of milestone 1

Each step on k3s needs the owner's OK when it comes up.

1. Merge the forge-api changes; roll out forge-api and Dex with `FORGE_WEB_ORIGINS` set to the dashboard's staging and production addresses (render, server dry run, `kubectl diff`, apply, with a rollback file). Dex restarts; logins already made stay valid.
2. `forge create forge-web --template nextjs`.
3. Push the dashboard to `forge-web` through pull requests; its CI deploys staging.
4. Check login and the read-only pages on staging as a writer and as a reader, then promote to production.
