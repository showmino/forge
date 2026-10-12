---
name: forge
description: Use when working in the Forge repo (showmino/forge) - the internal developer platform with the forge CLI, forge-api, ConnectRPC protos, Dex login and the k3s deploy - for any change, new command or RPC, phase planning, build/test/lint failures, or verifying login and the cluster deploy.
---

# Forge

Everything an agent needs is in files every tool reads. This skill only points
at them, so that there is one copy.

1. `AGENTS.md` at the repo root: what Forge is, the rules (the cluster is
   production, what needs the owner's OK, merging, secrets), commands,
   conventions, mistakes made before.
2. `docs/agents/status.md`: what is built, what is live, what is left.
3. `docs/agents/verifying.md`: the runbook. **Required for any login or deploy
   check**, and for every rollout. Its scripts are in `docs/agents/scripts/`.
4. `docs/index.md`: the approved design. Read the relevant section before
   designing anything.

Do not add project knowledge here or to a private memory: put it in those
files, in the same pull request as the change it describes.
