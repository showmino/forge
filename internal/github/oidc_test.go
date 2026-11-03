package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetOIDCSubjectClaims(t *testing.T) {
	t.Parallel()
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/my-org/infra-net/actions/oidc/customization/sub", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}
	if err := c.SetOIDCSubjectClaims(context.Background(), "my-org", "infra-net", []string{"repo", "context", "job_workflow_ref"}); err != nil {
		t.Fatal(err)
	}
	if want := `{"use_default":false,"include_claim_keys":["repo","context","job_workflow_ref"]}`; got != want {
		t.Errorf("body %s, want %s", got, want)
	}
}
