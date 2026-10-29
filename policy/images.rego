# Rules for the image a service without a repo runs (forge.yaml run.image),
# checked when it is created and on each deploy.
# Input: {"service", "image", "host", "repo", "tag", "digest",
# "allowed_registries", "service_images", "actor": {kind, subject}}, where
# image is written in full ("docker.io/library/nginx:1.27"),
# allowed_registries is the platform's list (--allowed-registries; empty
# allows any) and service_images is where the platform's services' CI pushes
# their images ("ghcr.io/my-org/").
# A message in deny refuses the image.
#
# A missing field reads as its fallback, which these rules refuse: an image
# with no tag or no host does not run. Forge fills every field from a
# parsed image name, so none has the wrong type.
package forge.images

import rego.v1

field(obj, key, fallback) := object.get(obj, key, fallback) if {
	is_object(obj)
} else := fallback

image := field(input, "image", "the image")

tag := field(input, "tag", "")

digest := field(input, "digest", "")

host := field(input, "host", "")

allowed := field(input, "allowed_registries", [])

repo := field(input, "repo", "")

# The registry's name without its port.
hostname := split(host, ":")[0]

# The last part of the name, for the example in a message: nginx.
short := name if {
	parts := split(field(input, "repo", ""), "/")
	name := parts[count(parts) - 1]
	name != ""
} else := "app"

# A tag that moves runs something else on every restart.
deny contains msg if {
	tag == "latest"
	msg := sprintf("%s: the tag latest moves; name a version, such as %s:1.27", [image, short])
}

deny contains msg if {
	tag == ""
	digest == ""
	msg := sprintf("%s names no tag; name a version, such as %s:1.27", [image, short])
}

deny contains msg if {
	is_array(allowed)
	count(allowed) > 0
	not host in allowed
	msg := sprintf("%s: images may only come from %s", [image, concat(", ", allowed)])
}

# The images the platform's services' CI builds run as those services, and
# are deployed from their repos: with the platform's pull secret, a service
# without a repo could otherwise run another team's private image.
deny contains msg if {
	prefix := field(input, "service_images", "")
	is_string(prefix)
	prefix != ""
	startswith(lower(sprintf("%s/%s/", [host, repo])), lower(prefix))
	msg := sprintf("%s is built by a service's CI, under %s; deploy it from that service's repo", [image, prefix])
}

# A registry named by an address or a one-word name (localhost, an IP) is
# inside the platform's network: only one the platform allows by name.
deny contains msg if {
	local_registry
	not host in allowed
	msg := sprintf("%s: the registry %s is an address or a local name; the platform must allow it by name", [image, host])
}

local_registry if not contains(hostname, ".")

local_registry if regex.match(`^[0-9.]+$`, hostname)
