package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	s, err := openStore(context.Background(), "", true, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*store.Memory); !ok {
		t.Fatalf("--in-memory: got %T, want *store.Memory", s)
	}
}

// Without a database URL forge-api does not start: it never falls back to a
// catalog that is lost on restart unless --in-memory says so.
func TestOpenStoreNeedsADatabase(t *testing.T) {
	t.Parallel()
	_, err := openStore(context.Background(), "", false, quiet)
	if err == nil {
		t.Fatal("no URL and no --in-memory: want an error")
	}
	for _, want := range []string{"--database-url", "FORGE_DATABASE_URL", "--in-memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// A database that does not answer is retried, then reported; neither the
// error nor the log lines show the password.
func TestOpenStoreUnreachableHidesPassword(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Port 1 on loopback: nothing listens, the connection is refused at once.
	_, err := openStore(ctx, "postgres://forge:s3cret-pw@127.0.0.1:1/forge?sslmode=disable&connect_timeout=1", false, log)
	if err == nil {
		t.Fatal("want an error for a database that does not answer")
	}
	if !strings.Contains(logs.String(), "waiting for the database") {
		t.Errorf("no retry was logged: %s", logs.String())
	}
	if strings.Contains(err.Error()+logs.String(), "s3cret-pw") {
		t.Fatalf("the password was shown:\nerror: %v\nlog: %s", err, logs.String())
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

	first, err := openStore(ctx, url, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	created, err := catalogClient(t, first).RegisterService(ctx, authed(register))
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := openStore(ctx, url, false, quiet) // a restart: connects and migrates again
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

func TestOpenStoreBadURLDoesNotRetry(t *testing.T) {
	t.Parallel()
	start := time.Now()
	_, err := openStore(context.Background(), "postgres://u:p@h:notaport/db", false, quiet)
	if err == nil {
		t.Fatal("want an error for an invalid URL")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("invalid URL took %v: it was retried", d)
	}
	if strings.Contains(err.Error(), ":p@") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "database URL") {
		t.Errorf("error %q does not say the URL is the problem", err)
	}
}

func TestOpenStoreStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := openStore(ctx, "postgres://u:p@127.0.0.1:1/db?connect_timeout=1", false, quiet)
	if err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancelled openStore took %v", d)
	}
}
