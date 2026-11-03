package github

import (
	"context"
	"net/http"
)

// SetOIDCSubjectClaims makes the subject of owner/repo's Actions OIDC tokens
// the given claims, in order ("repo", "context", "job_workflow_ref"), instead
// of GitHub's default. Then a cloud can trust a token only when it comes from
// one workflow. The App needs Actions: write.
func (c *Client) SetOIDCSubjectClaims(ctx context.Context, owner, repo string, claims []string) error {
	body := struct {
		UseDefault bool     `json:"use_default"`
		Claims     []string `json:"include_claim_keys"`
	}{false, claims}
	return c.doJSON(ctx, http.MethodPut, repoPath(owner, repo)+"/actions/oidc/customization/sub", body, nil)
}
