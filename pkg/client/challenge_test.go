package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func TestDetectChallenges(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/challenged", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Cf-Ray", "a4b71875bb978de0-ARN")
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: DetectChallenges(nil)}

	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		return hc.Do(req)
	}
	resp, err := get("/challenged")
	if err == nil {
		_ = resp.Body.Close()
	}
	var ce *ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("challenged: %v %v", resp, err)
	}
	if ce.RayID != "a4b71875bb978de0-ARN" || !strings.Contains(ce.Error(), "Cloudflare") || !strings.Contains(ce.Error(), "a4b71875bb978de0-ARN") || !strings.Contains(ce.Error(), strings.TrimPrefix(srv.URL, "http://")) {
		t.Errorf("message: %q", ce.Error())
	}

	// A refusal by the server itself is passed on as it is.
	resp, err = get("/forbidden")
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forbidden: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestNewDetectsChallenges(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Auth.GetStatus(context.Background(), connect.NewRequest(&forgev1.GetStatusRequest{}))
	var ce *ChallengeError
	if !errors.As(err, &ce) {
		t.Errorf("an API call through a challenge: %v", err)
	}
}
