package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPullsAndReviews(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/my-org/deploys/commits/abc/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"number":7,"html_url":"https://github.com/my-org/deploys/pull/7","user":{"login":"ana"},"merged_at":"2026-11-03T10:00:00Z","base":{"ref":"main"},"head":{"sha":"def"},"merge_commit_sha":"abc"},
			{"number":8,"html_url":"u","merged_at":null,"base":{"ref":"main"},"head":{"sha":"abc"}}]`)
	})
	mux.HandleFunc("GET /repos/my-org/deploys/pulls/7/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"user":{"login":"dee"},"state":"APPROVED","commit_id":"def"}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok"), HTTP: srv.Client()}
	pulls, err := c.PullsForCommit(context.Background(), "my-org", "deploys", "abc")
	if err != nil || len(pulls) != 2 || pulls[0].Number != 7 || pulls[0].User != "ana" || pulls[0].Merged.IsZero() || pulls[0].Base != "main" || pulls[0].HeadSHA != "def" || pulls[0].MergeCommit != "abc" || !pulls[1].Merged.IsZero() {
		t.Fatalf("pulls %+v %v", pulls, err)
	}

	reviews, err := c.PullReviews(context.Background(), "my-org", "deploys", 7)
	if err != nil || len(reviews) != 1 || reviews[0] != (Review{User: "dee", State: "APPROVED", CommitID: "def"}) {
		t.Fatalf("reviews %+v %v", reviews, err)
	}
}

func TestPullReviewsLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		fourthPage  int
		wantReviews int
		wantError   bool
	}{
		{name: "exactly 300", wantReviews: 300},
		{name: "more than 300", fourthPage: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := `{"user":{"login":"reviewer"},"state":"COMMENTED","commit_id":"head"}`
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/my-org/deploys/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
				count := 100
				if r.URL.Query().Get("page") == "4" {
					count = tc.fourthPage
				}
				rows := make([]string, count)
				for i := range rows {
					rows[i] = row
				}
				_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := &Client{BaseURL: srv.URL, Token: StaticToken("tok"), HTTP: srv.Client()}
			reviews, err := c.PullReviews(context.Background(), "my-org", "deploys", 7)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "more than 300 reviews") {
					t.Fatalf("PullReviews error = %v", err)
				}
				return
			}
			if err != nil || len(reviews) != tc.wantReviews {
				t.Fatalf("PullReviews returned %d reviews, %v", len(reviews), err)
			}
		})
	}
}
