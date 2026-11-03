package github

import (
	"context"
	"net/http"
	"net/url"
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

// DispatchWorkflow starts the workflow file (its name under
// .github/workflows, such as deploy.yml) of owner/repo on ref, with inputs,
// as a workflow_dispatch event. The App needs Actions: write.
func (c *Client) DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string, inputs map[string]string) error {
	body := struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{ref, inputs}
	return c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/actions/workflows/"+url.PathEscape(workflow)+"/dispatches", body, nil)
}
