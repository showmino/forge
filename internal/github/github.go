// Package github is a small client for the GitHub REST API: what Forge needs
// and nothing more. App authentication is in app.go; repos.go creates repos and commits.go reads refs, files and CI runs.
package github

import (
	"bytes"
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

// StatusError is a non-2xx answer from GitHub.
type StatusError struct {
	Method, Path string
	Status       int
	// Body is at most 512 bytes of the response.
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Body)
}

// Is makes errors.Is(err, ErrNotFound) true for 404 answers.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// do sends a request with in as its JSON body (none when nil) and returns
// the response if it answered 2xx; the caller closes its body.
func (c *Client) do(ctx context.Context, method, path string, in any) (*http.Response, error) {
	return c.doAccept(ctx, method, path, in, "application/vnd.github+json")
}

// doAccept is do with the media type the answer should have.
func (c *Client) doAccept(ctx context.Context, method, path string, in any, accept string) (*http.Response, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, &StatusError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
}

// doJSON is do that decodes a JSON reply into out (discarded when out is nil).
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	resp, err := c.do(ctx, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// ListTags returns every tag name of owner/repo.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]string, error) {
	tags, err := c.ListTagCommits(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	return names, nil
}

// Tarball returns the gzipped tarball of owner/repo at ref. GitHub answers
// with a redirect to a signed download URL, which the HTTP client follows
// without forwarding the Authorization header to the other host.
func (c *Client) Tarball(ctx context.Context, owner, repo, ref string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/tarball/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref)), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
