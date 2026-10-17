package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

func TestGitHubApp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyFile := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	notKey := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notKey, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	app, err := githubApp("5239131", keyFile, "showmino")
	if err != nil || app == nil || app.ID != 5239131 || app.Org != "showmino" || !app.Key.Equal(key) {
		t.Fatalf("app: %+v %v", app, err)
	}
	if app, err := githubApp("", "", "showmino"); app != nil || err != nil {
		t.Errorf("no App configured: %v %v", app, err)
	}
	if app, err := githubApp("", "", ""); app != nil || err != nil {
		t.Errorf("no App configured and no org either (pure local dev): %v %v", app, err)
	}
	for name, args := range map[string][3]string{
		"id only":     {"5239131", "", "showmino"},
		"key only":    {"", keyFile, "showmino"},
		"bad id":      {"forge", keyFile, "showmino"},
		"missing key": {"5239131", filepath.Join(dir, "nope.pem"), "showmino"},
		"not a key":   {"5239131", notKey, "showmino"},
		"no org":      {"5239131", keyFile, ""},
	} {
		_, err := githubApp(args[0], args[1], args[2])
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("%s: error leaks key material: %v", name, err)
		}
	}
}

func TestTemplateSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	app, err := githubApp("", "", "showmino") // nil App
	if err != nil {
		t.Fatal(err)
	}
	if src, err := templateSource(dir, "showmino/forge-templates", app); err != nil {
		t.Fatal(err)
	} else if _, ok := src.(server.DirSource); !ok {
		t.Errorf("directory: %T", src)
	}
	if src, err := templateSource("", "showmino/forge-templates", nil); err != nil || src != nil {
		t.Errorf("nothing configured: %v %v", src, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	withApp, err := templateSource("", "showmino/forge-templates", &github.App{ID: 1, Key: key, Org: "showmino"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := withApp.(*server.GitHubSource); !ok {
		t.Errorf("App: %T", withApp)
	}
	if _, err := templateSource(filepath.Join(dir, "nope"), "showmino/forge-templates", nil); err == nil {
		t.Error("missing directory accepted")
	}
	if _, err := templateSource("", "forge-templates", &github.App{ID: 1, Key: key, Org: "showmino"}); err == nil {
		t.Error("repo without owner accepted")
	}
}

func TestRepoHost(t *testing.T) {
	t.Parallel()
	if h := repoHost(nil); h != nil {
		t.Errorf("no App: want a nil RepoHost, got %#v", h)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := repoHost(&github.App{ID: 1, Key: key, Org: "showmino"}).(*github.Client)
	if !ok || c.Token == nil {
		t.Errorf("App: want a *github.Client with the App's token, got %#v", c)
	}
}
