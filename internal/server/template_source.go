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

// ErrInvalidRelease means a release exists but cannot be served (for
// example its tarball breaks the extraction rules): retrying will not help.
var ErrInvalidRelease = errors.New("the template release is invalid")

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

// tagRetry is how soon a failed tag refresh is retried while the last known
// tags are served.
const tagRetry = 10 * time.Second

// GitHubSource serves the semver tags of a GitHub repo. Each tag's tree is
// downloaded once and kept in memory: tags do not change. No lock is held
// while talking to GitHub, so a slow download stalls only its own callers.
type GitHubSource struct {
	client      *github.Client
	owner, repo string
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time

	mu       sync.Mutex
	tags     []string
	tagsAt   time.Time
	trees    map[string]fs.FS
	inflight map[string]*download
}

// download is one tarball fetch that concurrent callers share.
type download struct {
	done chan struct{}
	tree fs.FS
	err  error
}

// NewGitHubSource returns a source for repo, "owner/name".
func NewGitHubSource(c *github.Client, repo string) (*GitHubSource, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("template repo %q must be owner/name", repo)
	}
	return &GitHubSource{
		client: c, owner: owner, repo: name, Now: time.Now,
		trees: map[string]fs.FS{}, inflight: map[string]*download{},
	}, nil
}

// Versions implements TemplateSource. While GitHub is failing it serves the
// last known tags and retries every tagRetry.
func (s *GitHubSource) Versions(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	if s.tags != nil && s.Now().Sub(s.tagsAt) < tagTTL {
		tags := slices.Clone(s.tags)
		s.mu.Unlock()
		return tags, nil
	}
	s.mu.Unlock()

	all, err := s.client.ListTags(ctx, s.owner, s.repo)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.tags != nil {
			s.tagsAt = s.Now().Add(tagRetry - tagTTL)
			return slices.Clone(s.tags), nil
		}
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
	return slices.Clone(tags), nil
}

// Fetch implements TemplateSource. Concurrent callers of one uncached tag
// share a single download; each waits only as long as its own context allows.
func (s *GitHubSource) Fetch(ctx context.Context, v string) (fs.FS, error) {
	s.mu.Lock()
	tree, cached := s.trees[v]
	d, running := s.inflight[v]
	s.mu.Unlock()
	if cached {
		return tree, nil
	}
	if !running {
		tags, err := s.Versions(ctx)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(tags, v) {
			return nil, fmt.Errorf("%w: %s", ErrVersionNotFound, v)
		}
		s.mu.Lock()
		if tree, ok := s.trees[v]; ok {
			s.mu.Unlock()
			return tree, nil
		}
		if d, running = s.inflight[v]; !running {
			d = &download{done: make(chan struct{})}
			s.inflight[v] = d
			// The download outlives the caller that started it: others may be
			// waiting for it. The HTTP client's timeout bounds it.
			go s.download(context.WithoutCancel(ctx), v, d)
		}
		s.mu.Unlock()
	}
	select {
	case <-d.done:
		return d.tree, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *GitHubSource) download(ctx context.Context, v string, d *download) {
	tree, err := s.fetchTree(ctx, v)
	s.mu.Lock()
	if err == nil {
		s.trees[v] = tree
	}
	delete(s.inflight, v)
	s.mu.Unlock()
	d.tree, d.err = tree, err
	close(d.done)
}

func (s *GitHubSource) fetchTree(ctx context.Context, v string) (fs.FS, error) {
	body, err := s.client.Tarball(ctx, s.owner, s.repo, v)
	if err != nil {
		return nil, fmt.Errorf("download %s/%s@%s: %w", s.owner, s.repo, v, err)
	}
	defer body.Close()
	tree, err := untar(body)
	if err != nil {
		return nil, fmt.Errorf("%s/%s@%s: %w: %w", s.owner, s.repo, v, ErrInvalidRelease, err)
	}
	return tree, nil
}
