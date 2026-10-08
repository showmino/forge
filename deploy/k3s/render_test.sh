#!/usr/bin/env sh
# Checks render.sh output with and without a GitHub App. Run: sh deploy/k3s/render_test.sh
set -eu
dir=$(dirname "$0")
fail=0
check() { # check <description> <command...>
  desc=$1; shift
  if "$@"; then echo "ok   $desc"; else echo "FAIL $desc"; fail=1; fi
}

without=$(sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)
with=$(FORGE_GITHUB_APP_ID=5239131 sh "$dir/render.sh" --lb 10.150.1.220 10.150.1.221)

check "no App: no App settings"       sh -c "! printf '%s' \"\$1\" | grep -q -e FORGE_GITHUB_APP -e forge-github-app" _ "$without"
check "no App: no #app markers"       sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$without"
check "App: id"                       sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_ID, value: \"5239131\"'" _ "$with"
check "App: key file"                 sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_APP_KEY_FILE, value: /etc/forge/github-app/private-key.pem'" _ "$with"
check "App: org and template repo"    sh -c "printf '%s' \"\$1\" | grep -q 'FORGE_GITHUB_ORG, value: showmino' && printf '%s' \"\$1\" | grep -q 'FORGE_TEMPLATES_REPO, value: showmino/forge-templates'" _ "$with"
check "App: secret volume, read-only" sh -c "printf '%s' \"\$1\" | grep -q 'secretName: forge-github-app' && printf '%s' \"\$1\" | grep -q 'readOnly: true'" _ "$with"
check "App: key readable by the API"  sh -c "printf '%s' \"\$1\" | grep -q 'fsGroup: 65532' && printf '%s' \"\$1\" | grep -q 'defaultMode: 0440'" _ "$with"
check "App: no #app markers"          sh -c "! printf '%s' \"\$1\" | grep -q '#app'" _ "$with"
check "memory limit 256Mi"            sh -c "printf '%s' \"\$1\" | grep -A12 'name: forge-api$' | grep -q 'limits: {memory: 256Mi}' || printf '%s' \"\$1\" | grep -q 'limits: {memory: 256Mi}'" _ "$with"
check "valid YAML for kubectl"        sh -c "printf '%s' \"\$1\" | kubectl apply --dry-run=client -f - >/dev/null" _ "$with"
check "bad App id refused"            sh -c "! FORGE_GITHUB_APP_ID='5; rm' sh \"$dir/render.sh\" --lb 10.150.1.220 10.150.1.221 >/dev/null 2>&1"
exit $fail
