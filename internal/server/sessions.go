package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// The clients a login comes from. A session's kind is its client.
const (
	ClientCLI = "cli"
	ClientWeb = "web"
)

// sessionTokenPrefix starts every session token, so that forge-api can tell
// one from an ID token and secret scanners can tell one from noise.
const sessionTokenPrefix = "fgs_"

const (
	sessionPrefix  = "sessions/"
	currentSession = sessionPrefix + "current"
	// maxLoginAge is how old a login may be when it is turned into a
	// session: a stolen ID token is of no use for that minutes later.
	maxLoginAge = 10 * time.Minute
	// maxSessions is how many sessions one person keeps; a new one beyond
	// that ends the one unused for longest.
	maxSessions = 30
	// touchEvery is how often a use is written down: a session's last use
	// is good to the nearest few minutes.
	touchEvery = 5 * time.Minute
)

// Members tells who is in the GitHub organization and its teams
// (*github.Client with the organization permission "Members: read").
type Members interface {
	OrgMember(ctx context.Context, org, login string) (bool, error)
	TeamMember(ctx context.Context, org, slug, login string) (bool, error)
}

// SessionOptions configure sessions.
type SessionOptions struct {
	Store store.SessionStore
	// Members, with Org, lets forge-api check for itself that a session's
	// person is still in the organization and in which of Teams. Without it
	// a session keeps the groups of the login it was made from.
	Members Members
	Org     string
	// Teams are the groups ("<org>:<team slug>") to keep current: the ones
	// that grant something, that is, the writer groups.
	Teams []string
	// CLIIdle is how long a CLI session lasts unused (default 90 days), and
	// WebLifetime how long a web session lasts at most (default 7 days).
	CLIIdle, WebLifetime time.Duration
	// Recheck is how often memberships are checked (default 1h). MaxStale
	// is how long a session is trusted when GitHub cannot be asked
	// (default 24h).
	Recheck, MaxStale time.Duration
	Now               func() time.Time
	Log               *slog.Logger
}

// Sessions are forge-api's own logins: a token that works without the
// identity provider, for as long as its person stays in the organization.
type Sessions struct {
	o SessionOptions

	mu sync.Mutex
	// tried maps a session ID to the last time its memberships were asked
	// for, so that a GitHub outage is not hammered by every request.
	tried map[string]time.Time
}

// NewSessions returns the sessions kept in o.Store.
func NewSessions(o SessionOptions) *Sessions {
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.CLIIdle, 90 * 24 * time.Hour}, {&o.WebLifetime, 7 * 24 * time.Hour}, {&o.Recheck, time.Hour}, {&o.MaxStale, 24 * time.Hour}} {
		if *d.v <= 0 {
			*d.v = d.def
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Sessions{o: o, tried: map[string]time.Time{}}
}

// errSessionEnded is what every bad session token gets: unknown, signed
// out, expired and "left the organization" look the same from outside.
func errSessionEnded() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("the session has ended; sign in again ('forge auth login')"))
}

// IsSessionToken reports whether token is shaped like a session token.
func IsSessionToken(token string) bool { return strings.HasPrefix(token, sessionTokenPrefix) }

// newSessionToken returns a new session's ID and token.
func newSessionToken() (string, string, error) {
	buf := make([]byte, 40)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	return "ses_" + hex.EncodeToString(buf[:8]), sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(buf[8:]), nil
}

// Authenticate implements Authenticator for session tokens.
func (s *Sessions) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if !IsSessionToken(token) {
		return nil, errSessionEnded()
	}
	ses, err := s.o.Store.SessionByToken(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, errSessionEnded()
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the session could not be read"))
	}
	now := s.o.Now()
	if !now.Before(ses.ExpiresAt) {
		s.end(ctx, ses, "expired")
		return nil, errSessionEnded()
	}
	if ses, err = s.recheck(ctx, ses, now); err != nil {
		return nil, err
	}
	if now.Sub(ses.LastUsedAt) >= touchEvery {
		expires := ses.ExpiresAt
		if ses.Kind == ClientCLI {
			expires = now.Add(s.o.CLIIdle) // every use starts the 90 days again
		}
		if err := s.o.Store.TouchSession(ctx, ses.ID, now, expires); err != nil {
			s.o.Log.Warn("recording a session's use", "session", ses.ID, "error", err)
		}
	}
	return &Principal{Subject: ses.Subject, Email: ses.Email, Login: ses.Login, Groups: ses.Groups, Client: ses.Kind, Session: ses.ID}, nil
}

