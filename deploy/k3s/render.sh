#!/usr/bin/env sh
# Render the k3s manifests for a domain.
#
#   deploy/k3s/render.sh 192.168.1.50.nip.io | kubectl apply -f -
#   FORGE_API_IMAGE=ghcr.io/showmino/forge-api:0.1.0 deploy/k3s/render.sh forge.lan | kubectl apply -f -
set -eu

if [ $# -ne 1 ]; then
  echo "usage: $0 <domain>   (for example 192.168.1.50.nip.io)" >&2
  exit 2
fi
domain=$1
image=${FORGE_API_IMAGE:-forge-api:dev}

case $domain in
  *[!A-Za-z0-9.-]*) echo "invalid domain: $domain" >&2; exit 2 ;;
esac
case $image in
  *[!A-Za-z0-9./:@_-]*) echo "invalid image: $image" >&2; exit 2 ;;
esac

sed -e "s|\${FORGE_DOMAIN}|$domain|g" -e "s|\${FORGE_API_IMAGE}|$image|g" "$(dirname "$0")/forge.yaml.tmpl"
