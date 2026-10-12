package github

import (
	"context"
	"encoding/base64"
	"fmt"
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
