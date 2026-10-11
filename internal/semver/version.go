// Package semver works out a service's version tags: a release candidate
// for every build that reaches staging, a final version for what reaches
// production.
package semver

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
)

// Version is "vMAJOR.MINOR.PATCH" (RC is 0) or its candidate
// "vMAJOR.MINOR.PATCH-rc.RC".
type Version struct {
	Major, Minor, Patch int
	RC                  int
}

// Bump is which part of the version a final release raises.
type Bump int

// The bumps; a final release is a patch unless someone picks more.
const (
	Patch Bump = iota
	Minor
	Major
)

var tagRE = regexp.MustCompile(`^v(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:-rc\.([1-9]\d{0,8}))?$`)

// Parse reads a tag Forge wrote; ok is false for any other tag.
func Parse(tag string) (Version, bool) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.RC, _ = strconv.Atoi(m[4])
	}
	return v, true
}

func (v Version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.RC > 0 {
		s += fmt.Sprintf("-rc.%d", v.RC)
	}
	return s
}

// Final reports whether v is a final version, not a candidate.
func (v Version) Final() bool { return v.RC == 0 }

// Base is v without its candidate number: the final version it leads to.
func (v Version) Base() Version { return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch} }

// Compare orders versions as semver does: a candidate comes before its
// final version.
func Compare(a, b Version) int {
	if c := cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch)); c != 0 {
		return c
	}
	switch {
	case a.RC == b.RC:
		return 0
	case a.RC == 0:
		return 1
	case b.RC == 0:
		return -1
	}
	return cmp.Compare(a.RC, b.RC)
}

// Highest is the highest of vs; ok is false when vs is empty.
func Highest(vs []Version) (Version, bool) {
	var top Version
	for i, v := range vs {
		if i == 0 || Compare(v, top) > 0 {
			top = v
		}
	}
	return top, len(vs) > 0
}

// LastFinal is the highest final version in all.
func LastFinal(all []Version) (Version, bool) {
	var finals []Version
	for _, v := range all {
		if v.Final() {
			finals = append(finals, v)
		}
	}
	return Highest(finals)
}

// NextFinal is the final version that follows all: the last final version
// raised by bump. A service's first release is v0.1.0, or v1.0.0 for a major.
func NextFinal(all []Version, bump Bump) Version {
	last, ok := LastFinal(all)
	switch {
	case bump == Major:
		return Version{Major: last.Major + 1}
	case bump == Minor || !ok:
		return Version{Major: last.Major, Minor: last.Minor + 1}
	}
	return Version{Major: last.Major, Minor: last.Minor, Patch: last.Patch + 1}
}

// NextCandidate is the next candidate for the patch release that follows
// all: "-rc.1" for the first build after a final version, counting up.
func NextCandidate(all []Version) Version {
	next := NextFinal(all, Patch)
	for _, v := range all {
		if v.Base() == next.Base() && v.RC >= next.RC {
			next.RC = v.RC
		}
	}
	next.RC++
	return next
}

// Previous is the version release notes for v start from: the last final
// version below v, or for a candidate the candidate before it. ok is false
// for a service's first version.
func Previous(all []Version, v Version) (Version, bool) {
	var below []Version
	for _, o := range all {
		if Compare(o, v) < 0 && (o.Final() || (!v.Final() && o.Base() == v.Base())) {
			below = append(below, o)
		}
	}
	return Highest(below)
}
