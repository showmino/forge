package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/version"
)

// TemplateSource hands out the template repo's tree by version.
type TemplateSource interface {
	// Versions returns the available versions, newest first.
	Versions(ctx context.Context) ([]string, error)
	// Fetch returns the repo tree at version, or an error wrapping
	// ErrVersionNotFound when there is no such version.
	Fetch(ctx context.Context, version string) (fs.FS, error)
}

// ErrVersionNotFound means a template version does not exist.
var ErrVersionNotFound = errors.New("template version not found")

// DirSource serves a local checkout of the template repo as the single
// version "dev", for development.
type DirSource struct{ Dir string }

// Versions implements TemplateSource.
func (DirSource) Versions(context.Context) ([]string, error) { return []string{"dev"}, nil }

// Fetch implements TemplateSource.
func (s DirSource) Fetch(_ context.Context, v string) (fs.FS, error) {
	if v != "dev" {
		return nil, fmt.Errorf("%w: %s (a template directory only serves \"dev\")", ErrVersionNotFound, v)
	}
	return os.DirFS(s.Dir), nil
}

// tagTTL is how long the tag list is reused, so a new release shows up
// within a minute without a request to GitHub on every call.
const tagTTL = time.Minute

var semverTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// GitHubSource serves the semver tags of a GitHub repo. Each tag's tree is
// downloaded once and kept in memory: tags do not change.
type GitHubSource struct {
	client      *github.Client
	owner, repo string
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time

	mu     sync.Mutex
	tags   []string
	tagsAt time.Time
	trees  map[string]fs.FS
}

// NewGitHubSource returns a source for repo, "owner/name".
func NewGitHubSource(c *github.Client, repo string) (*GitHubSource, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("template repo %q must be owner/name", repo)
	}
	return &GitHubSource{client: c, owner: owner, repo: name, Now: time.Now, trees: map[string]fs.FS{}}, nil
}

// Versions implements TemplateSource.
func (s *GitHubSource) Versions(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs, err := s.versionsLocked(ctx)
	return slices.Clone(vs), err
}

func (s *GitHubSource) versionsLocked(ctx context.Context) ([]string, error) {
	if s.tags != nil && s.Now().Sub(s.tagsAt) < tagTTL {
		return s.tags, nil
	}
	all, err := s.client.ListTags(ctx, s.owner, s.repo)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s/%s: %w", s.owner, s.repo, err)
	}
	tags := []string{}
	for _, t := range all {
		if semverTag.MatchString(t) {
			tags = append(tags, t)
		}
	}
	slices.SortFunc(tags, func(a, b string) int { return version.Compare(b, a) })
	s.tags, s.tagsAt = tags, s.Now()
	return tags, nil
}

// Fetch implements TemplateSource.
func (s *GitHubSource) Fetch(ctx context.Context, v string) (fs.FS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tree, ok := s.trees[v]; ok {
		return tree, nil
	}
	tags, err := s.versionsLocked(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(tags, v) {
		return nil, fmt.Errorf("%w: %s", ErrVersionNotFound, v)
	}
	body, err := s.client.Tarball(ctx, s.owner, s.repo, v)
	if err != nil {
		return nil, fmt.Errorf("download %s/%s@%s: %w", s.owner, s.repo, v, err)
	}
	defer body.Close()
	tree, err := untar(body)
	if err != nil {
		return nil, fmt.Errorf("%s/%s@%s: %w", s.owner, s.repo, v, err)
	}
	s.trees[v] = tree
	return tree, nil
}
