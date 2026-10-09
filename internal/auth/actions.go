package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ActionsAudience is the audience forge-api expects on GitHub Actions tokens.
const ActionsAudience = "forge"

// ErrActionsTokenRefused is returned when GitHub answers the token request
// with 401 or 403: the job's request token was refused.
var ErrActionsTokenRefused = errors.New("GitHub refused the Actions OIDC token request")

// ActionsToken returns GitHub Actions OIDC tokens for audience, requested
// from requestURL (ACTIONS_ID_TOKEN_REQUEST_URL) with requestToken
// (ACTIONS_ID_TOKEN_REQUEST_TOKEN). A token is reused until a minute before
// its exp, so a long deploy gets a fresh one for its later calls.
func ActionsToken(hc *http.Client, requestURL, requestToken, audience string) oauth2.TokenSource {
	if hc == nil {
		hc = http.DefaultClient
	}
	return oauth2.ReuseTokenSourceWithExpiry(nil, &actionsSource{hc: hc, url: requestURL, token: requestToken, audience: audience}, time.Minute)
}

type actionsSource struct {
	hc                   *http.Client
	url, token, audience string
}

func (s *actionsSource) Token() (*oauth2.Token, error) {
	u, err := url.Parse(s.url)
	if err != nil || u.Scheme == "" {
		return nil, errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is not a URL")
	}
	q := u.Query()
	q.Set("audience", s.audience)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s", ErrActionsTokenRefused, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request a GitHub Actions OIDC token: %s", resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Value == "" {
		return nil, errors.New("request a GitHub Actions OIDC token: the response has no token")
	}
	return &oauth2.Token{AccessToken: out.Value, TokenType: "Bearer", Expiry: jwtExpiry(out.Value)}, nil
}

// jwtExpiry reads a JWT's exp without verifying it. When it can't, it
// returns now, so the token is used once and never cached.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Now()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Now()
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Exp == 0 {
		return time.Now()
	}
	return time.Unix(c.Exp, 0)
}
