package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrTagExists is returned by CreateTag when the tag is already there.
var ErrTagExists = errors.New("the tag already exists")

// ErrTooManyTags is returned by ListTagCommits for a repo with more tags
// than it reads: a partial list would give wrong versions.
var ErrTooManyTags = fmt.Errorf("the repository has more than %d tags", pageSize*maxPages)

// Tag is a tag and the commit it points at.
type Tag struct {
	Name, Commit string
}

// ListTagCommits returns every tag of owner/repo with its commit.
func (c *Client) ListTagCommits(ctx context.Context, owner, repo string) ([]Tag, error) {
	var out []Tag
	for page := 1; page <= maxPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/tags?per_page=%d&page=%d", url.PathEscape(owner), url.PathEscape(repo), pageSize, page), nil)
		if err != nil {
			return nil, err
		}
		var tags []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		err = json.NewDecoder(resp.Body).Decode(&tags)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags of %s/%s: %w", owner, repo, err)
		}
		for _, t := range tags {
			out = append(out, Tag{Name: t.Name, Commit: t.Commit.SHA})
		}
		if len(tags) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrTooManyTags, owner, repo)
}

// alreadyExists reports whether err is GitHub refusing to create something
// that is already there.
func alreadyExists(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity &&
		(strings.Contains(se.Body, "already exists") || strings.Contains(se.Body, "already_exists"))
}

// CreateTag points a new tag at commit sha; ErrTagExists when owner/repo
// has the tag already.
func (c *Client) CreateTag(ctx context.Context, owner, repo, tag, sha string) error {
	in := map[string]string{"ref": "refs/tags/" + tag, "sha": sha}
	err := c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/git/refs", in, nil)
	if alreadyExists(err) {
		return fmt.Errorf("%w: %s in %s/%s", ErrTagExists, tag, owner, repo)
	}
	return err
}

// Release is a GitHub release to create for a tag that exists.
type Release struct {
	Tag, Commit string
	// Previous is the tag its notes start from; empty for the first release.
	Previous string
	// Prerelease marks a release candidate.
	Prerelease bool
	// Latest makes it the repo's latest release.
	Latest bool
}

// ReleaseExists reports whether owner/repo has a release for tag.
func (c *Client) ReleaseExists(ctx context.Context, owner, repo, tag string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/releases/tags/"+url.PathEscape(tag), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// CreateRelease publishes r with notes GitHub writes from the pull requests
// merged since r.Previous; when GitHub cannot write them the release has
// none. A release that is already there is left alone.
func (c *Client) CreateRelease(ctx context.Context, owner, repo string, r Release) error {
	notesIn := map[string]string{"tag_name": r.Tag, "target_commitish": r.Commit}
	if r.Previous != "" {
		notesIn["previous_tag_name"] = r.Previous
	}
	var notes struct {
		Body string `json:"body"`
	}
	// The notes are a nicety: a release without them beats no release.
	_ = c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/releases/generate-notes", notesIn, &notes)
	latest := "false"
	if r.Latest && !r.Prerelease {
		latest = "true"
	}
	in := map[string]any{
		"tag_name": r.Tag, "target_commitish": r.Commit, "name": r.Tag, "body": notes.Body,
		"prerelease": r.Prerelease, "make_latest": latest,
	}
	err := c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/releases", in, nil)
	if alreadyExists(err) {
		return nil
	}
	return err
}
