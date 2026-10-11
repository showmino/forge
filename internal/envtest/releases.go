package envtest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/showmino/forge/internal/github"
)

// Releases is a repo's tags and GitHub releases in memory; it is what
// forge-api cuts versions with (server.ReleaseGitHub).
type Releases struct {
	mu       sync.Mutex
	tags     []github.Tag
	releases []github.Release
	// FailTag and FailRelease, when set, are what creating one answers.
	FailTag, FailRelease error
	// Race is added just before the next CreateTag, as if someone else
	// tagged at the same moment.
	Race *github.Tag
}

// ListTagCommits implements server.ReleaseGitHub.
func (r *Releases) ListTagCommits(_ context.Context, _, _ string) ([]github.Tag, error) {
	return r.Tags(), nil
}

// CreateTag implements server.ReleaseGitHub.
func (r *Releases) CreateTag(_ context.Context, _, _, tag, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FailTag != nil {
		return r.FailTag
	}
	if r.Race != nil {
		r.tags, r.Race = append(r.tags, *r.Race), nil
	}
	for _, t := range r.tags {
		if t.Name == tag {
			return fmt.Errorf("%w: %s", github.ErrTagExists, tag)
		}
	}
	r.tags = append(r.tags, github.Tag{Name: tag, Commit: sha})
	return nil
}

// ReleaseExists implements server.ReleaseGitHub.
func (r *Releases) ReleaseExists(_ context.Context, _, _, tag string) (bool, error) {
	_, ok := r.Release(tag)
	return ok, nil
}

// CreateRelease implements server.ReleaseGitHub.
func (r *Releases) CreateRelease(_ context.Context, _, _ string, rel github.Release) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FailRelease != nil {
		return r.FailRelease
	}
	r.releases = append(r.releases, rel)
	return nil
}

// Tags is the tags in the order they were made.
func (r *Releases) Tags() []github.Tag {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]github.Tag(nil), r.tags...)
}

// Names lists the tags as "tag@commit7", in the order they were made.
func (r *Releases) Names() string {
	var out []string
	for _, t := range r.Tags() {
		out = append(out, t.Name+"@"+t.Commit[:min(7, len(t.Commit))])
	}
	return strings.Join(out, " ")
}

// Release is the release made for tag.
func (r *Releases) Release(tag string) (github.Release, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rel := range r.releases {
		if rel.Tag == tag {
			return rel, true
		}
	}
	return github.Release{}, false
}

// Fail sets what creating a tag and creating a release answer from now on.
func (r *Releases) Fail(tag, release error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FailTag, r.FailRelease = tag, release
}
