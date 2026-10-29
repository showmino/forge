# Rules for the image a service without a repo runs (forge.yaml run.image),
# checked when it is created and on each deploy.
# Input: {"service", "image", "host", "repo", "tag", "digest",
# "allowed_registries", "actor": {kind, subject}}, where image is written in
# full ("docker.io/library/nginx:1.27") and allowed_registries is the
# platform's list (policy.allowedRegistries; empty allows any).
# A message in deny refuses the image.
#
# A missing or mistyped field reads as its fallback, which these rules
# refuse: an image the rules cannot judge does not run.
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
