# Rules for resource plans.
# Input: {"plan": <the output of tofu show -json>, "forge": {service,
# environment, resources, actor, allow_destroy}}.
# A message in deny refuses the plan; a message in warn is shown with it.
package forge.resources

import rego.v1

# The most storage a database may ask for, in Gi, per environment.
max_storage_gi := {"staging": 20, "production": 50}

# The most instances a database may have.
max_instances := 3

# changes are the plan's resource changes that do something.
changes contains rc if {
	some rc in input.plan.resource_changes
	some action in rc.change.actions
	not action in {"no-op", "read"}
}

# manifest is the Kubernetes object a change ends with, or the one it starts
# from when the change deletes it.
manifest(rc) := rc.change.after.manifest if {
	rc.change.after != null
} else := rc.change.before.manifest

is_database(rc) if {
	rc.type == "kubernetes_manifest"
	m := manifest(rc)
	m.apiVersion == "postgresql.cnpg.io/v1"
	m.kind == "Cluster"
}

gi(size) := to_number(trim_suffix(size, "Gi"))

namespace := sprintf("%s-%s", [input.forge.service, input.forge.environment])

# readable is true for a database whose size and instance count the rules can read.
readable(m) if {
	regex.match(`^[1-9][0-9]*Gi$`, m.spec.storage.size)
	is_number(m.spec.instances)
}

# Only databases, and only in the service's own namespace. This guards the
# platform: if the module or the runner ever tried to change something else,
# the plan is refused.
deny contains msg if {
	some rc in changes
	not is_database(rc)
	msg := sprintf("the plan changes %s, which is not a CloudNativePG cluster", [rc.address])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	ns := manifest(rc).metadata.namespace
	ns != namespace
	msg := sprintf("the plan changes a database in namespace %s, not %s", [ns, namespace])
}

# A database the rules cannot read is refused, never waved through.
deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	not readable(rc.change.after.manifest)
	msg := sprintf("the plan's database %s has no readable storage size or instance count", [rc.address])
}

deny contains msg if {
	some rc in changes
	"delete" in rc.change.actions
	not input.forge.allow_destroy
	msg := sprintf("the plan deletes database %s; plan again with --allow-destroy to remove it", [manifest(rc).metadata.name])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	size := gi(rc.change.after.manifest.spec.storage.size)
	limit := max_storage_gi[input.forge.environment]
	size > limit
	msg := sprintf("storage %dGi is above the %dGi limit for %s", [size, limit, input.forge.environment])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.before != null
	rc.change.after != null
	was := gi(rc.change.before.manifest.spec.storage.size)
	now := gi(rc.change.after.manifest.spec.storage.size)
	now < was
	msg := sprintf("storage cannot shrink from %dGi to %dGi", [was, now])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	input.forge.environment == "production"
	n := rc.change.after.manifest.spec.instances
	n < 2
	msg := sprintf("production needs at least 2 instances, not %d", [n])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	n := rc.change.after.manifest.spec.instances
	n > max_instances
	msg := sprintf("%d instances is above the limit of %d", [n, max_instances])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	class := object.get(rc.change.after.manifest.spec.storage, "storageClass", "")
	class != "longhorn-retain"
	msg := sprintf("storage class %q is not longhorn-retain", [class])
}

warn contains msg if {
	some rc in changes
	is_database(rc)
	rc.change.after != null
	input.forge.environment == "staging"
	n := rc.change.after.manifest.spec.instances
	n > 1
	msg := sprintf("staging has %d instances; 1 is usually enough", [n])
}
