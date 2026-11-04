package repositories

import (
	"fmt"

	"github.com/showmino/forge/internal/manifest"
)

// Codeowners makes the service's team the sole owner of every file.
func Codeowners(org, team string) ([]byte, error) {
	if !manifest.ValidID(org) || !manifest.ValidID(team) {
		return nil, fmt.Errorf("organization %q and team %q must be GitHub slugs", org, team)
	}
	return []byte("* @" + org + "/" + team + "\n"), nil
}