// end deletes a session that is over.
func (s *Sessions) end(ctx context.Context, ses store.Session, why string) {
	if err := s.o.Store.DeleteSession(context.WithoutCancel(ctx), ses.Subject, ses.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.o.Log.Warn("ending a session", "session", ses.ID, "error", err)
		return
	}
	s.o.Log.Info("session ended", "session", ses.ID, "email", ses.Email, "reason", why)
}

// recheck asks GitHub, when it is time, whether ses's person is still in the
// organization and which of the teams that matter they are in, and returns
// ses with its groups brought up to date. A person who left loses the
// session. When GitHub cannot be asked the session goes on with what it
// knew, for at most MaxStale.
func (s *Sessions) recheck(ctx context.Context, ses store.Session, now time.Time) (store.Session, error) {
	if s.o.Members == nil || now.Sub(ses.CheckedAt) < s.o.Recheck {
		return ses, nil
	}
	stale := func() (store.Session, error) {
		if now.Sub(ses.CheckedAt) >= s.o.MaxStale {
			return ses, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("your membership of %s on GitHub could not be confirmed for %s; try again shortly", s.o.Org, s.o.MaxStale))
		}
		return ses, nil
	}
	s.mu.Lock()
	last, tried := s.tried[ses.ID]
	if tried && now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return stale() // asked a moment ago, by this or another request
	}
	s.tried[ses.ID] = now
	s.mu.Unlock()

	groups, member, err := s.memberships(ctx, ses)
	if err != nil {
		s.o.Log.Warn("checking a session's memberships", "session", ses.ID, "error", err)
		return stale()
	}
	s.mu.Lock()
	delete(s.tried, ses.ID)
	s.mu.Unlock()
	if !member {
		s.end(ctx, ses, "no longer a member of "+s.o.Org)
		return ses, errSessionEnded()
	}
	if err := s.o.Store.SetSessionGroups(ctx, ses.ID, groups, now); err != nil {
		s.o.Log.Warn("recording a session's groups", "session", ses.ID, "error", err)
	}
	ses.Groups, ses.CheckedAt = groups, now
	return ses, nil
}

// memberships returns ses's groups with the teams that matter brought up to
// date, and whether the person is still in the organization.
func (s *Sessions) memberships(ctx context.Context, ses store.Session) ([]string, bool, error) {
	member, err := s.o.Members.OrgMember(ctx, s.o.Org, ses.Login)
	if err != nil || !member {
		return nil, false, err
	}
	groups := slices.Clone(ses.Groups)
	for _, g := range s.o.Teams {
		org, slug, ok := strings.Cut(g, ":")
		if !ok || !strings.EqualFold(org, s.o.Org) {
			continue // not a team of this organization: nothing to ask
		}
		in, err := s.o.Members.TeamMember(ctx, s.o.Org, slug, ses.Login)
		if err != nil {
			return nil, false, err
		}
		groups = slices.DeleteFunc(groups, func(have string) bool { return have == g })
		if in {
			groups = append(groups, g)
		}
	}
	slices.Sort(groups)
	return groups, true, nil
}

// RunSweeper removes ended sessions every interval until ctx is done. A
// session that has ended is refused whether or not it was swept; this only
// keeps the table small.
func (s *Sessions) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.o.Store.DeleteExpiredSessions(ctx, s.o.Now()); err != nil {
				s.o.Log.Warn("sweeping sessions", "error", err)
			} else if n > 0 {
				s.o.Log.Info("ended sessions removed", "count", n)
			}
		}
	}
}

// sessionLabel cleans what a client calls the place a session is made.
func sessionLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	return s
}

// create makes a session for p, who has just logged in.
func (s *Sessions) create(ctx context.Context, p *Principal, label string) (store.Session, string, error) {
	switch {
	case p.Workload != nil || p.Session != "":
		return store.Session{}, "", connect.NewError(connect.CodePermissionDenied, errors.New("a session is made from a login, not from a session or a CI token"))
	case p.Client != ClientCLI && p.Client != ClientWeb:
		return store.Session{}, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("this login was not made by the Forge CLI or the dashboard"))
	case p.IssuedAt.IsZero() || s.o.Now().Sub(p.IssuedAt) > maxLoginAge:
		return store.Session{}, "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the login is more than %s old; sign in again", maxLoginAge))
	case s.o.Members != nil && p.Login == "":
		return store.Session{}, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the login does not say which GitHub user it is, so its memberships could not be kept current"))
	}
	id, token, err := newSessionToken()
	if err != nil {
		return store.Session{}, "", connect.NewError(connect.CodeInternal, errors.New("no random bytes for a session"))
	}
	now := s.o.Now()
	ses := store.Session{
		ID: id, TokenHash: hashToken(token), Kind: p.Client, Label: sessionLabel(label),
		Subject: p.Subject, Email: p.Email, Login: p.Login, Groups: slices.Clone(p.Groups),
		CreatedAt: now, LastUsedAt: now, CheckedAt: now, ExpiresAt: now.Add(s.o.WebLifetime),
	}
	if ses.Kind == ClientCLI {
		ses.ExpiresAt = now.Add(s.o.CLIIdle)
	}
	if err := s.o.Store.CreateSession(ctx, ses); err != nil {
		return store.Session{}, "", storeErr(err)
	}
	s.trim(ctx, p.Subject, now)
	return ses, token, nil
}

