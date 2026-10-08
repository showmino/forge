package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeAppAPI checks App JWTs the way GitHub does (signature, issuer, a
// lifetime of at most ten minutes around now) and hands out numbered tokens
// that expire an hour after they are minted.
func fakeAppAPI(t *testing.T, pub *rsa.PublicKey, now func() time.Time) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var exchanges atomic.Int32
	validJWT := func(r *http.Request) bool {
		jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if !ok || len(parts) != 3 {
			return false
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err != nil || rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
			return false
		}
		var c struct {
			Iat, Exp int64
			Iss      string
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if json.Unmarshal(payload, &c) != nil {
			return false
		}
		n := now().Unix()
		return c.Iss == "5239131" && c.Iat <= n && c.Exp > n && c.Exp-c.Iat <= 600
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/showmino/installation", func(w http.ResponseWriter, r *http.Request) {
		if !validJWT(r) {
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"id":169296924}`)
	})
	mux.HandleFunc("GET /orgs/elsewhere/installation", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("POST /app/installations/169296924/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !validJWT(r) {
			http.Error(w, `{"message":"bad JWT"}`, http.StatusUnauthorized)
			return
		}
		n := exchanges.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"inst-%d","expires_at":%q}`, n, now().Add(time.Hour).Format(time.RFC3339))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &exchanges
}

func TestAppToken(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	var clock atomic.Int64 // read by the fake server's goroutines
	clock.Store(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	srv, exchanges := fakeAppAPI(t, &key.PublicKey, now)
	a := &App{ID: 5239131, Key: key, Org: "showmino", BaseURL: srv.URL, Now: now}
	ctx := context.Background()

	for range 3 {
		tok, err := a.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "inst-1" {
			t.Errorf("token %q, want the cached inst-1", tok)
		}
	}
	if n := exchanges.Load(); n != 1 {
		t.Errorf("%d token exchanges, want 1", n)
	}

	clock.Add(int64((59*time.Minute + 30*time.Second) / time.Second)) // within a minute of expiry
	if tok, err := a.Token(ctx); err != nil || tok != "inst-2" {
		t.Errorf("near expiry: %q %v, want a fresh inst-2", tok, err)
	}
}

func TestAppErrors(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	srv, _ := fakeAppAPI(t, &key.PublicKey, time.Now)

	notInstalled := &App{ID: 5239131, Key: key, Org: "elsewhere", BaseURL: srv.URL}
	if _, err := notInstalled.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed on elsewhere") {
		t.Errorf("not installed: %v", err)
	}
	wrongKey := &App{ID: 5239131, Key: testKey(t), Org: "showmino", BaseURL: srv.URL}
	if _, err := wrongKey.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong key: %v", err)
	}
}

func TestParsePrivateKey(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, data := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParsePrivateKey(data)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	for name, data := range map[string][]byte{
		"not pem": []byte("hello"),
		"not rsa": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"garbage": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("x")}),
	} {
		if _, err := ParsePrivateKey(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
