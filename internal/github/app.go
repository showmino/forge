package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// ParsePrivateKey reads a GitHub App private key: PEM-encoded PKCS#1 (what
// GitHub issues) or PKCS#8.
func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the App private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the App private key is neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the App private key is not an RSA key")
	}
	return rk, nil
}

// App authenticates as a GitHub App's installation on one org. Token fits
// Client.Token.
type App struct {
	ID  int64
	Key *rsa.PrivateKey
	// Org is the account the App is installed on, for example "showmino".
	Org string
	// BaseURL and HTTP as for Client.
	BaseURL string
	HTTP    *http.Client
	// Now defaults to time.Now; tests replace it.
	Now func() time.Time

	mu           sync.Mutex
	installation int64
	token        string
	expires      time.Time
}

// tokenMargin renews an installation token this long before it expires.
const tokenMargin = time.Minute

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// JWT returns a token that authenticates as the App itself: valid for nine
// minutes (GitHub allows ten) and back-dated a minute for clock drift.
func (a *App) JWT() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	if err != nil {
		return "", err
	}
	signed := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign the App JWT: %w", err)
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// Token returns an installation token for Org, reusing it until a minute
// before it expires.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.now().Add(tokenMargin).Before(a.expires) {
		return a.token, nil
	}
	if a.installation == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := a.call(ctx, http.MethodGet, "/orgs/"+url.PathEscape(a.Org)+"/installation", &inst); err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", fmt.Errorf("the GitHub App %d is not installed on %s: %w", a.ID, a.Org, err)
			}
			return "", err
		}
		a.installation = inst.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", a.installation), &tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		return "", errors.New("GitHub returned an empty installation token")
	}
	a.token, a.expires = tok.Token, tok.ExpiresAt
	return a.token, nil
}

// call sends an App-authenticated (JWT) request and decodes its JSON reply into out.
func (a *App) call(ctx context.Context, method, path string, out any) error {
	jwt, err := a.JWT()
	if err != nil {
		return err
	}
	c := &Client{BaseURL: a.BaseURL, HTTP: a.HTTP, Token: StaticToken(jwt)}
	resp, err := c.do(ctx, method, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}