// trim removes subject's ended sessions and, beyond maxSessions, the ones
// unused for longest.
func (s *Sessions) trim(ctx context.Context, subject string, now time.Time) {
	all, err := s.o.Store.ListSessions(ctx, subject)
	if err != nil {
		return
	}
	slices.SortFunc(all, func(a, b store.Session) int { return b.LastUsedAt.Compare(a.LastUsedAt) })
	for i, ses := range all {
		if i >= maxSessions || !now.Before(ses.ExpiresAt) {
			_ = s.o.Store.DeleteSession(ctx, subject, ses.ID)
		}
	}
}

func sessionProto(ses store.Session, current string) *forgev1.Session {
	client := forgev1.Session_CLIENT_CLI
	if ses.Kind == ClientWeb {
		client = forgev1.Session_CLIENT_WEB
	}
	return &forgev1.Session{
		Name: sessionPrefix + ses.ID, Client: client, Label: ses.Label,
		CreateTime: timestamppb.New(ses.CreatedAt), LastUseTime: timestamppb.New(ses.LastUsedAt), ExpireTime: timestamppb.New(ses.ExpiresAt),
		Current: ses.ID == current,
	}
}

// sessionsOff is the answer of a Forge API that keeps no sessions. Clients
// read Unimplemented as "use the login as it is".
func sessionsOff() error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("this Forge API keeps no sessions"))
}

func (a *authService) CreateSession(ctx context.Context, req *connect.Request[forgev1.CreateSessionRequest]) (*connect.Response[forgev1.CreateSessionResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	ses, token, err := a.o.Sessions.create(ctx, p, req.Msg.GetLabel())
	if err != nil {
		return nil, err
	}
	noteAuditTarget(ctx, sessionPrefix+ses.ID)
	return connect.NewResponse(&forgev1.CreateSessionResponse{Session: sessionProto(ses, ""), Token: token}), nil
}

func (a *authService) ListSessions(ctx context.Context, _ *connect.Request[forgev1.ListSessionsRequest]) (*connect.Response[forgev1.ListSessionsResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Workload != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	all, err := a.o.Sessions.o.Store.ListSessions(ctx, p.Subject)
	if err != nil {
		return nil, storeErr(err)
	}
	now := a.o.Sessions.o.Now()
	out := &forgev1.ListSessionsResponse{}
	for _, ses := range all {
		if now.Before(ses.ExpiresAt) {
			out.Sessions = append(out.Sessions, sessionProto(ses, p.Session))
		}
	}
	return connect.NewResponse(out), nil
}

func (a *authService) DeleteSession(ctx context.Context, req *connect.Request[forgev1.DeleteSessionRequest]) (*connect.Response[forgev1.DeleteSessionResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Workload != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	name := req.Msg.GetName()
	if name == currentSession {
		if p.Session == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this call was not made with a session"))
		}
		name = sessionPrefix + p.Session
	}
	id, ok := strings.CutPrefix(name, sessionPrefix)
	if !ok || id == "" {
		return nil, invalidArg("session %q must be sessions/<id>", req.Msg.GetName())
	}
	noteAuditTarget(ctx, sessionPrefix+id)
	err := a.o.Sessions.o.Store.DeleteSession(ctx, p.Subject, id)
	if errors.Is(err, store.ErrNotFound) {
		// Another person's session and no session look the same.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", sessionPrefix, id))
	}
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.DeleteSessionResponse{}), nil
}

// WithSessions returns an Authenticator that reads session tokens with s and
// hands every other token to next.
func WithSessions(s *Sessions, next Authenticator) Authenticator {
	if s == nil {
		return next
	}
	return sessionFirst{s: s, next: next}
}

type sessionFirst struct {
	s    *Sessions
	next Authenticator
}

func (f sessionFirst) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if IsSessionToken(token) {
		return f.s.Authenticate(ctx, token)
	}
	return f.next.Authenticate(ctx, token)
}
