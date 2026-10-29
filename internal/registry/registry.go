// Package registry checks whether an image exists in an OCI registry
// (ghcr.io, an Azure Container Registry, Docker Hub, ...) and finds its
// digest, following the registry's own login challenge. The login goes only to the registry's own domain. Errors
// never contain credentials or tokens.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Ref is a parsed image reference "host/repo:tag", "host/repo@digest" or
// both.
type Ref struct{ Host, Repo, Tag, Digest string }

// Reference is what a manifest request names: the digest when there is one,
// else the tag.
func (r Ref) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Pinned is the image at digest, without its tag: what a cluster pulls, so
// that a moved tag changes nothing that runs.
func (r Ref) Pinned(digest string) string { return r.Host + "/" + r.Repo + "@" + digest }

// ParseRef parses "host/owner/name:tag", "host/owner/name@sha256:..." or
// both. A host, and a tag or a digest, are required.
func ParseRef(image string) (Ref, error) {
	bad := fmt.Errorf("image %q must be host/repo:tag", image)
	host, rest, ok := strings.Cut(image, "/")
	if !ok || !isHost(host) {
		return Ref{}, bad
	}
	r := Ref{Host: host}
	if name, digest, ok := strings.Cut(rest, "@"); ok {
		if !digestRE.MatchString(digest) {
			return Ref{}, fmt.Errorf("image %q: the digest must be sha256:<64 hex digits>", image)
		}
		rest, r.Digest = name, digest
	}
	if i := strings.LastIndex(rest, ":"); i > 0 && !strings.Contains(rest[i:], "/") {
		rest, r.Tag = rest[:i], rest[i+1:]
		if r.Tag == "" {
			return Ref{}, bad
		}
	}
	if !repoRE.MatchString(rest) || (r.Tag == "" && r.Digest == "") {
		return Ref{}, bad
	}
	r.Repo = rest
	return r, nil
}

