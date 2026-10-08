// Package github is a small client for the GitHub REST API: what Forge needs
// and nothing more. App authentication is in app.go; milestone 3 adds repo creation.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API.
const DefaultBaseURL = "https://api.github.com"

const (
	pageSize = 100
	maxPages = 50
)

// ErrNotFound is wrapped by errors for HTTP 404 responses.
var ErrNotFound = errors.New("not found on GitHub")

// Client calls the GitHub REST API.
type Client struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// HTTP defaults to a client with a 60s timeout.
	HTTP *http.Client
	// Token returns the bearer token for each request; nil sends none.
	Token func(ctx context.Context) (string, error)
}

// StaticToken returns a Token func for a fixed token.
func StaticToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token, nil }
}

// do sends a request and returns the response if it answered 2xx; the caller closes its body.
func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("github token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
}

// ListTags returns every tag name of owner/repo.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]string, error) {
	var names []string
	for page := 1; page <= maxPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/tags?per_page=%d&page=%d", url.PathEscape(owner), url.PathEscape(repo), pageSize, page))
		if err != nil {
			return nil, err
		}
		var tags []struct {
			Name string `json:"name"`
		}
		err = json.NewDecoder(resp.Body).Decode(&tags)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags of %s/%s: %w", owner, repo, err)
		}
		for _, t := range tags {
			names = append(names, t.Name)
		}
		if len(tags) < pageSize {
			break
		}
	}
	return names, nil
}

// Tarball returns the gzipped tarball of owner/repo at ref. GitHub answers
// with a redirect to a signed download URL, which the HTTP client follows
// without forwarding the Authorization header to the other host.
func (c *Client) Tarball(ctx context.Context, owner, repo, ref string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/tarball/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref)))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
