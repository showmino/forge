package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTagsAndReleases(t *testing.T) {
	t.Parallel()
	var created []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/pay/tags", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"name":"v0.1.0","commit":{"sha":"aaa"}},{"name":"v0.1.1-rc.1","commit":{"sha":"bbb"}}]`)
	})
	mux.HandleFunc("POST /repos/showmino/pay/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["ref"] == "refs/tags/v0.1.0" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Reference already exists"}`)
			return
		}
		if in["ref"] != "refs/tags/v0.1.1" || in["sha"] != "ccc" {
			t.Errorf("create ref: %v", in)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /repos/showmino/pay/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("tag") != "v0.1.0" {
			w.WriteHeader(http.StatusNotFound)
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /repos/showmino/busy/tags", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "["+strings.Repeat(`{"name":"t","commit":{"sha":"aaa"}},`, pageSize-1)+`{"name":"t","commit":{"sha":"aaa"}}]`)
	})
	mux.HandleFunc("POST /repos/showmino/pay/releases/generate-notes", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["tag_name"] == "v0.1.3" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		fmt.Fprintf(w, `{"name":"x","body":"notes since %s"}`, in["previous_tag_name"])
	})
	mux.HandleFunc("POST /repos/showmino/pay/releases", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["tag_name"] == "v0.1.0" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"resource":"Release","code":"already_exists","field":"tag_name"}]}`)
			return
		}
		created = append(created, in)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}
	ctx := context.Background()

	tags, err := c.ListTagCommits(ctx, "showmino", "pay")
	if err != nil || len(tags) != 2 || tags[1] != (Tag{Name: "v0.1.1-rc.1", Commit: "bbb"}) {
		t.Fatalf("tags %+v, %v", tags, err)
	}
	if names, err := c.ListTags(ctx, "showmino", "pay"); err != nil || len(names) != 2 || names[0] != "v0.1.0" {
		t.Errorf("names %v, %v", names, err)
	}

	if err := c.CreateTag(ctx, "showmino", "pay", "v0.1.1", "ccc"); err != nil {
		t.Errorf("create tag: %v", err)
	}
	if err := c.CreateTag(ctx, "showmino", "pay", "v0.1.0", "ccc"); !errors.Is(err, ErrTagExists) {
		t.Errorf("existing tag: %v", err)
	}

	for tag, want := range map[string]bool{"v0.1.0": true, "v0.1.1": false} {
		if got, err := c.ReleaseExists(ctx, "showmino", "pay", tag); err != nil || got != want {
			t.Errorf("ReleaseExists(%s) = %v, %v", tag, got, err)
		}
	}

	// A list that cannot be read to its end is an error, not a short list.
	if _, err := c.ListTagCommits(ctx, "showmino", "busy"); !errors.Is(err, ErrTooManyTags) {
		t.Errorf("too many tags: %v", err)
	}

	if err := c.CreateRelease(ctx, "showmino", "pay", Release{Tag: "v0.1.1", Commit: "ccc", Previous: "v0.1.0", Latest: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateRelease(ctx, "showmino", "pay", Release{Tag: "v0.1.2-rc.1", Commit: "ddd", Previous: "v0.1.1", Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	// One that is already there is not an error.
	if err := c.CreateRelease(ctx, "showmino", "pay", Release{Tag: "v0.1.0", Commit: "aaa"}); err != nil {
		t.Errorf("existing release: %v", err)
	}
	// An older version released late is not the latest, and a release
	// GitHub writes no notes for is made without them.
	if err := c.CreateRelease(ctx, "showmino", "pay", Release{Tag: "v0.1.3", Commit: "eee"}); err != nil {
		t.Fatal(err)
	}
	if len(created) != 3 {
		t.Fatalf("created %v", created)
	}
	if late := created[2]; late["tag_name"] != "v0.1.3" || late["body"] != "" || late["make_latest"] != "false" || late["prerelease"] != false {
		t.Errorf("late release: %v", late)
	}
	final, rc := created[0], created[1]
	if final["name"] != "v0.1.1" || final["target_commitish"] != "ccc" || final["body"] != "notes since v0.1.0" || final["prerelease"] != false || final["make_latest"] != "true" {
		t.Errorf("final release: %v", final)
	}
	if rc["tag_name"] != "v0.1.2-rc.1" || rc["prerelease"] != true || rc["make_latest"] != "false" {
		t.Errorf("candidate release: %v", rc)
	}
}
