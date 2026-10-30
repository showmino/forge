# Rules for the Helm chart a service without a repo runs (forge.yaml chart),
# checked when it is created and on each deploy.
# Input: {"service", "repo", "host", "name", "version",
# "allowed_repos", "actor": {kind, subject}}, where repo is the chart
# repository as forge.yaml has it ("oci://ghcr.io/stefanprodan/charts"),
# host its host, and allowed_repos the platform's list
# (--allowed-chart-repos; empty allows any).
# A message in deny refuses the chart.
#
# A missing field reads as its fallback, which these rules refuse.
package forge.charts

import rego.v1

field(obj, key, fallback) := object.get(obj, key, fallback) if {
	is_object(obj)
} else := fallback

repo := field(input, "repo", "")

host := field(input, "host", "")

name := field(input, "name", "the chart")

allowed := field(input, "allowed_repos", [])

hostname := split(host, ":")[0]

deny contains msg if {
	is_array(allowed)
	count(allowed) > 0
	not repo in allowed
	msg := sprintf("%s from %s: charts may only come from %s", [name, repo, concat(", ", allowed)])
}

# A repository named by an address or a one-word name (localhost, an IP) is
# inside the platform's network: only one the platform allows by name.
deny contains msg if {
	local_host
	not repo in allowed
	msg := sprintf("%s from %s: the host %s is an address or a local name; the platform must allow the repository by name", [name, repo, host])
}

local_host if not contains(hostname, ".")

local_host if regex.match(`^[0-9.]+$`, hostname)
