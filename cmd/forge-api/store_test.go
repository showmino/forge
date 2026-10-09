package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/internal/store/pgtest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestOpenStoreMemory(t *testing.T) {
	t.Parallel()
	s, err := openStore(context.Background(), "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*store.Memory); !ok {
		t.Fatalf("no URL: got %T, want *store.Memory", s)
	}
}

// catalogClient serves a catalog kept in s, as forge-api does, to a caller
// who may write.
func catalogClient(t *testing.T, s store.Store) forgev1connect.ServiceCatalogServiceClient {
	t.Helper()
	h, err := server.New(server.Options{
		Catalog:       server.NewCatalogWithStore(s, nil),
		Authenticator: server.StaticAuthenticator{"w": {Subject: "ana", Groups: []string{"platform"}}},
		Authorizer:    server.Authorizer{WriterGroups: []string{"platform"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
}

func authed[T any](msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer w")
	return r
}

func TestCatalogSurvivesRestart(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	ctx := context.Background()
	register := &forgev1.RegisterServiceRequest{
		ServiceId: "payments", RequestId: "req-1",
		Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	}

	first, err := openStore(ctx, url, quiet)
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalogClient(t, first).RegisterService(ctx, authed(register))
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := openStore(ctx, url, quiet) // a restart: connects and migrates again
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	client := catalogClient(t, second)
	got, err := client.GetService(ctx, authed(&forgev1.GetServiceRequest{Name: "services/payments"}))
	if err != nil || got.Msg.GetService().GetEtag() != created.Msg.GetService().GetEtag() {
		t.Fatalf("after restart: %v %v", got, err)
	}
	retry, err := client.RegisterService(ctx, authed(register))
	if err != nil || retry.Msg.GetService().GetName() != "services/payments" {
		t.Fatalf("retried request_id after restart: %v %v (want the stored service)", retry, err)
	}
}
