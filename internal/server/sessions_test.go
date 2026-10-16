package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

// logins plays the identity provider: fixed tokens that are ID tokens of a
// login made age ago (zero: just now).
type logins struct {
	now     func() time.Time
	byToken map[string]Principal
	age     map[string]time.Duration
}

func (l logins) Authenticate(_ context.Context, token string) (*Principal, error) {
	p, ok := l.byToken[token]
	if !ok {
		return nil, errors.New("unknown token")
	}
	c := p
	c.Groups = slices.Clone(p.Groups)
	if c.Client != "" {
		c.IssuedAt = l.now().Add(-l.age[token])
	}
	return &c, nil
}

// fakeMembers is GitHub's view of who is in the organization and the team.
type fakeMembers struct {
	mu        sync.Mutex
	org, team map[string]bool
	err       error
	calls     int
	// slow is how long GitHub takes to answer.
	slow time.Duration
}

func (m *fakeMembers) OrgMember(ctx context.Context, _, login string) (bool, error) {
	m.mu.Lock()
	m.calls++
	slow := m.slow
	m.mu.Unlock()
	time.Sleep(slow)
	if err := ctx.Err(); err != nil {
		return false, err // as an HTTP client: a caller that left gets no answer
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.org[login], m.err
}

func (m *fakeMembers) TeamMember(_ context.Context, _, slug, login string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slug == "platform" && m.team[login], m.err
}

func (m *fakeMembers) set(f func(*fakeMembers)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

type sessionFixture struct {
	t       *testing.T
	mu      sync.Mutex
	clock   time.Time
	store   *store.Memory
	members *fakeMembers
	auth    forgev1connect.AuthServiceClient
}

func (f *sessionFixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *sessionFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{
		t: t, clock: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), store: store.NewMemory(),
		members: &fakeMembers{org: map[string]bool{"ana": true, "bo": true}, team: map[string]bool{"ana": true}},
	}
	ana := Principal{Subject: "sub-ana", Email: "ana@example.com", Login: "ana", Groups: []string{"showmino:other", "showmino:platform"}}
	with := func(p Principal, client string) Principal { p.Client = client; return p }
	h, err := New(Options{
		Authenticator: logins{now: f.now, age: map[string]time.Duration{"old-login": time.Hour}, byToken: map[string]Principal{
			"cli-login": with(ana, ClientCLI), "web-login": with(ana, ClientWeb), "old-login": with(ana, ClientCLI),
			"bo-login":    {Subject: "sub-bo", Email: "bo@example.com", Login: "bo", Groups: []string{"showmino:other"}, Client: ClientCLI},
			"other-login": ana, // not made by a Forge client
			"no-name":     {Subject: "sub-cy", Email: "cy@example.com", Client: ClientCLI},
			"once-login":  {Subject: "sub-ana", Email: "ana@example.com", Login: "ana", Client: ClientCLI, LoginID: "the hash of one ID token"},
			"ci":          {Subject: "github:showmino/payments", Workload: &Workload{Repository: "showmino/payments", Service: "payments"}},
		}},
		Authorizer: Authorizer{WriterGroups: []string{"showmino:platform"}},
		Audit:      f.store,
		Sessions: NewSessions(SessionOptions{
			Store: f.store, Members: f.members, Org: "showmino", Teams: []string{"showmino:platform", "elsewhere:team"}, Now: f.now,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.auth = forgev1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	return f
}

// login turns a login into a session and returns its token and name.
func (f *sessionFixture) login(token, label string) (string, *forgev1.Session) {
	f.t.Helper()
	resp, err := f.auth.CreateSession(context.Background(), as(token, &forgev1.CreateSessionRequest{Label: label}))
	if err != nil {
		f.t.Fatalf("create a session from %s: %v", token, err)
	}
	return resp.Msg.GetToken(), resp.Msg.GetSession()
}

func (f *sessionFixture) status(token string) (*forgev1.GetStatusResponse, error) {
	resp, err := f.auth.GetStatus(context.Background(), as(token, &forgev1.GetStatusRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func canWrite(st *forgev1.GetStatusResponse) bool {
	return slices.Contains(st.GetPermissions(), PermServicesWrite)
}

func TestSessionCreateAndUse(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	ctx := context.Background()

	token, ses := f.login("cli-login", "  ana's\x00 laptop\n"+strings.Repeat("x", 200))
	if !strings.HasPrefix(token, "fgs_") || len(token) < 40 || !strings.HasPrefix(ses.GetName(), "sessions/ses_") || ses.GetClient() != forgev1.Session_CLIENT_CLI {
		t.Fatalf("session %v, token of %d characters", ses, len(token))
	}
	if l := ses.GetLabel(); !strings.HasPrefix(l, "ana's laptopxxx") || len([]rune(l)) != 100 {
		t.Errorf("label %q", l)
	}
	if got := ses.GetExpireTime().AsTime(); !got.Equal(f.now().Add(90 * 24 * time.Hour)) {
		t.Errorf("a CLI session ends %v", got)
	}
	// Only the token's hash is kept.
	if _, err := f.store.SessionByToken(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the token itself is in the store: %v", err)
	}
	if _, err := f.store.SessionByToken(ctx, hashToken(token)); err != nil {
		t.Errorf("the token's hash is not in the store: %v", err)
	}

	st, err := f.status(token)
	if err != nil || st.GetEmail() != "ana@example.com" || st.GetSubject() != "sub-ana" || !canWrite(st) ||
		st.GetSession().GetName() != ses.GetName() || !st.GetSession().GetCurrent() {
		t.Fatalf("status with the session: %v %v", st, err)
	}
	// A call with an ID token has no session.
	if st, _ := f.status("cli-login"); st.GetSession() != nil {
		t.Errorf("status with a login: %v", st.GetSession())
	}

	for name, bad := range map[string]string{"made up": "fgs_" + strings.Repeat("A", 43), "the id": strings.TrimPrefix(ses.GetName(), "sessions/"), "empty": "fgs_"} {
		if _, err := f.status(bad); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s token: %v", name, err)
		}
	}

	// One login makes one session: a copy of its ID token is of no use.
	if _, ses := f.login("once-login", ""); ses.GetName() == "" {
		t.Fatal("no session from a login used once")
	}
	if _, err := f.auth.CreateSession(ctx, as("once-login", &forgev1.CreateSessionRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already turned into a session") {
		t.Errorf("a login used twice: %v", err)
	}
	// Clients are told the API keeps sessions, and the audit log says which
	// client a session was made for, not what it was called.
	if info, err := f.auth.GetServerInfo(ctx, connect.NewRequest(&forgev1.GetServerInfoRequest{})); err != nil || !info.Msg.GetSessions() {
		t.Errorf("server info: %v %v", info, err)
	}
	evs, err := f.store.ListAuditEvents(ctx, store.AuditQuery{Action: "auth.session.create", Limit: 10})
	if err != nil || len(evs) == 0 {
		t.Fatalf("audit events: %v %v", evs, err)
	}
	for _, ev := range evs {
		if ev.GetResult().GetCode() == "ok" && (!strings.HasPrefix(ev.GetTarget(), "sessions/ses_") || ev.GetDetails()["client"] != "cli" || strings.Contains(ev.String(), "laptop") || strings.Contains(ev.String(), "fgs_")) {
			t.Errorf("audit event %v", ev)
		}
	}

	// What a session cannot be made from.
	for name, tc := range map[string]struct {
		token string
		code  connect.Code
	}{
		"a session":                     {token, connect.CodePermissionDenied},
		"an old login":                  {"old-login", connect.CodeFailedPrecondition},
		"a login by another client":     {"other-login", connect.CodeFailedPrecondition},
		"a login without a GitHub name": {"no-name", connect.CodeFailedPrecondition},
		"a CI token":                    {"ci", connect.CodePermissionDenied},
		"no token":                      {"nope", connect.CodeUnauthenticated},
	} {
		if _, err := f.auth.CreateSession(ctx, as(tc.token, &forgev1.CreateSessionRequest{})); connect.CodeOf(err) != tc.code {
			t.Errorf("a session from %s: %v, want %v", name, err, tc.code)
		}
	}
}

func TestSessionLifetimes(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	day := 24 * time.Hour

	cli, _ := f.login("cli-login", "laptop")
	web, ws := f.login("web-login", "Firefox")
	if ws.GetClient() != forgev1.Session_CLIENT_WEB || !ws.GetExpireTime().AsTime().Equal(f.now().Add(7*day)) {
		t.Fatalf("web session %v", ws)
	}

	// A web session ends 7 days after the sign-in, used or not.
	f.advance(6 * day)
	if _, err := f.status(web); err != nil {
		t.Fatalf("web session on day 6: %v", err)
	}
	f.advance(day + time.Minute)
	if _, err := f.status(web); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("web session after 7 days: %v", err)
	}
	if list, _ := f.store.ListSessions(context.Background(), "sub-ana"); len(list) != 1 {
		t.Errorf("the ended web session is still stored: %d sessions", len(list))
	}

	// A CLI session ends 90 days after its last use: every use starts again.
	f.advance(80 * day)
	if _, err := f.status(cli); err != nil {
		t.Fatalf("CLI session used after 87 days: %v", err)
	}
	f.advance(80 * day)
	if st, err := f.status(cli); err != nil || !st.GetSession().GetExpireTime().AsTime().After(f.now().Add(89*day)) {
		t.Fatalf("CLI session used again 80 days later: %v %v", st, err)
	}
	f.advance(91 * day)
	if _, err := f.status(cli); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("CLI session unused for 91 days: %v", err)
	}
}

func TestSessionMembership(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	token, _ := f.login("cli-login", "laptop")
	calls := func() int {
		f.members.mu.Lock()
		defer f.members.mu.Unlock()
		return f.members.calls
	}

	// Within the hour forge-api does not ask GitHub.
	f.advance(30 * time.Minute)
	if st, err := f.status(token); err != nil || !canWrite(st) || calls() != 0 {
		t.Fatalf("after 30 minutes: %v %v, %d calls", st, err, calls())
	}
	// Taken out of the writer team: the next check takes writing away, and
	// keeps the groups forge-api cannot check.
	f.members.set(func(m *fakeMembers) { m.team["ana"] = false })
	f.advance(31 * time.Minute)
	st, err := f.status(token)
	if err != nil || canWrite(st) || !slices.Equal(st.GetGroups(), []string{"showmino:other"}) {
		t.Fatalf("after leaving the team: %v %v", st, err)
	}
	// Put back: writing returns at the next check, not before.
	f.members.set(func(m *fakeMembers) { m.team["ana"] = true })
	if st, _ := f.status(token); canWrite(st) {
		t.Error("the team was asked again within the hour")
	}
	f.advance(61 * time.Minute)
	if st, err := f.status(token); err != nil || !canWrite(st) {
		t.Fatalf("after joining again: %v %v", st, err)
	}

	// GitHub is down: the session goes on with what it knew, and GitHub is
	// asked at most once a minute, not by every request.
	f.members.set(func(m *fakeMembers) { m.err = errors.New("502") })
	f.advance(61 * time.Minute)
	before := calls()
	for range 5 {
		if st, err := f.status(token); err != nil || !canWrite(st) {
			t.Fatalf("with GitHub down: %v %v", st, err)
		}
	}
	if got := calls() - before; got != 1 {
		t.Errorf("GitHub was asked %d times in one minute", got)
	}
	// Down for a day: the session is no longer trusted, but it is not lost.
	f.advance(24 * time.Hour)
	if _, err := f.status(token); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("GitHub down for a day: %v", err)
	}
	f.members.set(func(m *fakeMembers) { m.err = nil })
	f.advance(2 * time.Minute)
	if st, err := f.status(token); err != nil || !canWrite(st) {
		t.Fatalf("GitHub back: %v %v", st, err)
	}

	// Out of the organization: the session ends.
	f.members.set(func(m *fakeMembers) { m.org["ana"] = false })
	f.advance(61 * time.Minute)
	if _, err := f.status(token); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("after leaving the organization: %v", err)
	}
	if list, _ := f.store.ListSessions(context.Background(), "sub-ana"); len(list) != 0 {
		t.Errorf("sessions of someone who left: %d", len(list))
	}
	// forge-api ended it on its own, and says so in the audit log.
	evs, err := f.store.ListAuditEvents(context.Background(), store.AuditQuery{Action: "auth.session.end", Limit: 10})
	if err != nil || len(evs) != 1 || evs[0].GetActor().GetKind() != actorSystem || !strings.HasPrefix(evs[0].GetTarget(), "sessions/ses_") ||
		evs[0].GetDetails()["email"] != "ana@example.com" || evs[0].GetDetails()["reason"] != "no longer a member of showmino" {
		t.Errorf("audit of the ended session: %v %v", evs, err)
	}
}

// A caller cannot make the check fail by leaving early, and so keep rights
// its person lost: the check has its own time limit.
func TestSessionCheckOutlivesTheCaller(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	token, _ := f.login("cli-login", "laptop")
	f.members.set(func(m *fakeMembers) { m.org["ana"], m.slow = false, 20*time.Millisecond })
	f.advance(61 * time.Minute)
	sessions := NewSessions(SessionOptions{Store: f.store, Members: f.members, Org: "showmino", Teams: []string{"showmino:platform"}, Now: f.now})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sessions.Authenticate(gone, token); err == nil {
		t.Fatal("a caller that left was let in")
	}
	// The check went on without the caller, and ended the session.
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if list, _ := f.store.ListSessions(context.Background(), "sub-ana"); len(list) == 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the session of someone who left is still there")
		}
	}
	if _, err := f.status(token); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("after the check: %v", err)
	}
}

// A page that makes many calls at once, on a session not used for days:
// they all wait for the one check and get in.
func TestSessionParallelCheck(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	token, _ := f.login("web-login", "Firefox")
	f.members.set(func(m *fakeMembers) { m.slow = 30 * time.Millisecond })
	f.advance(3 * 24 * time.Hour)
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Go(func() {
			st, err := f.status(token)
			if err == nil && !canWrite(st) {
				err = errors.New("no write permission")
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	f.members.mu.Lock()
	defer f.members.mu.Unlock()
	if f.members.calls < 1 || f.members.calls > 3 {
		t.Errorf("GitHub was asked %d times for one session", f.members.calls)
	}
}

func TestSessionListAndDelete(t *testing.T) {
	t.Parallel()
	f := newSessionFixture(t)
	ctx := context.Background()
	laptop, ls := f.login("cli-login", "laptop")
	f.advance(time.Minute)
	browser, bs := f.login("web-login", "Firefox")
	boToken, _ := f.login("bo-login", "bo's laptop")

	list, err := f.auth.ListSessions(ctx, as(laptop, &forgev1.ListSessionsRequest{}))
	got := list.Msg.GetSessions()
	if err != nil || len(got) != 2 || got[0].GetName() != bs.GetName() || got[0].GetCurrent() || got[1].GetName() != ls.GetName() || !got[1].GetCurrent() {
		t.Fatalf("ana's sessions: %v %v", got, err)
	}
	// A login lists them too, with none current.
	if list, err := f.auth.ListSessions(ctx, as("cli-login", &forgev1.ListSessionsRequest{})); err != nil || len(list.Msg.GetSessions()) != 2 || list.Msg.GetSessions()[1].GetCurrent() {
		t.Errorf("listed with a login: %v %v", list, err)
	}

	// Nobody ends another person's session, or learns that it exists.
	del := func(token, name string) error {
		_, err := f.auth.DeleteSession(ctx, as(token, &forgev1.DeleteSessionRequest{Name: name}))
		return err
	}
	if err := del(boToken, ls.GetName()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bo ends ana's session: %v", err)
	}
	if err := del(laptop, "sessions/ses_nope"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a session that does not exist: %v", err)
	}
	for _, bad := range []string{"", "ses_x", "sessions/"} {
		if err := del(laptop, bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	if err := del("cli-login", "sessions/current"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("'current' with a login: %v", err)
	}

	// Ending one from another session, and signing out of the current one.
	if err := del(laptop, bs.GetName()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.status(browser); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the ended session still works: %v", err)
	}
	if err := del(laptop, "sessions/current"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.status(laptop); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("after signing out: %v", err)
	}
	if _, err := f.status(boToken); err != nil {
		t.Errorf("bo's session ended with ana's: %v", err)
	}
}

func TestSessionsOffAndTrim(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Authenticator: StaticAuthenticator{"tok": {Subject: "ana", Client: ClientCLI}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	off := forgev1connect.NewAuthServiceClient(srv.Client(), srv.URL)
	if _, err := off.CreateSession(context.Background(), as("tok", &forgev1.CreateSessionRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("a Forge API without sessions: %v", err)
	}
	// A session token means nothing to it.
	if _, err := off.GetStatus(context.Background(), as("fgs_abc", &forgev1.GetStatusRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a session token without sessions: %v", err)
	}

	// One person keeps at most maxSessions; the one unused for longest
	// goes, which is not the same as the oldest.
	f := newSessionFixture(t)
	first, _ := f.login("cli-login", "first")
	f.advance(time.Minute)
	second, _ := f.login("cli-login", "second")
	for range maxSessions - 2 {
		f.advance(time.Minute)
		f.login("cli-login", "more")
	}
	f.advance(10 * time.Minute)
	if _, err := f.status(first); err != nil { // the oldest is used again
		t.Fatal(err)
	}
	f.login("cli-login", "one too many")
	if list, _ := f.store.ListSessions(context.Background(), "sub-ana"); len(list) != maxSessions {
		t.Errorf("%d sessions kept, want %d", len(list), maxSessions)
	}
	if _, err := f.status(first); err != nil {
		t.Errorf("the oldest session, used a moment ago, was removed: %v", err)
	}
	if _, err := f.status(second); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the session unused for longest survived: %v", err)
	}
}

// The identity provider's token decides what kind of login it is: which
// client it was issued to, how old it is and which GitHub user it names.
func TestOIDCLoginFacts(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	ctx := context.Background()
	a := &OIDCAuthenticator{
		verifier:  oidc.NewVerifier(iss.srv.URL, oidc.NewRemoteKeySet(ctx, iss.srv.URL+"/.well-known/jwks"), &oidc.Config{SkipClientIDCheck: true}),
		audiences: []string{"forge-cli", "forge-web"},
	}
	a.SetWebClient("forge-web")
	issued := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	token := func(aud any) string {
		return iss.sign(t, iss.key, map[string]any{
			"iss": iss.srv.URL, "aud": aud, "sub": "sub-ana", "email": "ana@example.com", "preferred_username": "ana-gh",
			"groups": []string{"showmino:platform"}, "iat": issued.Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	cli := token("forge-cli")
	p, err := a.Authenticate(ctx, cli)
	if err != nil || p.Client != ClientCLI || p.Login != "ana-gh" || !p.IssuedAt.Equal(issued) || p.LoginID != hashToken(cli) || p.Session != "" || p.Email != "ana@example.com" {
		t.Fatalf("a CLI login: %+v %v", p, err)
	}
	// A token issued to the dashboard is a web login, also when it names both.
	for _, aud := range []any{"forge-web", []string{"forge-cli", "forge-web"}} {
		if p, err := a.Authenticate(ctx, token(aud)); err != nil || p.Client != ClientWeb {
			t.Errorf("audience %v: %+v %v", aud, p, err)
		}
	}
	if _, err := a.Authenticate(ctx, token("someone-else")); err == nil {
		t.Error("a token for another client was accepted")
	}
}

// slugMembers says who is in which team of the organization.
type slugMembers map[string][]string

func (m slugMembers) OrgMember(context.Context, string, string) (bool, error) { return true, nil }
func (m slugMembers) TeamMember(_ context.Context, _, slug, login string) (bool, error) {
	return slices.Contains(m[slug], login), nil
}

// A service's team is rechecked like the admin groups, so a person who left
// the team loses its services within the hour, and one who joined gains them.
func TestSessionRechecksOwningTeams(t *testing.T) {
	t.Parallel()
	s := NewSessions(SessionOptions{
		Store: store.NewMemory(), Org: "showmino", Teams: []string{"showmino:platform"},
		Members: slugMembers{"billing": {"ana"}, "frontend": {"ana"}},
		OwningTeams: func(context.Context) ([]string, error) {
			return []string{"billing", "frontend", "search"}, nil
		},
	})
	got, ok, err := s.memberships(context.Background(), store.Session{Login: "ana", Groups: []string{"showmino:platform", "showmino:search", "showmino:unknown"}})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := []string{"showmino:billing", "showmino:frontend", "showmino:unknown"}
	if !slices.Equal(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
}
