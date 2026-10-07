// Command forge-api is the Forge API server.
//
// Phase 1 serves the service catalog (in memory) and auth discovery. Callers
// authenticate with OIDC ID tokens from the configured issuer (Dex locally).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forge-api:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func run() error {
	var (
		addr         = flag.String("addr", env("FORGE_API_ADDR", ":8080"), "listen address")
		issuer       = flag.String("oidc-issuer", env("FORGE_OIDC_ISSUER", ""), "OIDC issuer URL, for example http://localhost:5556/dex")
		clientID     = flag.String("oidc-client-id", env("FORGE_OIDC_CLIENT_ID", "forge-cli"), "OIDC client ID the CLI uses; tokens must carry it as audience")
		writers      = flag.String("writer-groups", env("FORGE_WRITER_GROUPS", ""), "comma-separated groups allowed to change the catalog (empty = every authenticated user)")
		minClient    = flag.String("min-client-version", env("FORGE_MIN_CLIENT_VERSION", "0.1.0"), "oldest CLI version accepted")
		insecureDev  = flag.Bool("insecure-dev-token", false, "DEVELOPMENT ONLY: accept the token 'dev' as user dev@example.com instead of verifying OIDC tokens")
		logJSON      = flag.Bool("log-json", env("FORGE_LOG_FORMAT", "") == "json", "log as JSON")
		printVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *printVersion {
		fmt.Println(version.Get().Version)
		return nil
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var authn server.Authenticator
	switch {
	case *insecureDev:
		log.Warn("accepting the static token 'dev'; never use --insecure-dev-token outside local development")
		authn = server.StaticAuthenticator{"dev": {Subject: "dev", Email: "dev@example.com", Groups: []string{"dev"}}}
	case *issuer == "":
		return errors.New("--oidc-issuer (or FORGE_OIDC_ISSUER) is required")
	default:
		// The issuer may start after the API (docker compose, k3s); retry discovery briefly.
		var err error
		for attempt := 1; ; attempt++ {
			authn, err = server.NewOIDCAuthenticator(ctx, *issuer, *clientID)
			if err == nil || attempt == 10 || ctx.Err() != nil {
				break
			}
			log.Info("waiting for OIDC issuer", "issuer", *issuer, "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		if err != nil {
			return err
		}
	}

	var groups []string
	for _, g := range strings.Split(*writers, ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	h, err := server.New(server.Options{
		MinClientVersion: *minClient,
		OIDC:             server.OIDC{Issuer: *issuer, ClientID: *clientID},
		Authenticator:    authn,
		Authorizer:       server.Authorizer{WriterGroups: groups},
		Logger:           log,
	})
	if err != nil {
		return err
	}
	srv := server.HTTPServer(*addr, h)

	errc := make(chan error, 1)
	go func() {
		log.Info("forge-api listening", "addr", *addr, "version", version.Get().Version, "issuer", *issuer)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
