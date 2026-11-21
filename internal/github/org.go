package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// OrgName returns the display name of org, "My Org"; empty when the
// organization has none. It needs no permission beyond the installation.
func (c *Client) OrgName(ctx context.Context, org string) (string, error) {
	var out struct {
		Name *string `json:"name"`
	}
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s", url.PathEscape(org)), nil, &out); err != nil {
		return "", err
	}
	if out.Name == nil {
		return "", nil
	}
	return *out.Name, nil
}
