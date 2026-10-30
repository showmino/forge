// Package chartrepo finds a Helm chart's version in its repository: an OCI
// registry (oci://<registry>/<path>, the chart is <path>/<name>:<version>)
// or an HTTPS chart repository (its index.yaml).
package chartrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrNotFound means the repository has no such chart or version.
var ErrNotFound = errors.New("chart not found")

// maxIndex is the most bytes of an index.yaml read: large public
// repositories have a few megabytes.
const maxIndex = 32 << 20

// Registry looks up OCI artifacts; *registry.Client is one.
type Registry interface {
	Exists(ctx context.Context, image string) (bool, error)
}

// Client looks charts up.
type Client struct {
	// Registry answers for oci:// repositories; nil refuses them.
	Registry Registry
	// HTTP defaults to a client with a 30s timeout that follows redirects
	// only to https.
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 5 {
				return errors.New("redirected off https")
			}
			return nil
		},
	}
}

// Has returns nil when repo has the chart name at version, ErrNotFound
// when it has not, or why it could not be asked.
func (c *Client) Has(ctx context.Context, repo, name, version string) error {
	u, err := url.Parse(repo)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "oci":
		if c.Registry == nil {
			return errors.New("this Forge API cannot look up OCI registries")
		}
		// Helm writes a version's '+' as '_' in an OCI tag.
		ref := strings.TrimSuffix(u.Host+u.Path, "/") + "/" + name + ":" + strings.ReplaceAll(version, "+", "_")
		ok, err := c.Registry.Exists(ctx, ref)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		return nil
	case "https":
		return c.index(ctx, repo, name, version)
	}
	return fmt.Errorf("%s is not an oci:// or https:// chart repository", repo)
}

func (c *Client) index(ctx context.Context, repo, name, version string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repo+"/index.yaml", nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("read the chart repository's index: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s has no index.yaml: is it a chart repository? %w", repo, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("read the chart repository's index: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxIndex+1))
	if err != nil {
		return fmt.Errorf("read the chart repository's index: %w", err)
	}
	if len(data) > maxIndex {
		return fmt.Errorf("the chart repository's index is over %d MiB", maxIndex>>20)
	}
	var idx struct {
		Entries map[string][]struct {
			Version string `yaml:"version"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("the chart repository's index.yaml is not valid: %w", err)
	}
	for _, e := range idx.Entries[name] {
		if e.Version == version || "v"+e.Version == version || e.Version == "v"+version {
			return nil
		}
	}
	return ErrNotFound
}
