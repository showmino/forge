// Package registry checks whether an image exists in an OCI registry
// (ghcr.io, an Azure Container Registry, Docker Hub, ...), following the
// registry's own login challenge. Errors never contain credentials or
// tokens.
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

// Exists reports whether image (host/repo:tag) is in its registry. It asks
// without a login first; when the registry answers with a challenge, it logs
// in where the challenge says (ghcr.io's /token, an Azure registry's
// /oauth2/token, Docker Hub's auth.docker.io) and asks again.
func (c *Client) Exists(ctx context.Context, image string) (bool, error) {
	r, err := ParseRef(image)
	if err != nil {
		return false, err
	}
	base := c.BaseURL
	if base == "" {
		base = "https://" + r.Host
	}
	manifest := base + "/v2/" + r.Repo + "/manifests/" + url.PathEscape(r.Tag)
	status, hdr, err := c.head(ctx, manifest, "")
	if err != nil {
		return false, err
	}
	if status == http.StatusUnauthorized {
		ch := parseChallenge(hdr.Get("WWW-Authenticate"))
		user, pass, err := c.Creds(ctx, r.Host)
		if err != nil {
			return false, fmt.Errorf("registry login for %s: %w", r.Host, err)
		}
		var auth string
		switch ch.scheme {
		case "bearer":
			token, err := c.token(ctx, ch, r, user, pass)
			if err != nil {
				return false, err
			}
			auth = "Bearer " + token
		case "basic":
			auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
		default:
			return false, fmt.Errorf("HEAD %s manifest %s: %d with no login method the registry names", r.Repo, r.Tag, status)
		}
		if status, _, err = c.head(ctx, manifest, auth); err != nil {
			return false, err
		}
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s manifest %s: %d %s", r.Repo, r.Tag, status, http.StatusText(status))
	}
}

// head sends a manifest HEAD request and returns the status and headers.
func (c *Client) head(ctx context.Context, manifest, auth string) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifest, nil)
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept", manifestTypes)
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, nil, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// challenge is a WWW-Authenticate header: the scheme, lower case, and for
// "bearer" where to get a token.
type challenge struct{ scheme, realm, service string }

// parseChallenge reads a single challenge such as
// Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="...".
func parseChallenge(h string) challenge {
	scheme, params, _ := strings.Cut(strings.TrimSpace(h), " ")
	ch := challenge{scheme: strings.ToLower(scheme)}
	for _, p := range strings.Split(params, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(k) {
		case "realm":
			ch.realm = v
		case "service":
			ch.service = v
		}
	}
	return ch
}

// token gets a pull token for r from the challenge's realm. The login goes
// only to an https realm (or to any realm when BaseURL is set, in tests).
func (c *Client) token(ctx context.Context, ch challenge, r Ref, user, pass string) (string, error) {
	realm, err := url.Parse(ch.realm)
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && c.BaseURL == "") {
		return "", fmt.Errorf("registry token for %s: the registry names no https token service", r.Repo)
	}
	service := ch.service
	if service == "" {
		service = r.Host
	}
	q := realm.Query()
	q.Set("service", service)
	q.Set("scope", "repository:"+r.Repo+":pull")
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
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
	// ghcr.io answers "token", Azure "access_token"; the spec allows either.
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("registry token for %s: unreadable answer", r.Repo)
	}
	if out.Token == "" {
		out.Token = out.AccessToken
	}
	if out.Token == "" {
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
		a, ok = cfg.Auths["https://"+host]
	}
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
