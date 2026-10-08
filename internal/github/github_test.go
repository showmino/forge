package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-GitHub-Api-Version") == "" {
			http.Error(w, "bad headers", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			http.Error(w, "want per_page=100", http.StatusBadRequest)
			return
		}
		first, n := 0, 100 // 150 tags over two pages
		if r.URL.Query().Get("page") == "2" {
			first, n = 100, 50
		}
		var items []string
		for i := first; i < first+n; i++ {
			items = append(items, fmt.Sprintf(`{"name":"t%d","commit":{"sha":"x"}}`, i))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/o/r/tarball/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/codeload/o/r/v1.0.0", http.StatusFound) // GitHub redirects to codeload
	})
	mux.HandleFunc("GET /codeload/o/r/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("TARBALL"))
	})
	mux.HandleFunc("GET /repos/o/broken/tags", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestListTags(t *testing.T) {
	t.Parallel()
	c := &Client{BaseURL: fakeGitHub(t).URL, Token: StaticToken("tok")}
	tags, err := c.ListTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 150 || tags[0] != "t0" || tags[149] != "t149" {
		t.Errorf("got %d tags: first %q last %q", len(tags), tags[0], tags[len(tags)-1])
	}
}

func TestTarballFollowsRedirect(t *testing.T) {
	t.Parallel()
	c := &Client{BaseURL: fakeGitHub(t).URL, Token: StaticToken("tok")}
	body, err := c.Tarball(context.Background(), "o", "r", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "TARBALL" {
		t.Errorf("body %q", data)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv := fakeGitHub(t)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}
	if _, err := c.Tarball(context.Background(), "o", "r", "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag: %v", err)
	}
	_, err := c.ListTags(context.Background(), "o", "broken")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("server error: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "tok") {
		t.Errorf("error leaks the token: %v", err)
	}
	failing := &Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", errors.New("no key") }}
	if _, err := failing.ListTags(context.Background(), "o", "r"); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("token error: %v", err)
	}
}
