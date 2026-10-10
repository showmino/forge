# Rules for resource plans.
# Input: {"plan": <the output of tofu show -json>, "forge": {service,
# environment, resources, actor, allow_destroy}}.
# A message in deny refuses the plan; a message in warn is shown with it.
#
# In Rego a rule whose body reads a missing field does not fire. So every
# rule here reads optional fields with object.get and a default, and a
# change the rules cannot read is refused by a rule of its own.
package forge.resources

import rego.v1

# The most storage a database may ask for, in Gi, per environment.
max_storage_gi := {"staging": 20, "production": 50}

# The most instances a database may have.
max_instances := 3

address(rc) := object.get(rc, "address", "an unnamed resource")

actions(rc) := object.get(object.get(rc, "change", {}), "actions", null)

# side is one end of a change, "before" or "after": null when it is absent.
side(rc, name) := object.get(object.get(rc, "change", {}), name, null)

# A change is idle when its actions say it does nothing.
idle(rc) if {
	acts := actions(rc)
	is_array(acts)
	count(acts) > 0
	every a in acts {
		a in {"no-op", "read"}
	}
}

# changes are the plan's resource changes that do something, or that the
# rules cannot tell do nothing.
changes contains rc if {
	some rc in object.get(input.plan, "resource_changes", [])
	not idle(rc)
}

# sides are the ends of a change that exist: one for a create or a delete,
# two for an update or a replace.
sides(rc) := [s | some name in ["before", "after"]; s := side(rc, name); s != null]

is_cluster(s) if {
	m := object.get(s, "manifest", {})
	object.get(m, "apiVersion", "") == "postgresql.cnpg.io/v1"
	object.get(m, "kind", "") == "Cluster"
}

# A change is a database change when it is a kubernetes_manifest and every
# end of it that exists is a CloudNativePG cluster.
is_database(rc) if {
	object.get(rc, "type", "") == "kubernetes_manifest"
	count(sides(rc)) > 0
	every s in sides(rc) {
		is_cluster(s)
	}
}

deletes(rc) if "delete" in actions(rc)

gi(size) := to_number(trim_suffix(size, "Gi"))

namespace := sprintf("%s-%s", [input.forge.service, input.forge.environment])

# readable is true for a cluster whose size and instance count the rules can read.
readable(s) if {
	regex.match(`^[1-9][0-9]*Gi$`, s.manifest.spec.storage.size)
	is_number(s.manifest.spec.instances)
}

name(s) := object.get(object.get(object.get(s, "manifest", {}), "metadata", {}), "name", "")

# The rules know the limits of two environments. Any other is refused.
deny contains msg if {
	not max_storage_gi[input.forge.environment]
	msg := sprintf("the rules have no limits for environment %q", [object.get(input.forge, "environment", "")])
}

# A change whose actions cannot be read is refused.
deny contains msg if {
	some rc in changes
	acts := actions(rc)
	not valid_actions(acts)
	msg := sprintf("the plan's change to %s has no readable actions", [address(rc)])
}

valid_actions(acts) if {
	is_array(acts)
	count(acts) > 0
}

# Only databases, and only in the service's own namespace. This guards the
# platform: if the module or the runner ever tried to change something else,
# the plan is refused.
deny contains msg if {
	some rc in changes
	not is_database(rc)
	msg := sprintf("the plan changes %s, which is not a CloudNativePG cluster", [address(rc)])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	some s in sides(rc)
	ns := object.get(object.get(s.manifest, "metadata", {}), "namespace", "")
	ns != namespace
	msg := sprintf("the plan changes a database in namespace %q, not %s", [ns, namespace])
}

# A database the rules cannot read is refused, never waved through. On an
# update both ends must be readable, or a shrink could hide.
deny contains msg if {
	some rc in changes
	is_database(rc)
	some s in sides(rc)
	not deleted_side(rc, s)
	not readable(s)
	msg := sprintf("the plan's database %s has no readable storage size or instance count", [address(rc)])
}

# The before of a pure delete need not be readable: it only has to go.
deleted_side(rc, s) if {
	side(rc, "after") == null
	s == side(rc, "before")
}

deny contains msg if {
	some rc in changes
	deletes(rc)
	not input.forge.allow_destroy
	msg := sprintf("the plan deletes database %s; plan again with --allow-destroy to remove it", [deleted_name(rc)])
}

# deleted_name names what a delete removes: the cluster's name, or the
# plan address when the name cannot be read.
deleted_name(rc) := n if {
	n := name(side(rc, "before"))
	n != ""
} else := address(rc)

after(rc) := s if {
	is_database(rc)
	s := side(rc, "after")
	s != null
	readable(s)
}

deny contains msg if {
	some rc in changes
	s := after(rc)
	size := gi(s.manifest.spec.storage.size)
	limit := max_storage_gi[input.forge.environment]
	size > limit
	msg := sprintf("storage %dGi is above the %dGi limit for %s", [size, limit, input.forge.environment])
}

deny contains msg if {
	some rc in changes
	s := after(rc)
	b := side(rc, "before")
	b != null
	readable(b)
	was := gi(b.manifest.spec.storage.size)
	now := gi(s.manifest.spec.storage.size)
	now < was
	msg := sprintf("storage cannot shrink from %dGi to %dGi", [was, now])
}

deny contains msg if {
	some rc in changes
	s := after(rc)
	input.forge.environment == "production"
	n := s.manifest.spec.instances
	n < 2
	msg := sprintf("production needs at least 2 instances, not %v", [n])
}

deny contains msg if {
	some rc in changes
	s := after(rc)
	n := s.manifest.spec.instances
	n < 1
	msg := sprintf("%v instances is below the minimum of 1", [n])
}

deny contains msg if {
	some rc in changes
	s := after(rc)
	n := s.manifest.spec.instances
	n > max_instances
	msg := sprintf("%v instances is above the limit of %d", [n, max_instances])
}

deny contains msg if {
	some rc in changes
	is_database(rc)
	s := side(rc, "after")
	s != null
	class := object.get(object.get(object.get(s.manifest, "spec", {}), "storage", {}), "storageClass", "")
	class != "longhorn-retain"
	msg := sprintf("storage class %q is not longhorn-retain", [class])
}

warn contains msg if {
	some rc in changes
	s := after(rc)
	input.forge.environment == "staging"
	n := s.manifest.spec.instances
	n > 1
	msg := sprintf("staging has %v instances; 1 is usually enough", [n])
}
