package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListTeams(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	// 130 teams over two pages, newest first as GitHub may send them.
	mux.HandleFunc("GET /orgs/showmino/teams", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != fmt.Sprint(pageSize) {
			t.Errorf("per_page: %q", r.URL.Query().Get("per_page"))
		}
		first, last := 129, 30
		if r.URL.Query().Get("page") == "2" {
			first, last = 29, 0
		}
		fmt.Fprint(w, "[")
		for i := first; i >= last; i-- {
			fmt.Fprintf(w, `{"slug":"team-%03d","name":"Team %d","description":"d%d","privacy":"closed"}`, i, i, i)
			if i > last {
				fmt.Fprint(w, ",")
			}
		}
		fmt.Fprint(w, "]")
	})
	mux.HandleFunc("GET /orgs/locked/teams", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}

	teams, err := c.ListTeams(context.Background(), "showmino")
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 130 || teams[0] != (Team{Slug: "team-000", Name: "Team 0", Description: "d0"}) || teams[129].Slug != "team-129" {
		t.Errorf("got %d teams: first %+v last %+v", len(teams), teams[0], teams[len(teams)-1])
	}

	// Without the "Members: read" permission GitHub answers 403; callers tell that apart.
	_, err = c.ListTeams(context.Background(), "locked")
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusForbidden {
		t.Errorf("got %v", err)
	}
}

func TestTeamRepoAccess(t *testing.T) {
	t.Parallel()
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /orgs/showmino/teams/billing/repos/showmino/payments", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, "PUT "+string(body))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /orgs/showmino/teams/nobody/repos/showmino/payments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("DELETE /orgs/showmino/teams/billing/repos/showmino/payments", func(w http.ResponseWriter, _ *http.Request) {
		got = append(got, "DELETE")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /orgs/showmino/teams/gone/repos/showmino/payments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: StaticToken("tok")}
	ctx := context.Background()

	if err := c.SetTeamRepoPermission(ctx, "showmino", "billing", "showmino", "payments", "maintain"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetTeamRepoPermission(ctx, "showmino", "nobody", "showmino", "payments", "maintain"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a team that does not exist: %v", err)
	}
	if err := c.RemoveTeamRepo(ctx, "showmino", "billing", "showmino", "payments"); err != nil {
		t.Fatal(err)
	}
	// Already gone is done.
	if err := c.RemoveTeamRepo(ctx, "showmino", "gone", "showmino", "payments"); err != nil {
		t.Errorf("a team without access: %v", err)
	}
	if len(got) != 2 || got[0] != `PUT {"permission":"maintain"}` || got[1] != "DELETE" {
		t.Errorf("calls: %q", got)
	}
}
