package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
)

// Personal tokens (phase 15, milestone F): for CI and scripts. A token is a
// session of kind "token": only its hash is kept, its person's memberships
// are rechecked as a login's are, and it acts as its person, never with more
// than their role and only within its scopes.

const (
	// ClientToken is the Client of a call made with a personal token.
	ClientToken = "token"
	// tokenPrefix starts a personal token; sessions start with fgs_.
	tokenPrefix = "fgp_"
	tokenName   = "tokens/"
	// maxTokens is how many tokens one person may have.
	maxTokens = 20
	// maxTokenDays is the longest expiry but never.
	maxTokenDays = 365
)

// Scopes of a token.
const (
	ScopeRead     = "read"
	ScopeDeploy   = "deploy"
	ScopeSettings = "settings"
)

// tokenNever is the expiry of a token that does not expire.
var tokenNever = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// tokenLabel is a token's name: a word for what it is for.
var tokenLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// deployProcedures are the calls the deploy scope adds to reading.
var deployProcedures = []string{
	forgev1connect.DeployServiceStartDeployProcedure,
	forgev1connect.DeployServicePromoteDeployProcedure,
	forgev1connect.DeployServiceRollbackDeployProcedure,
	forgev1connect.EnvironmentServiceCreateEnvironmentProcedure,
	forgev1connect.EnvironmentServiceExtendEnvironmentProcedure,
	forgev1connect.EnvironmentServiceDestroyEnvironmentProcedure,
	forgev1connect.OperationServiceCancelOperationProcedure,
}

// credentialProcedures make or end logins and tokens: a token never may. A
// token that leaks must not make another, or outlive its revocation.
var credentialProcedures = []string{
	forgev1connect.AuthServiceCreateSessionProcedure,
	forgev1connect.AuthServiceDeleteSessionProcedure,
	forgev1connect.AuthServiceCreateTokenProcedure,
	forgev1connect.AuthServiceDeleteTokenProcedure,
}

// humanProcedures are decisions a person makes themselves: approving or
// refusing a production run, the exceptions to that, the rules' modes and
// who is on call. A script holding a token would remove the human gate they
// exist for, so no token may, whatever its scopes.
var humanProcedures = []string{
	forgev1connect.ApprovalServiceApproveApprovalProcedure,
	forgev1connect.ApprovalServiceRejectApprovalProcedure,
	forgev1connect.ApprovalServiceRequestExceptionProcedure,
	forgev1connect.ApprovalServiceRevokeExceptionProcedure,
	forgev1connect.PolicyServiceUpdateRuleProcedure,
	forgev1connect.TeamServiceUpdateTeamProcedure,
}

// tokenID is a token's name as Forge makes it: tokens/tok_ and 16 hex digits.
var tokenID = regexp.MustCompile(`^tokens/tok_[0-9a-f]{16}$`)

// validTokenName reports whether name may name a token: a short word, never
// one that looks like a secret (fgp_..., fgs_...).
func validTokenName(name string) bool {
	return tokenLabel.MatchString(name) && !strings.HasPrefix(name, tokenPrefix) && !strings.HasPrefix(name, sessionTokenPrefix)
}

// IsPersonalToken reports whether token is a personal token.
func IsPersonalToken(token string) bool { return strings.HasPrefix(token, tokenPrefix) }

// tokenMayCall reports whether a token with scopes may call procedure. A
// read needs no scope; a call that is neither a known read nor a known
// write is refused.
func tokenMayCall(scopes []string, procedure string) bool {
	switch {
	case slices.Contains(credentialProcedures, procedure), slices.Contains(humanProcedures, procedure):
		return false
	case slices.Contains(auditReads, procedure):
		return true
	case slices.Contains(deployProcedures, procedure):
		return slices.Contains(scopes, ScopeDeploy) || slices.Contains(scopes, ScopeSettings)
	}
	if _, write := auditActions[procedure]; write {
		return slices.Contains(scopes, ScopeSettings)
	}
	return false
}

