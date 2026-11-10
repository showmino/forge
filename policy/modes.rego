# The modes of the rules an admin may switch (phase 15, milestone D).
# A switchable rule reports a violation tagged with its id; its package
# turns the violation into a deny (enforce), a warning (warn) or nothing
# (off) with mode below. The modes come from input.modes, set by Forge from
# its database; a rule not there has its default. The other rules guard
# Forge itself and are always denies.
package forge.modes

import rego.v1

# defaults are the switchable rules and their modes when nobody changed them.
# The list in policy.go (Rules) must name the same rules.
defaults := {
	"images.pinned-version": "enforce",
	"images.allowed-registries": "enforce",
	"charts.allowed-repos": "enforce",
	"promotion.healthy-staging": "enforce",
	"resources.storage-limit": "enforce",
	"resources.production-ha": "enforce",
	"resources.instance-limit": "enforce",
	"resources.staging-single": "warn",
}

# mode is the rule's mode: input.modes when it holds a known mode for it,
# else the default. A rule that is not switchable is enforced.
mode(rule) := m if {
	m := input.modes[rule]
	m in {"enforce", "warn", "off"}
	defaults[rule]
} else := defaults[rule] if {
	defaults[rule]
} else := "enforce"
