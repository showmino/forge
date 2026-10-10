# Rules for resource plans.
# Input: {"plan": <the output of tofu show -json>, "forge": {service,
# environment, resources, actor, allow_destroy}}.
# A message in deny refuses the plan; a message in warn is shown with it.
#
# In Rego a rule whose body reads a missing field, or a field of the wrong
# type, does not fire. So every rule here reads optional fields with field
# and a fallback, and a change the rules cannot read is refused by a rule
# of its own.
package forge.resources

import rego.v1

# The most storage a database may ask for, in Gi, per environment.
max_storage_gi := {"staging": 20, "production": 50}

# The most instances a database may have.
max_instances := 3

# field reads obj[key], or fallback when obj is not an object or has no such
# key. object.get alone is undefined on a string or a number, which would
# silence the rule that calls it.
field(obj, key, fallback) := object.get(obj, key, fallback) if {
	is_object(obj)
} else := fallback

address(rc) := field(rc, "address", "an unnamed resource")

actions(rc) := field(field(rc, "change", {}), "actions", null)

# side is one end of a change, "before" or "after": null when it is absent.
side(rc, name) := field(field(rc, "change", {}), name, null)

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
	some rc in resource_changes
	not idle(rc)
}

resource_changes := field(input.plan, "resource_changes", [])

# sides are the ends of a change that exist: one for a create or a delete,
# two for an update or a replace.
sides(rc) := [s | some name in ["before", "after"]; s := side(rc, name); s != null]

is_cluster(s) if {
	m := field(s, "manifest", {})
	field(m, "apiVersion", "") == "postgresql.cnpg.io/v1"
	field(m, "kind", "") == "Cluster"
}

# A change is a database change when it is a kubernetes_manifest and every
# end of it that exists is a CloudNativePG cluster.
is_database(rc) if {
	field(rc, "type", "") == "kubernetes_manifest"
	count(sides(rc)) > 0
	every s in sides(rc) {
		is_cluster(s)
	}
}

deletes(rc) if "delete" in actions(rc)

gi(size) := to_number(trim_suffix(size, "Gi"))

# What forge-api says about the request. Each is read with a fallback, so a
# missing key cannot silence a rule.
forge := field(input, "forge", {})

service := field(forge, "service", "")

environment := field(forge, "environment", "")

declared := field(forge, "resources", [])

allow_destroy if field(forge, "allow_destroy", false) == true

namespace := sprintf("%s-%s", [service, environment])

# The actions a change may have. Anything else (OpenTofu's "forget", for
# one) is refused until a rule knows what it means.
known_actions := {"create", "update", "delete", "no-op", "read"}

# readable is true for a cluster whose size and instance count the rules can read.
readable(s) if {
	regex.match(`^[1-9][0-9]*Gi$`, s.manifest.spec.storage.size)
	is_number(s.manifest.spec.instances)
}

name(s) := field(field(field(s, "manifest", {}), "metadata", {}), "name", "")

# The rules know the limits of two environments. Any other is refused.
deny contains msg if {
	not max_storage_gi[environment]
	msg := sprintf("the rules have no limits for environment %v", [json.marshal(environment)])
}

# The list of changes itself must be a list.
deny contains "the plan's resource_changes is not a list" if not is_array(resource_changes)

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

deny contains msg if {
	some rc in changes
	acts := actions(rc)
	valid_actions(acts)
	some a in acts
	not a in known_actions
	msg := sprintf("the plan's change to %s has an action the rules do not know: %v", [address(rc), json.marshal(a)])
}

# A change that leaves nothing behind must say that it deletes.
deny contains msg if {
	some rc in changes
	side(rc, "after") == null
	not deletes(rc)
	msg := sprintf("the plan's change to %s leaves nothing behind but is not a delete", [address(rc)])
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
	ns := field(field(field(s, "manifest", {}), "metadata", {}), "namespace", "")
	ns != namespace
	msg := sprintf("the plan changes a database in namespace %v, not %s", [json.marshal(ns), namespace])
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
	not allow_destroy
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
	limit := max_storage_gi[environment]
	size > limit
	msg := sprintf("storage %dGi is above the %dGi limit for %s", [size, limit, environment])
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
	environment == "production"
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
	class := field(field(field(field(s, "manifest", {}), "spec", {}), "storage", {}), "storageClass", "")
	class != "longhorn-retain"
	msg := sprintf("storage class %v is not longhorn-retain", [json.marshal(class)])
}

# What a plan makes must be what forge.yaml declares: the same database, with
# the same size and instance count.
deny contains msg if {
	some rc in changes
	s := after(rc)
	not declared_as(s)
	msg := sprintf("the plan makes database %v with %s and %v instances, which is not what forge.yaml declares", [json.marshal(name(s)), s.manifest.spec.storage.size, s.manifest.spec.instances])
}

declared_as(s) if {
	some d in declared
	field(d, "type", "") == "postgres"
	field(d, "name", "") == name(s)
	field(d, "storage_gi", -1) == gi(s.manifest.spec.storage.size)
	field(d, "instances", -1) == s.manifest.spec.instances
}

warn contains msg if {
	some rc in changes
	s := after(rc)
	environment == "staging"
	n := s.manifest.spec.instances
	n > 1
	msg := sprintf("staging has %v instances; 1 is usually enough", [n])
}
