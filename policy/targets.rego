# Rules for the target an infrastructure repo deploys to (phase 12), checked
# when it is created.
# Input: {"service", "team", "target", "teams", "environments", "offered",
# "full", "actor": {kind, subject}}, where teams are the teams the target
# allows (empty allows every team), environments the ones asked for, offered
# the ones the target has, and full those of them with no room left
# (maxRepos).
# A message in deny refuses the create.
package forge.targets

import rego.v1

field(obj, key, fallback) := object.get(obj, key, fallback) if {
	is_object(obj)
} else := fallback

target := field(input, "target", "the target")

team := field(input, "team", "")

teams := field(input, "teams", [])

environments := field(input, "environments", [])

offered := field(input, "offered", [])

full := field(input, "full", [])

deny contains msg if {
	count(teams) > 0
	not team in teams
	msg := sprintf("target %s is for the teams %s, not %s", [target, concat(", ", teams), team])
}

deny contains msg if {
	count(environments) == 0
	msg := sprintf("choose at least one environment of target %s (%s)", [target, concat(", ", offered)])
}

deny contains msg if {
	some env in environments
	not env in offered
	msg := sprintf("target %s has no %s (it has %s)", [target, env, concat(", ", offered)])
}

deny contains msg if {
	some env in environments
	env in full
	msg := sprintf("target %s's %s has no room for another repo; ask the platform team for another target", [target, env])
}
