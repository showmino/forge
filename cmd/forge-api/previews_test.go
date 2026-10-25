package main

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

func fakeKube() (kubernetes.Interface, dynamic.Interface, error) {
	return envtest.Kube(), envtest.Dynamic(), nil
}

func TestPreviews(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &github.App{ID: 1, Key: key, Org: "showmino"}
	ok := previewFlags{on: true, domain: "preview.10.150.1.222.nip.io", gateway: "forge/forge-previews", pullSecret: "forge/mino-ghcr", max: 10, imageTimeout: "10m", readyTimeout: "5m"}
	log := slog.New(slog.DiscardHandler)
	cat, ops := server.NewCatalog(nil), server.NewOperations(nil)

	if envs, _, _, err := previews(previewFlags{}, nil, cat, ops, "showmino", log, fakeKube); envs != nil || err != nil {
		t.Errorf("off: %v %v", envs, err)
	}
	if envs, _, _, err := previews(ok, app, cat, ops, "showmino", log, fakeKube); envs == nil || err != nil {
		t.Errorf("on: %v %v", envs, err)
	}
	for _, r := range []string{"ghcr.io", "acrexample.azurecr.io", "registry.example:5000/team"} {
		f := ok
		f.registry = r
		if envs, _, _, err := previews(f, app, cat, ops, "showmino", log, fakeKube); envs == nil || err != nil {
			t.Errorf("registry %q: %v %v", r, envs, err)
		}
	}
	for name, tc := range map[string]struct {
		f   previewFlags
		app *github.App
		msg string
	}{
		"no app":        {ok, nil, "needs the GitHub App"},
		"no domain":     {func() previewFlags { f := ok; f.domain = ""; return f }(), app, "--preview-domain"},
		"bad gateway":   {func() previewFlags { f := ok; f.gateway = "x"; return f }(), app, "namespace/name"},
		"bad pull":      {func() previewFlags { f := ok; f.pullSecret = "x"; return f }(), app, "namespace/name"},
		"no pull":       {func() previewFlags { f := ok; f.pullSecret = ""; return f }(), app, "namespace/name"},
		"bad max":       {func() previewFlags { f := ok; f.max = 0; return f }(), app, "--preview-max"},
		"bad image t/o": {func() previewFlags { f := ok; f.imageTimeout = "soon"; return f }(), app, "FORGE_PREVIEW_IMAGE_TIMEOUT"},
		"bad ready t/o": {func() previewFlags { f := ok; f.readyTimeout = "-1m"; return f }(), app, "FORGE_PREVIEW_READY_TIMEOUT"},
		"scheme":        {func() previewFlags { f := ok; f.registry = "https://acr.example.io"; return f }(), app, "--image-registry"},
		"trailing /":    {func() previewFlags { f := ok; f.registry = "acr.example.io/"; return f }(), app, "--image-registry"},
		"upper case":    {func() previewFlags { f := ok; f.registry = "ACR.example.io"; return f }(), app, "--image-registry"},
		"no host":       {func() previewFlags { f := ok; f.registry = "team"; return f }(), app, "--image-registry"},
	} {
		if _, _, _, err := previews(tc.f, tc.app, cat, ops, "showmino", log, fakeKube); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.msg)
		}
	}
}
