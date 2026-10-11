package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

// githubApp returns the GitHub App forge-api acts as, or nil when none is
// configured. Errors never include the key file's content.
func githubApp(id, keyFile, org string) (*github.App, error) {
	switch {
	case id == "" && keyFile == "":
		return nil, nil
	case id == "" || keyFile == "":
		return nil, errors.New("set both --github-app-id and --github-app-key-file")
	case org == "":
		return nil, errors.New("--github-org is required with a GitHub App")
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("--github-app-id %q is not an App ID", id)
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read --github-app-key-file: %w", err)
	}
	key, err := github.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("--github-app-key-file %s: %w", keyFile, err)
	}
	return &github.App{ID: n, Key: key, Org: org}, nil
}

// teamSource lists the organization's teams as the App; without an App there
// is none, and TeamService answers FAILED_PRECONDITION.
func teamSource(app *github.App) server.TeamSource {
	if app == nil {
		return nil
	}
	return &github.Client{Token: app.Token}
}

// templateSource picks where the API reads templates from: a local checkout
// (development) or the GitHub repo as the App. It returns nil when neither is
// configured; TemplateService then answers FAILED_PRECONDITION.
func templateSource(dir, repo string, app *github.App) (server.TemplateSource, error) {
	switch {
	case dir != "":
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("--templates-dir %s is not a directory", dir)
		}
		return server.DirSource{Dir: dir}, nil
	case app != nil:
		return server.NewGitHubSource(&github.Client{Token: app.Token}, repo)
	default:
		return nil, nil
	}
}

// repoHost returns where new services' repos are created: GitHub as the App,
// or nil (an untyped nil interface) when no App is configured.
func repoHost(app *github.App) server.RepoHost {
	if app == nil {
		return nil
	}
	return &github.Client{Token: app.Token}
}
