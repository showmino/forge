package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Repo is a repository GitHub created.
type Repo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

// File is one file of a commit.
type File struct {
	Path       string
	Executable bool
	Content    []byte
}

// ErrRepoExists means a repository of that name already exists.
var ErrRepoExists = errors.New("the repository already exists")

func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// RepoExists reports whether owner/name exists and is visible to the token.
func (c *Client) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	err := c.doJSON(ctx, http.MethodGet, repoPath(owner, name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// DefaultBranch returns owner/name's default branch. An unknown repo is ErrNotFound.
func (c *Client) DefaultBranch(ctx context.Context, owner, name string) (string, error) {
	var r Repo
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, name), nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("GitHub returned no default branch for %s/%s", owner, name)
	}
	return r.DefaultBranch, nil
}

// CreateOrgRepo creates a private repository in org with GitHub's initial
// commit: the Git Data API cannot write to an empty repository.
func (c *Client) CreateOrgRepo(ctx context.Context, org, name, description string) (*Repo, error) {
	var r Repo
	err := c.doJSON(ctx, http.MethodPost, "/orgs/"+url.PathEscape(org)+"/repos", map[string]any{
		"name":        name,
		"description": description,
		"private":     true,
		"auto_init":   true,
	}, &r)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity && strings.Contains(se.Body, "already exists") {
		return nil, fmt.Errorf("%s/%s: %w", org, name, ErrRepoExists)
	}
	if err != nil {
		return nil, err
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	return &r, nil
}

// DeleteRepo deletes owner/name.
func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error {
	return c.doJSON(ctx, http.MethodDelete, repoPath(owner, name), nil, nil)
}

// refWait is the first wait for a new repository's branch; it grows linearly.
const refWait = 500 * time.Millisecond

// PushFiles replaces the content of branch with files in one commit that has
// no parents, force-updating the branch, and returns the commit's SHA. Text
// files go into the tree inline; other files are uploaded as blobs.
func (c *Client) PushFiles(ctx context.Context, owner, repo, branch, message string, files []File) (string, error) {
	git := repoPath(owner, repo) + "/git"
	if err := c.waitForRef(ctx, git+"/ref/heads/"+branch); err != nil {
		return "", err
	}
	entries := make([]map[string]any, 0, len(files))
	for _, f := range files {
		mode := "100644"
		if f.Executable {
			mode = "100755"
		}
		e := map[string]any{"path": f.Path, "mode": mode, "type": "blob"}
		if utf8.Valid(f.Content) {
			e["content"] = string(f.Content)
		} else {
			var blob struct {
				SHA string `json:"sha"`
			}
			in := map[string]string{"content": base64.StdEncoding.EncodeToString(f.Content), "encoding": "base64"}
			if err := c.doJSON(ctx, http.MethodPost, git+"/blobs", in, &blob); err != nil {
				return "", fmt.Errorf("upload %s: %w", f.Path, err)
			}
			e["sha"] = blob.SHA
		}
		entries = append(entries, e)
	}
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/trees", map[string]any{"tree": entries}, &tree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPatch, git+"/refs/heads/"+branch, map[string]any{"sha": commit.SHA, "force": true}, nil); err != nil {
		return "", fmt.Errorf("update %s: %w", branch, err)
	}
	return commit.SHA, nil
}

// waitForRef reads a branch until it exists: right after creation GitHub
// can answer 404 or 409 ("Git Repository is empty") for a moment.
func (c *Client) waitForRef(ctx context.Context, path string) error {
	var err error
	for attempt := range 6 {
		if err = c.doJSON(ctx, http.MethodGet, path, nil, nil); err == nil {
			return nil
		}
		var se *StatusError
		if !errors.As(err, &se) || (se.Status != http.StatusNotFound && se.Status != http.StatusConflict) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(refWait * time.Duration(attempt+1)):
		}
	}
	return fmt.Errorf("the new repository's branch never appeared: %w", err)
}
