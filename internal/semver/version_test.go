package semver

import "testing"

func parseAll(t *testing.T, tags ...string) []Version {
	t.Helper()
	var out []Version
	for _, tag := range tags {
		v, ok := Parse(tag)
		if !ok {
			t.Fatalf("Parse(%q) failed", tag)
		}
		out = append(out, v)
	}
	return out
}

func TestParse(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.20.3", "v0.3.1-rc.2", "v10.0.0-rc.11"} {
		v, ok := Parse(tag)
		if !ok || v.String() != tag {
			t.Errorf("Parse(%q) = %v, %v", tag, v, ok)
		}
	}
	for _, tag := range []string{"", "0.1.0", "v1.2", "v1.2.3.4", "v01.2.3", "v1.2.3-rc.0", "v1.2.3-rc1", "v1.2.3-beta.1", "v1.2.3+build", "v1.2.3\n", "release-1", "v99999999999.0.0"} {
		if v, ok := Parse(tag); ok {
			t.Errorf("Parse(%q) = %v, want not ok", tag, v)
		}
	}
}

func TestCompare(t *testing.T) {
	order := parseAll(t, "v0.1.0-rc.1", "v0.1.0-rc.2", "v0.1.0", "v0.1.1-rc.1", "v0.2.0", "v0.10.0", "v1.0.0-rc.9", "v1.0.0")
	for i := range order {
		for j := range order {
			got, want := Compare(order[i], order[j]), 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%v, %v) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestNext(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		tags                           []string
		candidate, patch, minor, major string
	}{
		{"nothing yet", nil, "v0.1.0-rc.1", "v0.1.0", "v0.1.0", "v1.0.0"},
		{"only candidates", []string{"v0.1.0-rc.1", "v0.1.0-rc.2"}, "v0.1.0-rc.3", "v0.1.0", "v0.1.0", "v1.0.0"},
		{"after a final", []string{"v0.1.0-rc.1", "v0.1.0"}, "v0.1.1-rc.1", "v0.1.1", "v0.2.0", "v1.0.0"},
		{"candidates since", []string{"v0.3.0", "v0.3.1-rc.1", "v0.3.1-rc.4", "v0.2.9"}, "v0.3.1-rc.5", "v0.3.1", "v0.4.0", "v1.0.0"},
		{"a minor was picked over patch candidates", []string{"v0.3.0", "v0.3.1-rc.1", "v0.4.0"}, "v0.4.1-rc.1", "v0.4.1", "v0.5.0", "v1.0.0"},
		{"after a major", []string{"v1.0.0", "v0.9.3"}, "v1.0.1-rc.1", "v1.0.1", "v1.1.0", "v2.0.0"},
	} {
		all := parseAll(t, tc.tags...)
		if got := NextCandidate(all).String(); got != tc.candidate {
			t.Errorf("%s: NextCandidate = %s, want %s", tc.name, got, tc.candidate)
		}
		for bump, want := range map[Bump]string{Patch: tc.patch, Minor: tc.minor, Major: tc.major} {
			if got := NextFinal(all, bump).String(); got != want {
				t.Errorf("%s: NextFinal(%d) = %s, want %s", tc.name, bump, got, want)
			}
		}
	}
}

func TestPrevious(t *testing.T) {
	all := parseAll(t, "v0.1.0-rc.1", "v0.1.0", "v0.1.1-rc.1", "v0.1.1-rc.2", "v0.2.0")
	for _, tc := range []struct{ of, want string }{
		{"v0.1.0-rc.1", ""},
		{"v0.1.0", ""},
		{"v0.1.1-rc.1", "v0.1.0"},
		{"v0.1.1-rc.2", "v0.1.1-rc.1"},
		{"v0.2.0", "v0.1.0"}, // a final's notes cover everything since the last final
		{"v0.2.1-rc.1", "v0.2.0"},
	} {
		v, _ := Parse(tc.of)
		got := ""
		if p, ok := Previous(all, v); ok {
			got = p.String()
		}
		if got != tc.want {
			t.Errorf("Previous(%s) = %q, want %q", tc.of, got, tc.want)
		}
	}
}
