// Package registry checks whether an image exists in an OCI registry that
// uses the registry token flow, such as ghcr.io. Errors never contain
// credentials or tokens.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Ref is a parsed image reference "host/repo:tag".
type Ref struct{ Host, Repo, Tag string }

// ParseRef parses "host/owner/name:tag". A host and a tag are required.
func ParseRef(image string) (Ref, error) {
	host, rest, ok := strings.Cut(image, "/")
	if !ok || !strings.Contains(host, ".") {
		return Ref{}, fmt.Errorf("image %q must be host/repo:tag", image)
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return Ref{}, fmt.Errorf("image %q must be host/repo:tag", image)
	}
	return Ref{Host: host, Repo: rest[:i], Tag: rest[i+1:]}, nil
}

// manifestTypes are the manifest media types Exists accepts.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Client checks images in a registry.
type Client struct {
	// BaseURL overrides "https://<host>" (tests).
	BaseURL string
	// HTTP defaults to a client with a 30s timeout.
	HTTP *http.Client
	// Creds returns the registry login for host.
	Creds func(ctx context.Context, host string) (user, pass string, err error)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Exists reports whether image (host/repo:tag) is in its registry.
func (c *Client) Exists(ctx context.Context, image string) (bool, error) {
	r, err := ParseRef(image)
	if err != nil {
		return false, err
	}
	base := c.BaseURL
	if base == "" {
		base = "https://" + r.Host
	}
	user, pass, err := c.Creds(ctx, r.Host)
	if err != nil {
		return false, fmt.Errorf("registry login for %s: %w", r.Host, err)
	}
	token, err := c.token(ctx, base, r, user, pass)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/v2/"+r.Repo+"/manifests/"+url.PathEscape(r.Tag), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", manifestTypes)
	resp, err := c.http().Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s manifest %s: %s", r.Repo, r.Tag, resp.Status)
	}
}

func (c *Client) token(ctx context.Context, base string, r Ref, user, pass string) (string, error) {
	q := url.Values{"service": {r.Host}, "scope": {"repository:" + r.Repo + ":pull"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/token?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, pass)
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("registry token for %s: %s", r.Repo, resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("registry token for %s: unreadable answer", r.Repo)
	}
	return out.Token, nil
}

// CredsFromDockerConfig returns the login for host from a
// kubernetes.io/dockerconfigjson Secret's .dockerconfigjson.
func CredsFromDockerConfig(data []byte, host string) (string, string, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", errors.New("pull secret is not a docker config")
	}
	a, ok := cfg.Auths[host]
	if !ok {
		return "", "", fmt.Errorf("pull secret has no login for %s", host)
	}
	if a.Username != "" {
		return a.Username, a.Password, nil
	}
	raw, err := base64.StdEncoding.DecodeString(a.Auth)
	if err != nil {
		return "", "", fmt.Errorf("pull secret login for %s is not base64", host)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", fmt.Errorf("pull secret login for %s is not user:password", host)
	}
	return user, pass, nil
}
