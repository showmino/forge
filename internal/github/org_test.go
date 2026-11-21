package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOrgName(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/my-org", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"login":"my-org","name":"My Org"}`)
	})
	mux.HandleFunc("GET /orgs/unnamed", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"login":"unnamed","name":null}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}

	if name, err := c.OrgName(context.Background(), "my-org"); err != nil || name != "My Org" {
		t.Errorf("my-org: %q %v", name, err)
	}
	// An organization without a display name has none: the caller falls back.
	if name, err := c.OrgName(context.Background(), "unnamed"); err != nil || name != "" {
		t.Errorf("unnamed: %q %v", name, err)
	}
	if _, err := c.OrgName(context.Background(), "missing"); err == nil {
		t.Error("missing: no error")
	}
}
