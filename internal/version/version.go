// Package version holds build information set at link time by GoReleaser:
//
//	-ldflags "-X github.com/showmino/forge/internal/version.Version=1.2.3 ..."
package version

import (
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

var (
	// Version is the semantic version without a leading "v". "dev" for local builds.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = ""
	// Date is the build date in RFC 3339.
	Date = ""
)

// Info is the build information of this binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
}

// Get returns build information, falling back to the module build info for
// binaries built with "go install".
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	if info.Version != "dev" {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := moduleVersion(bi.Main.Version); v != "" {
			info.Version = v
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			}
		}
	}
	return info
}

// pseudoVersion matches the timestamp-and-commit suffix of a Go module
// pseudo-version, such as v0.0.0-20261007221236-dc5438b5aeac(+dirty).
var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}(\+.*)?$`)

// moduleVersion returns the release version from a module build version, or
// "" when there is none: "(devel)", or the pseudo-version Go 1.24+ stamps on
// 'go build' in a git checkout. Those stay "dev" so they are never blocked.
func moduleVersion(v string) string {
	if v == "" || v == "(devel)" || pseudoVersion.MatchString(v) {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// Compare compares two "MAJOR.MINOR.PATCH" versions (a leading "v" and any
// pre-release suffix are ignored). It returns -1, 0 or 1. Unparseable versions,
// such as "dev", compare as newer than everything so local builds are never blocked.
func Compare(a, b string) int {
	pa, okA := parse(a)
	pb, okB := parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return 1
	case !okB:
		return -1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