var (
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	repoRE   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	hostRE   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?$`)
)

// isHost reports whether the first part of an image name is a registry
// host: it has a dot or a port, or is localhost.
func isHost(s string) bool {
	return (strings.ContainsAny(s, ".:") || s == "localhost") && hostRE.MatchString(s)
}

// Normalize writes an image as people type it ("nginx:1.27",
// "grafana/grafana:11.2.0") in full, the way ParseRef reads it:
// "docker.io/library/nginx:1.27". With neither a tag nor a digest the tag
// is "latest", as Docker reads it; whether that may run is policy's call.
func Normalize(image string) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("image %q: %s", image, why) }
	if image == "" || strings.ContainsAny(image, " \t\n") || strings.Contains(image, "://") {
		return bad("write it as [host/]name[:tag][@sha256:digest], without a scheme")
	}
	name, digest, hasDigest := strings.Cut(image, "@")
	if hasDigest && !digestRE.MatchString(digest) {
		return bad("the digest must be sha256:<64 hex digits>")
	}
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		name, tag = name[:i], name[i+1:]
		if !tagRE.MatchString(tag) {
			return bad("the tag must be letters, digits, '_', '.' or '-', at most 128, not starting with '.' or '-'")
		}
	}
	host, repo := "docker.io", name
	if first, rest, ok := strings.Cut(name, "/"); ok && isHost(first) {
		host, repo = strings.ToLower(first), rest
	}
	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if !repoRE.MatchString(repo) {
		return bad("the name must be lowercase letters, digits and separators")
	}
	out := host + "/" + repo
	if tag == "" && !hasDigest {
		tag = "latest"
	}
	if tag != "" {
		out += ":" + tag
	}
	if hasDigest {
		out += "@" + digest
	}
	return out, nil
}

// apiBase is where a registry's API answers: Docker Hub's images are named
// docker.io, its API is registry-1.docker.io.
func apiBase(host string) string {
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return "https://" + host
}

// ErrNotFound is the registry's answer for an image that is not there.
var ErrNotFound = errors.New("image not found")

// ErrNoLogin is what Creds returns when the install has no login for a
// registry at all. Only then is the registry asked anonymously: any other
// failure to read the logins (a broken pull secret, a denied read) is
// reported, not hidden behind a "not found".
var ErrNoLogin = errors.New("no login for this registry")

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
	_, err := c.lookup(ctx, image)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Digest returns the digest of image's manifest (for a multi-platform image,
// its index), as the registry names it: what a deploy pins.
func (c *Client) Digest(ctx context.Context, image string) (string, error) {
	hdr, err := c.lookup(ctx, image)
	if err != nil {
		return "", err
	}
	d := hdr.Get("Docker-Content-Digest")
	if !digestRE.MatchString(d) {
		return "", fmt.Errorf("%s: the registry named no sha256 digest for it", image)
	}
	if r, _ := ParseRef(image); r.Digest != "" && r.Digest != d {
		return "", fmt.Errorf("%s: the registry answered with %s, not the digest it names", image, d)
	}
	return d, nil
}

// lookup asks for image's manifest and returns the answer's headers, or
// ErrNotFound. A registry that wants a login gets the install's; without
// one for its host, a bearer registry is asked for an anonymous token, which
// public images on Docker Hub and ghcr.io need.
func (c *Client) lookup(ctx context.Context, image string) (http.Header, error) {
	r, err := ParseRef(image)
	if err != nil {
		return nil, err
	}
	base := c.BaseURL
	if base == "" {
		base = apiBase(r.Host)
	}
	ref := r.Reference()
	manifest := base + "/v2/" + r.Repo + "/manifests/" + url.PathEscape(ref)
	status, hdr, err := c.head(ctx, manifest, "")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		ch := parseChallenge(hdr.Get("WWW-Authenticate"))
		user, pass, credsErr := "", "", ErrNoLogin
		if c.Creds != nil {
			user, pass, credsErr = c.Creds(ctx, r.Host)
		}
		anonymous := errors.Is(credsErr, ErrNoLogin)
		if credsErr != nil && !anonymous {
			return nil, fmt.Errorf("registry login for %s: %w", r.Host, credsErr)
		}
		var auth string
		switch ch.scheme {
		case "bearer":
			if anonymous {
				user, pass = "", ""
			}
			token, err := c.token(ctx, ch, r, user, pass)
			if err != nil {
				if anonymous {
					return nil, fmt.Errorf("registry login for %s: %w (and an anonymous token: %w)", r.Host, credsErr, err)
				}
				return nil, err
			}
			auth = "Bearer " + token
		case "basic":
			if anonymous {
				return nil, fmt.Errorf("registry login for %s: %w", r.Host, credsErr)
			}
			auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
		default:
			return nil, fmt.Errorf("HEAD %s manifest %s: %d with no login method the registry names", r.Repo, ref, status)
		}
		if status, hdr, err = c.head(ctx, manifest, auth); err != nil {
			return nil, err
		}
		// Anonymous and still refused: the image is private, and the install
		// has no login that could read it.
		if anonymous && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			return nil, fmt.Errorf("HEAD %s manifest %s: %d anonymously; registry login for %s: %w", r.Repo, ref, status, r.Host, credsErr)
		}
	}
	switch status {
	case http.StatusOK:
		// Not a redirect's target: head does not follow redirects.
		return hdr, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", image, ErrNotFound)
	default:
		return nil, fmt.Errorf("HEAD %s manifest %s: %d %s", r.Repo, ref, status, http.StatusText(status))
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
	// A registry answers a manifest itself. A redirect (docker.io's goes to
	// www.docker.com, which answers 200 for anything) is not an answer.
	hc := *c.http()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
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
// only to an https realm in the registry's own domain (any realm when
// BaseURL is set, in tests).
func (c *Client) token(ctx context.Context, ch challenge, r Ref, user, pass string) (string, error) {
	realm, err := url.Parse(ch.realm)
	if err != nil || realm.Host == "" || (c.BaseURL == "" && (realm.Scheme != "https" || !sameDomain(realm.Hostname(), r.Host))) {
		return "", fmt.Errorf("registry token for %s: the registry names no https token service in its own domain", r.Repo)
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
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
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

// sameDomain reports whether host is registry's host or shares its last two
// labels (auth.registry.example for registry.example).
func sameDomain(host, registry string) bool {
	if h, _, err := net.SplitHostPort(registry); err == nil {
		registry = h
	}
	host, registry = strings.ToLower(host), strings.ToLower(registry)
	if host == registry {
		return true
	}
	last2 := func(s string) string {
		parts := strings.Split(s, ".")
		if len(parts) < 2 {
			return ""
		}
		return strings.Join(parts[len(parts)-2:], ".")
	}
	d := last2(registry)
	return d != "" && strings.Contains(d, ".") && last2(host) == d && net.ParseIP(host) == nil
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
	keys := []string{host, "https://" + host}
	if host == "docker.io" { // where `docker login` writes Docker Hub's login
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io")
	}
	var a struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	ok := false
	for _, k := range keys {
		if a, ok = cfg.Auths[k]; ok {
			break
		}
	}
	if !ok {
		return "", "", fmt.Errorf("pull secret has no login for %s: %w", host, ErrNoLogin)
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
