// Package auth implements 'forge auth login' with the OIDC device
// authorization flow (RFC 8628), credential storage and token refresh.
//
// The CLI sends the OIDC ID token as its bearer token: the API verifies it
// against the issuer with the CLI's client ID as audience. Refresh tokens
// (scope offline_access) renew it without another login.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Provider is an OIDC provider's endpoints, from its discovery document.
type Provider struct {
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
}

// Discover fetches {issuer}/.well-known/openid-configuration.
func Discover(ctx context.Context, hc *http.Client, issuer string) (*Provider, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient(hc).Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach identity provider %s: %w", issuer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity provider discovery at %s returned %s", url, resp.Status)
	}
	var p Provider
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("parse discovery document: %w", err)
	}
	if strings.TrimRight(p.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("discovery document issuer %q does not match %q", p.Issuer, issuer)
	}
	if p.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("identity provider %s does not support the device authorization flow", issuer)
	}
	return &p, nil
}

// Config is what the CLI needs to log in, normally from AuthService.GetServerInfo.
type Config struct {
	Issuer   string
	ClientID string
	Scopes   []string
	// HTTPClient is used for every call to the identity provider.
	HTTPClient *http.Client
}

func (c Config) oauth2(p *Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: c.ClientID,
		Scopes:   c.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       p.AuthorizationEndpoint,
			TokenURL:      p.TokenEndpoint,
			DeviceAuthURL: p.DeviceAuthorizationEndpoint,
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// DeviceCode is what the user must see to approve the login.
type DeviceCode struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresAt               time.Time
}

// Credentials are stored per API URL.
type Credentials struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	Scopes       []string  `json:"scopes,omitempty"`
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
	Subject      string    `json:"subject,omitempty"`
	Email        string    `json:"email,omitempty"`
}

// Valid reports whether the ID token is still usable for at least the margin.
func (c *Credentials) Valid(now time.Time, margin time.Duration) bool {
	return c != nil && c.IDToken != "" && now.Add(margin).Before(c.Expiry)
}

// DeviceLogin runs the device flow. show is called once with the code to
// display; DeviceLogin then polls until the user approves, denies, the code
// expires or ctx is cancelled.
func DeviceLogin(ctx context.Context, c Config, show func(DeviceCode)) (*Credentials, error) {
	ctx = withClient(ctx, c.HTTPClient)
	p, err := Discover(ctx, c.HTTPClient, c.Issuer)
	if err != nil {
		return nil, err
	}
	oc := c.oauth2(p)
	da, err := oc.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("start device login: %w", describe(err))
	}
	show(DeviceCode{
		UserCode:                da.UserCode,
		VerificationURI:         da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete,
		ExpiresAt:               da.Expiry,
	})
	tok, err := oc.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("device login: %w", describe(err))
	}
	return fromToken(c, tok)
}

// fromToken extracts the ID token and its claims from a token response.
func fromToken(c Config, tok *oauth2.Token) (*Credentials, error) {
	id, _ := tok.Extra("id_token").(string)
	if id == "" {
		return nil, errors.New("identity provider returned no id_token; is the 'openid' scope allowed for this client?")
	}
	claims, err := parseClaims(id)
	if err != nil {
		return nil, err
	}
	return &Credentials{
		Issuer:       c.Issuer,
		ClientID:     c.ClientID,
		Scopes:       c.Scopes,
		IDToken:      id,
		RefreshToken: tok.RefreshToken,
		Expiry:       time.Unix(claims.Exp, 0),
		Subject:      claims.Sub,
		Email:        claims.Email,
	}, nil
}

type claims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
}

// parseClaims reads JWT claims WITHOUT verifying the signature. The CLI only
// uses them for display and expiry; the API does the verification.
func parseClaims(jwt string) (claims, error) {
	var c claims
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return c, errors.New("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, fmt.Errorf("decode id_token: %w", err)
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("decode id_token claims: %w", err)
	}
	if c.Exp == 0 {
		return c, errors.New("id_token has no exp claim")
	}
	return c, nil
}

// ErrLoginRequired means there are no usable credentials.
var ErrLoginRequired = errors.New("not logged in")

// TokenSource returns ID tokens for key from store, refreshing and saving
// them when they are about to expire. It is safe for concurrent use.
func TokenSource(ctx context.Context, store Store, key string, hc *http.Client, now func() time.Time) oauth2.TokenSource {
	if now == nil {
		now = time.Now
	}
	return &refreshingSource{ctx: ctx, store: store, key: key, hc: hc, now: now}
}

type refreshingSource struct {
	ctx   context.Context
	store Store
	key   string
	hc    *http.Client
	now   func() time.Time

	mu    sync.Mutex
	creds *Credentials
}

const expiryMargin = 30 * time.Second

func (s *refreshingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		c, err := s.store.Get(s.key)
		if errors.Is(err, ErrNotFound) {
			return nil, ErrLoginRequired
		}
		if err != nil {
			return nil, err
		}
		s.creds = c
	}
	if !s.creds.Valid(s.now(), expiryMargin) {
		if s.creds.RefreshToken == "" {
			return nil, fmt.Errorf("%w: session expired", ErrLoginRequired)
		}
		next, err := Refresh(s.ctx, s.hc, s.creds)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh failed: %w", ErrLoginRequired, err)
		}
		if err := s.store.Set(s.key, next); err != nil {
			return nil, fmt.Errorf("save refreshed credentials: %w", err)
		}
		s.creds = next
	}
	return &oauth2.Token{AccessToken: s.creds.IDToken, TokenType: "Bearer", Expiry: s.creds.Expiry}, nil
}

// Refresh exchanges the refresh token for new credentials.
func Refresh(ctx context.Context, hc *http.Client, c *Credentials) (*Credentials, error) {
	ctx = withClient(ctx, hc)
	cfg := Config{Issuer: c.Issuer, ClientID: c.ClientID, Scopes: c.Scopes, HTTPClient: hc}
	p, err := Discover(ctx, hc, c.Issuer)
	if err != nil {
		return nil, err
	}
	// An empty access token with a past expiry forces the refresh grant.
	tok, err := cfg.oauth2(p).TokenSource(ctx, &oauth2.Token{RefreshToken: c.RefreshToken, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		return nil, describe(err)
	}
	next, err := fromToken(cfg, tok)
	if err != nil {
		return nil, err
	}
	if next.RefreshToken == "" { // providers may not rotate refresh tokens
		next.RefreshToken = c.RefreshToken
	}
	return next, nil
}

// StaticToken is a TokenSource for FORGE_TOKEN.
func StaticToken(t string) oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: t, TokenType: "Bearer"})
}

func withClient(ctx context.Context, hc *http.Client) context.Context {
	if hc == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, hc)
}

func httpClient(hc *http.Client) *http.Client {
	if hc != nil {
		return hc
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// describe turns OAuth2 error responses into readable messages.
func describe(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "access_denied":
			return errors.New("login was denied")
		case "expired_token":
			return errors.New("the code expired before it was approved; run 'forge auth login' again")
		case "invalid_grant":
			return errors.New("the refresh token is no longer valid")
		}
		if re.ErrorDescription != "" {
			return fmt.Errorf("%s: %s", re.ErrorCode, re.ErrorDescription)
		}
		if re.ErrorCode != "" {
			return errors.New(re.ErrorCode)
		}
	}
	return err
}
