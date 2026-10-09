package server

import (
	"net/http"
	"path/filepath"
	"regexp"
)

// cliNameRE names the CLI builds forge-api serves, so CI jobs on the LAN can
// fetch a CLI that matches the API without a token for the private forge repo.
var cliNameRE = regexp.MustCompile(`^forge-linux-(amd64|arm64)$`)

// cliDownload serves dir/<name> for the names cliNameRE allows; anything
// else, or no dir, is 404. The binaries are not secret, so there is no auth.
func cliDownload(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if dir == "" || !cliNameRE.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		//nolint:gosec // name matched cliNameRE above: no separators, so no traversal.
		http.ServeFile(w, r, filepath.Join(dir, name))
	}
}