// tokenScopes checks the asked scopes and returns them in order, read first.
func tokenScopes(asked []string) ([]string, error) {
	for _, s := range asked {
		if s != ScopeRead && s != ScopeDeploy && s != ScopeSettings {
			return nil, invalidArg("scope %q must be read, deploy or settings", s)
		}
	}
	out := []string{ScopeRead}
	for _, s := range []string{ScopeDeploy, ScopeSettings} {
		if slices.Contains(asked, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func newPersonalToken() (string, string, error) {
	buf := make([]byte, 40)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	return "tok_" + hex.EncodeToString(buf[:8]), tokenPrefix + base64.RawURLEncoding.EncodeToString(buf[8:]), nil
}

func tokenProto(t store.Session) *forgev1.Token {
	out := &forgev1.Token{
		Name: tokenName + t.ID, DisplayName: t.Label, Scopes: slices.Clone(t.Scopes),
		CreateTime: timestamppb.New(t.CreatedAt), LastUseTime: timestamppb.New(t.LastUsedAt),
	}
	if t.ExpiresAt.Before(tokenNever) {
		out.ExpireTime = timestamppb.New(t.ExpiresAt)
	}
	return out
}

// loginOnly refuses a call that was not made by a person's login.
func loginOnly(ctx context.Context) (*Principal, error) {
	p, ok := PrincipalFrom(ctx)
	switch {
	case !ok:
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	case p.Workload != nil || p.Client == ClientToken:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("tokens are made and revoked by a person's login, not by a token or a CI job"))
	}
	return p, nil
}

func (a *authService) CreateToken(ctx context.Context, req *connect.Request[forgev1.CreateTokenRequest]) (*connect.Response[forgev1.CreateTokenResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, err := loginOnly(ctx)
	if err != nil {
		return nil, err
	}
	// Only a Forge session made from a fresh login (the CLI's or the
	// dashboard's) makes a token: a copied ID token, of any age, must not
	// become a credential that outlives it.
	if p.Session == "" || (p.Client != ClientCLI && p.Client != ClientWeb) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("a token is made from a Forge session: log in with 'forge auth login' or the dashboard"))
	}
	m := req.Msg
	if !validTokenName(m.GetDisplayName()) {
		return nil, invalidArg("display_name must be a short name for what it is for: lowercase letters, digits, '.', '_' and '-', at most 63")
	}
	scopes, err := tokenScopes(m.GetScopes())
	if err != nil {
		return nil, err
	}
	if d := m.GetExpireDays(); d < 0 || d > maxTokenDays {
		return nil, invalidArg("expire_days %d must be 1 to %d, or 0 for never", d, maxTokenDays)
	}
	s := a.o.Sessions
	now := s.o.Now()
	mine, err := s.tokens(ctx, p.Subject, now)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(mine, func(t store.Session) bool { return t.Label == m.GetDisplayName() }) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("you already have a token called %s: revoke it first, or pick another name", m.GetDisplayName()))
	}
	if len(mine) >= maxTokens {
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("you have %d tokens, the most one person may have: revoke one first", len(mine)))
	}
	if s.o.Members != nil && p.Login == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the login does not say which GitHub user it is, so the token's memberships could not be kept current"))
	}
	// The groups come from the creating session, as last checked: the token
	// keeps that check's time, not a new one.
	checked := now
	if all, err := s.o.Store.ListSessions(ctx, p.Subject); err == nil {
		for _, ses := range all {
			if ses.ID == p.Session {
				checked = ses.CheckedAt
			}
		}
	}
	id, secret, err := newPersonalToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no random bytes for a token"))
	}
	t := store.Session{
		ID: id, TokenHash: hashToken(secret), Kind: ClientToken, Label: m.GetDisplayName(), Scopes: scopes,
		Subject: p.Subject, Email: p.Email, Login: p.Login, Groups: slices.Clone(p.Groups),
		CreatedAt: now, LastUsedAt: now, CheckedAt: checked, ExpiresAt: tokenNever,
	}
	if d := m.GetExpireDays(); d > 0 {
		t.ExpiresAt = now.Add(time.Duration(d) * 24 * time.Hour)
	}
	noteAuditTarget(ctx, tokenName+id)
	noteAuditDetail(ctx, "scopes", strings.Join(scopes, ","))
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.CreateTokenResponse{Token: tokenProto(t)}), nil
	}
	if err := s.o.Store.CreateSession(ctx, t); err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.CreateTokenResponse{Token: tokenProto(t), Secret: secret}), nil
}

// tokens are subject's tokens that have not ended, newest first.
func (s *Sessions) tokens(ctx context.Context, subject string, now time.Time) ([]store.Session, error) {
	all, err := s.o.Store.ListSessions(ctx, subject)
	if err != nil {
		return nil, storeErr(err)
	}
	var out []store.Session
	for _, t := range all {
		if t.Kind == ClientToken && now.Before(t.ExpiresAt) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (a *authService) ListTokens(ctx context.Context, _ *connect.Request[forgev1.ListTokensRequest]) (*connect.Response[forgev1.ListTokensResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Workload != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	mine, err := a.o.Sessions.tokens(ctx, p.Subject, a.o.Sessions.o.Now())
	if err != nil {
		return nil, err
	}
	out := &forgev1.ListTokensResponse{}
	for _, t := range mine {
		out.Tokens = append(out.Tokens, tokenProto(t))
	}
	return connect.NewResponse(out), nil
}

func (a *authService) DeleteToken(ctx context.Context, req *connect.Request[forgev1.DeleteTokenRequest]) (*connect.Response[forgev1.DeleteTokenResponse], error) {
	if a.o.Sessions == nil {
		return nil, sessionsOff()
	}
	p, err := loginOnly(ctx)
	if err != nil {
		return nil, err
	}
	// The name is not echoed: whatever was pasted here (a token's secret,
	// say) stays out of answers and logs.
	if !tokenID.MatchString(req.Msg.GetName()) {
		return nil, invalidArg("a token's name looks like tokens/tok_<16 hex digits>; 'forge token list' shows them")
	}
	id := strings.TrimPrefix(req.Msg.GetName(), tokenName)
	noteAuditTarget(ctx, tokenName+id)
	err = a.o.Sessions.o.Store.DeleteSession(ctx, p.Subject, id)
	if errors.Is(err, store.ErrNotFound) {
		// Another person's token and no token look the same.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", tokenName, id))
	}
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.DeleteTokenResponse{}), nil
}

// deployOnlyToken reports whether the call was made with a token that may
// deploy but not change settings: the handlers that do more than deploy
// with a deploy call (cancelling any operation, new chart values) refuse it.
func deployOnlyToken(ctx context.Context) (string, bool) {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Client != ClientToken || slices.Contains(p.Scopes, ScopeSettings) {
		return "", false
	}
	return p.TokenName, true
}

// deployScopeKinds are the operations a deploy-only token may cancel.
var deployScopeKinds = map[string]bool{kindDeploy: true, kindPromote: true, kindRollback: true, kindCreateEnv: true, kindDestroyEnv: true}
