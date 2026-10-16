# Rules for promotions: whether what runs in one environment may go to the
# next. A rollback is never checked, so an incident can always be undone.
# Input: {"service", "from", "to", "source": {sync, health, at_revision,
# commit, deploy_commit, version, image}, "actor": {kind, subject}}, where
# source is what Argo CD says about the environment promoted from.
# A message in deny refuses the promotion.
#
# A missing or mistyped field reads as its fallback, which these rules
# refuse: a promotion the rules cannot judge does not go ahead.
package forge.deploys

import rego.v1

field(obj, key, fallback) := object.get(obj, key, fallback) if {
	is_object(obj)
} else := fallback

source := field(input, "source", {})

from := field(input, "from", "the source environment")

sync := field(source, "sync", "")

health := field(source, "health", "")

shown(s) := s if {
	is_string(s)
	s != ""
} else := "unknown"

deny contains msg if {
	sync != "Synced"
	msg := sprintf("%s is %s; it must be Synced to be promoted", [from, shown(sync)])
}

deny contains msg if {
	health != "Healthy"
	msg := sprintf("%s is %s; it must be Healthy to be promoted", [from, shown(health)])
}

# Argo CD can still say Synced and Healthy about the deploy before the
# latest one: the image being promoted is then not the one that was checked.
deny contains msg if {
	sync == "Synced"
	field(source, "at_revision", false) != true
	msg := sprintf("%s does not run its latest deploy (%s) yet; wait for Argo CD to sync it", [from, shown(field(source, "deploy_commit", ""))])
}
