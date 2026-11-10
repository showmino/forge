package store

import (
	"context"
	"time"
)

// Session is a login forge-api keeps for one client. Only the hash of its
// token is stored.
type Session struct {
	// ID is "ses_<hex>"; it names the session and is not a secret.
	ID string
	// TokenHash is the hex SHA-256 of the session's token.
	TokenHash string
	// Kind is "cli" or "web" for a login, "token" for a personal token
	// (phase 15), whose Label is its name.
	Kind string
	// Label is where the client said the session was made.
	Label string
	// Subject, Email and Login (the GitHub user name) are who logged in.
	Subject, Email, Login string
	// Groups are the person's groups as of CheckedAt.
	Groups []string
	// Scopes limit what a token may do ("read", "deploy", "settings"); nil
	// for a login, which may do all its person may.
	Scopes []string

	CreatedAt, LastUsedAt, ExpiresAt, CheckedAt time.Time
}

// SessionStore keeps sessions. A session that ended (logout, expiry, a
// person who left) is deleted: there is no "revoked" state to forget to check.
type SessionStore interface {
	// CreateSession stores s.
	CreateSession(ctx context.Context, s Session) error
	// SessionByToken returns the session with this token hash, expired or
	// not, or ErrNotFound.
	SessionByToken(ctx context.Context, tokenHash string) (Session, error)
	// TouchSession records a use: when, and when the session now ends.
	TouchSession(ctx context.Context, id string, lastUsed, expires time.Time) error
	// SetSessionGroups records the person's groups as checked at checked.
	SetSessionGroups(ctx context.Context, id string, groups []string, checked time.Time) error
	// ListSessions returns subject's sessions, newest first.
	ListSessions(ctx context.Context, subject string) ([]Session, error)
	// DeleteSession removes subject's session id, or returns ErrNotFound
	// when subject has no such session.
	DeleteSession(ctx context.Context, subject, id string) error
	// DeleteExpiredSessions removes the sessions that ended before now and
	// returns how many.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}
