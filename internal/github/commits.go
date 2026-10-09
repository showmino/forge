package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// escapeRef escapes a ref for a URL path, keeping the slashes of branch
// names like "feature/x".
func escapeRef(ref string) string {
	return strings.ReplaceAll(url.PathEscape(ref), "%2F", "/")
}

// ResolveRef returns the full SHA of the commit that ref (a branch, tag or
// commit) points to. An unknown ref is ErrNotFound.
func (c *Client) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+escapeRef(ref), nil, &out)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
		// GitHub answers 422 "No commit found for SHA" for unknown SHAs.
		return "", fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("GitHub returned no SHA for %s/%s@%s", owner, repo, ref)
	}
	return out.SHA, nil
}

// FileAt returns the content of path at ref. A missing file is ErrNotFound.
func (c *Client) FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	var out struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	p := repoPath(owner, repo) + "/contents/" + escapeRef(path) + "?ref=" + url.QueryEscape(ref)
	if err := c.doJSON(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("%s at %s: unexpected encoding %q", path, ref, out.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// WorkflowRun is one GitHub Actions run.
type WorkflowRun struct {
	Name string `json:"name"`
	// Status is queued, in_progress, completed, ...
	Status string `json:"status"`
	// Conclusion is success, failure, cancelled, ... once completed.
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// RunsFor returns the Actions runs for commit sha, newest first.
func (c *Client) RunsFor(ctx context.Context, owner, repo, sha string) ([]WorkflowRun, error) {
	var out struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	p := repoPath(owner, repo) + "/actions/runs?per_page=20&head_sha=" + url.QueryEscape(sha)
	if err := c.doJSON(ctx, http.MethodGet, p, nil, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}
