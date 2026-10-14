package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ReleaseAsset returns the content of the asset name of owner/repo's release
// tag. A missing release or asset is ErrNotFound. GitHub answers with a
// redirect to a signed URL, which the HTTP client follows without the token.
func (c *Client) ReleaseAsset(ctx context.Context, owner, repo, tag, name string) (io.ReadCloser, error) {
	var rel struct {
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/releases/tags/"+url.PathEscape(tag), nil, &rel); err != nil {
		return nil, err
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			resp, err := c.doAccept(ctx, http.MethodGet, fmt.Sprintf("%s/releases/assets/%d", repoPath(owner, repo), a.ID), nil, "application/octet-stream")
			if err != nil {
				return nil, err
			}
			return resp.Body, nil
		}
	}
	return nil, fmt.Errorf("%s in release %s of %s/%s: %w", name, tag, owner, repo, ErrNotFound)
}
