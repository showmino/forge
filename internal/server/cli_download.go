package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/showmino/forge/internal/github"
)

// cliNameRE names the CLI builds forge-api serves, so CI jobs on the LAN can
// fetch a CLI that matches the API without a token for the private forge repo.
var cliNameRE = regexp.MustCompile(`^forge-linux-(amd64|arm64)$`)

// CLIReleases reads assets of the forge repo's GitHub releases.
type CLIReleases interface {
	ReleaseAsset(ctx context.Context, owner, repo, tag, name string) (io.ReadCloser, error)
}

var (
	releaseTagRE   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	releaseAssetRE = regexp.MustCompile(`^(checksums\.txt|forge_\d+\.\d+\.\d+_(linux|darwin)_(amd64|arm64)\.tar\.gz|forge_\d+\.\d+\.\d+_windows_(amd64|arm64)\.zip)$`)
)

// cliRelease serves GET /cli/releases/{tag}/{file}: the CLI archives and
// checksums of a forge release, for 'forge upgrade' on any OS without a
// GitHub token. Like the Linux builds, they are not secret.
func cliRelease(src CLIReleases, repo string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tag, file := r.PathValue("tag"), r.PathValue("file")
		owner, name, ok := strings.Cut(repo, "/")
		if src == nil || !ok || !releaseTagRE.MatchString(tag) || !releaseAssetRE.MatchString(file) {
			http.NotFound(w, r)
			return
		}
		body, err := src.ReleaseAsset(r.Context(), owner, name, tag, file)
		if errors.Is(err, github.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.WarnContext(r.Context(), "cli release asset", "tag", tag, "file", file, "error", err)
			http.Error(w, "the release could not be read from GitHub", http.StatusBadGateway)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, body)
	}
}

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
