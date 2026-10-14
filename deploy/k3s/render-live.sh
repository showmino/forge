#!/usr/bin/env sh
# Render the live Forge from live.env into live/forge.yaml, the file Argo CD
# compares with the cluster. A rollout changes live.env (usually the image),
# runs this, and commits both; --check fails when the file is not up to date.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out="$here/live/forge.yaml"
set -a
# shellcheck disable=SC1091
. "$here/live.env"
set +a
rendered=$(sh "$here/render.sh" --lb "$FORGE_LB_DEX_IP" "$FORGE_LB_API_IP")
if [ "${1:-}" = "--check" ]; then
  if [ "$rendered" != "$(cat "$out")" ]; then
    echo "deploy/k3s/live/forge.yaml is not the render of live.env: run sh deploy/k3s/render-live.sh and commit it" >&2
    exit 1
  fi
  exit 0
fi
mkdir -p "$here/live"
printf '%s\n' "$rendered" > "$out"
