package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TreeEntry is one entry of a Git tree.
type TreeEntry struct {
	Path string `json:"path"`
	// Type is "blob" (a file), "tree" (a folder) or "commit" (a submodule).
	Type string `json:"type"`
	SHA  string `json:"sha"`
	// Size is the blob's size in bytes; folders have none.
	Size int64 `json:"size"`
}

// Tree is a Git tree, with the paths of its entries relative to it.
type Tree struct {
	SHA     string      `json:"sha"`
	Entries []TreeEntry `json:"tree"`
	// Truncated is true when GitHub left entries out (more than 100,000, or 7 MB).
	Truncated bool `json:"truncated"`
}

// HeadSHA returns the commit at the head of owner/repo's default branch,
// asking GitHub for the SHA alone. An empty or unknown repo is ErrNotFound.
func (c *Client) HeadSHA(ctx context.Context, owner, repo string) (string, error) {
	resp, err := c.doAccept(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/HEAD", nil, "application/vnd.github.sha")
	var se *StatusError
	if errors.As(err, &se) && (se.Status == http.StatusConflict || se.Status == http.StatusUnprocessableEntity) {
		// 409: the repo is empty; 422: no commit for HEAD.
		return "", fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(data))
	if len(sha) != 40 && len(sha) != 64 {
		return "", fmt.Errorf("GitHub returned %q as the head of %s/%s", sha, owner, repo)
	}
	return sha, nil
}

// Tree returns the tree treeish (a tree or commit SHA) names, and with
// recursive every entry below it too.
func (c *Client) Tree(ctx context.Context, owner, repo, treeish string, recursive bool) (*Tree, error) {
	p := repoPath(owner, repo) + "/git/trees/" + url.PathEscape(treeish)
	if recursive {
		p += "?recursive=1"
	}
	var t Tree
	if err := c.doJSON(ctx, http.MethodGet, p, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Blob returns the content of the blob sha. Unlike the contents API it reads
// files of any size up to GitHub's 100 MB.
func (c *Client) Blob(ctx context.Context, owner, repo, sha string) ([]byte, error) {
	var out struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/git/blobs/"+url.PathEscape(sha), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("blob %s: unexpected encoding %q", sha, out.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// LastChange returns the newest commit reachable from ref that changed
// path (a file or a folder). A path no commit touched is ErrNotFound.
func (c *Client) LastChange(ctx context.Context, owner, repo, ref, path string) (CommitInfo, error) {
	var out []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	q := url.Values{"sha": {ref}, "path": {path}, "per_page": {"1"}}
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/commits?"+q.Encode(), nil, &out); err != nil {
		return CommitInfo{}, err
	}
	if len(out) == 0 {
		return CommitInfo{}, fmt.Errorf("%s at %s: %w", path, ref, ErrNotFound)
	}
	return CommitInfo{SHA: out[0].SHA, Message: out[0].Commit.Message, Time: out[0].Commit.Committer.Date}, nil
}
