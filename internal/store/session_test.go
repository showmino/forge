package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/showmino/forge/internal/store/pgtest"
)

func TestMemorySessions(t *testing.T) {
	t.Parallel()
	runSessionSuite(t, NewMemory())
}

func TestPostgresSessions(t *testing.T) {
	t.Parallel()
	p := openTestPostgres(t, pgtest.URL(t))
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runSessionSuite(t, p)
}

// runSessionSuite checks a SessionStore that starts empty.
func runSessionSuite(t *testing.T, s SessionStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	mk := func(id, subject string, min int) Session {
		c := at.Add(time.Duration(min) * time.Minute)
		return Session{
			ID: id, TokenHash: "hash-" + id, Kind: "cli", Label: "laptop", Subject: subject, Email: subject + "@example.com", Login: subject,
			Groups: []string{"showmino:platform"}, CreatedAt: c, LastUsedAt: c, ExpiresAt: c.Add(time.Hour), CheckedAt: c,
		}
	}
	for _, ses := range []Session{mk("ses_a", "ana", 0), mk("ses_b", "ana", 10), mk("ses_c", "bo", 20)} {
		if err := s.CreateSession(ctx, ses); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, Session{ID: "ses_n", TokenHash: "hash-n", Kind: "web", Subject: "nia", CreatedAt: at, LastUsedAt: at, ExpiresAt: at, CheckedAt: at}); err != nil {
		t.Fatalf("a session without groups: %v", err)
	}

	got, err := s.SessionByToken(ctx, "hash-ses_a")
	if err != nil || got.ID != "ses_a" || got.Kind != "cli" || got.Label != "laptop" || got.Email != "ana@example.com" || got.Login != "ana" ||
		!slices.Equal(got.Groups, []string{"showmino:platform"}) || !got.CreatedAt.Equal(at) || !got.ExpiresAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("by token: %+v %v", got, err)
	}
	if _, err := s.SessionByToken(ctx, "hash-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown token: %v", err)
	}
	// The ID is not the token.
	if _, err := s.SessionByToken(ctx, "ses_a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the id used as a token: %v", err)
	}

	used := at.Add(30 * time.Minute)
	if err := s.TouchSession(ctx, "ses_a", used, used.Add(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionGroups(ctx, "ses_a", nil, used); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SessionByToken(ctx, "hash-ses_a")
	if !got.LastUsedAt.Equal(used) || !got.ExpiresAt.Equal(used.Add(90*24*time.Hour)) || len(got.Groups) != 0 || !got.CheckedAt.Equal(used) {
		t.Errorf("after a use and a check: %+v", got)
	}

	// A person sees their own sessions, newest first.
	list, err := s.ListSessions(ctx, "ana")
	if err != nil || len(list) != 2 || list[0].ID != "ses_b" || list[1].ID != "ses_a" {
		t.Errorf("list: %+v %v", list, err)
	}
	if list, _ := s.ListSessions(ctx, "nobody"); len(list) != 0 {
		t.Errorf("list for nobody: %+v", list)
	}

	// Nobody deletes another person's session.
	if err := s.DeleteSession(ctx, "bo", "ses_a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting another person's session: %v", err)
	}
	if err := s.DeleteSession(ctx, "ana", "ses_b"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "ana", "ses_b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	if _, err := s.SessionByToken(ctx, "hash-ses_b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted session: %v", err)
	}

	// ses_c ends at 10:20, ses_n at 09:00, ses_a in 90 days.
	n, err := s.DeleteExpiredSessions(ctx, at.Add(80*time.Minute))
	if err != nil || n != 2 {
		t.Errorf("expired: %d %v", n, err)
	}
	if _, err := s.SessionByToken(ctx, "hash-ses_a"); err != nil {
		t.Errorf("a session that has not ended was removed: %v", err)
	}
}
