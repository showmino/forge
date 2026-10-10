// Command forge-api is the Forge API server: the service catalog (in PostgreSQL with --database-url, else in memory),
// templates and new services, operations and preview environments. Callers
// authenticate with OIDC ID tokens from the configured issuer (Dex locally).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/showmino/forge/internal/github"
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

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		return -1 // invalid: --preview-max validation reports it
	}
	return def
}

func run() error {
	var (
		addr         = flag.String("addr", env("FORGE_API_ADDR", ":8080"), "listen address")
		issuer       = flag.String("oidc-issuer", env("FORGE_OIDC_ISSUER", ""), "OIDC issuer URL, for example http://localhost:5556/dex")
		clientID     = flag.String("oidc-client-id", env("FORGE_OIDC_CLIENT_ID", "forge-cli"), "OIDC client ID the CLI uses; tokens must carry it as audience")
		writers      = flag.String("writer-groups", env("FORGE_WRITER_GROUPS", ""), "comma-separated groups allowed to change the catalog (empty = every authenticated user)")
		dbURL        = flag.String("database-url", "", "PostgreSQL URL for the service catalog, or FORGE_DATABASE_URL (empty = in memory, lost on restart)")
		minClient    = flag.String("min-client-version", env("FORGE_MIN_CLIENT_VERSION", "0.1.0"), "oldest CLI version accepted")
		insecureDev  = flag.Bool("insecure-dev-token", false, "DEVELOPMENT ONLY: accept the token 'dev' as user dev@example.com instead of verifying OIDC tokens")
		logJSON      = flag.Bool("log-json", env("FORGE_LOG_FORMAT", "") == "json", "log as JSON")
		printVersion = flag.Bool("version", false, "print the version and exit")

		templatesDir  = flag.String("templates-dir", env("FORGE_TEMPLATES_DIR", ""), "serve templates from this local checkout as version 'dev' (development)")
		templatesRepo = flag.String("templates-repo", env("FORGE_TEMPLATES_REPO", "showmino/forge-templates"), "GitHub repo (owner/name) whose semver tags are template releases")
		appID         = flag.String("github-app-id", env("FORGE_GITHUB_APP_ID", ""), "GitHub App ID forge-api acts as")
		appKey        = flag.String("github-app-key-file", env("FORGE_GITHUB_APP_KEY_FILE", ""), "file with the GitHub App's private key (PEM)")
		githubOrg     = flag.String("github-org", env("FORGE_GITHUB_ORG", "showmino"), "GitHub org the App is installed on")

		previewsOn     = flag.Bool("previews", env("FORGE_PREVIEWS", "") == "1" || env("FORGE_PREVIEWS", "") == "true", "serve preview environments (needs the GitHub App and Kubernetes access)")
		previewDomain  = flag.String("preview-domain", env("FORGE_PREVIEW_DOMAIN", ""), "preview hosts are <environment>.<domain>, for example preview.10.150.1.222.nip.io")
		previewGateway = flag.String("preview-gateway", env("FORGE_PREVIEW_GATEWAY", "forge/forge-previews"), "Gateway (namespace/name) that routes previews")
		previewPull    = flag.String("preview-pull-secret", env("FORGE_PREVIEW_PULL_SECRET", "forge/mino-ghcr"), "dockerconfigjson Secret (namespace/name) copied into previews and used to check images")
		previewMax     = flag.Int("preview-max", envInt("FORGE_PREVIEW_MAX", 10), "most preview environments at once")

		deploysOn    = flag.Bool("deploys", env("FORGE_DEPLOYS", "") == "1" || env("FORGE_DEPLOYS", "") == "true", "serve deploys to staging and production through Argo CD (needs --previews)")
		deployRepo   = flag.String("deploy-repo", env("FORGE_DEPLOY_REPO", "showmino/forge-deploys"), "GitOps repo (owner/name) Forge commits deploys to")
		deployDomain = flag.String("deploy-domain", env("FORGE_DEPLOY_DOMAIN", ""), "deploy hosts are <service>.<env>.<domain> (default: the preview domain without 'preview.')")

		cliDir = flag.String("cli-dir", env("FORGE_CLI_DIR", ""), "directory of forge CLI builds served at /cli/forge-linux-<arch> for CI jobs")
	)
	flag.Parse()
	if *printVersion {
		fmt.Println(version.Get().Version)
		return nil
	}
	if *dbURL == "" {
		// Not the flag's default: --help prints defaults, and the URL holds a password.
		*dbURL = os.Getenv("FORGE_DATABASE_URL")
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
	if len(groups) == 0 && !*insecureDev {
		log.Warn("no --writer-groups: every authenticated user may change the catalog, create services and deploy")
	}
	app, err := githubApp(*appID, *appKey, *githubOrg)
	if err != nil {
		return err
	}
	templates, err := templateSource(*templatesDir, *templatesRepo, app)
	if err != nil {
		return err
	}
	if templates == nil {
		log.Info("templates are not configured; set --templates-dir, or --github-app-id and --github-app-key-file")
	}
	if app == nil {
		log.Info("creating services is off; set --github-app-id and --github-app-key-file")
	}
	st, err := openStore(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer st.Close()
	catalog, ops := server.NewCatalogWithStore(st, nil), server.NewOperations(nil)
	envs, cluster, images, err := previews(previewFlags{
		on: *previewsOn, domain: *previewDomain, gateway: *previewGateway, pullSecret: *previewPull, max: *previewMax,
		imageTimeout: env("FORGE_PREVIEW_IMAGE_TIMEOUT", "10m"), readyTimeout: env("FORGE_PREVIEW_READY_TIMEOUT", "5m"),
	}, app, catalog, ops, *githubOrg, log, kubeClients)
	if err != nil {
		return err
	}
	if envs == nil {
		log.Info("preview environments are off; set --previews")
	} else {
		go envs.RunReaper(ctx, time.Minute)
	}
	deps, err := deploys(deployFlags{
		on: *deploysOn, repo: *deployRepo, domain: *deployDomain, syncTimeout: env("FORGE_DEPLOY_SYNC_TIMEOUT", "10m"),
		namespaceTimeout: env("FORGE_DEPLOY_NAMESPACE_TIMEOUT", "5m"),
	}, app, *previewDomain, cluster, images, catalog, ops, *githubOrg, log)
	if err != nil {
		return err
	}
	if deps == nil {
		log.Info("deploys are off; set --deploys")
	} else {
		audience := env("FORGE_GITHUB_OIDC_AUDIENCE", "forge")
		authn = withCITokens(ctx, authn, audience, &github.Client{Token: app.Token}, catalog, *githubOrg)
		log.Info("accepting GitHub Actions OIDC tokens for staging deploys", "issuer", server.GitHubIssuer, "audience", audience)
	}
	h, err := server.New(server.Options{
		Catalog:          catalog,
		Operations:       ops,
		Environments:     envs,
		Deploys:          deps,
		CLIDir:           *cliDir,
		Templates:        templates,
		Repos:            repoHost(app),
		GitHubOrg:        *githubOrg,
		MinClientVersion: *minClient,
		OIDC:             server.OIDC{Issuer: *issuer, ClientID: *clientID},
		Authenticator:    authn,
		Authorizer:       server.Authorizer{WriterGroups: groups},
		Audit:            st,
		Logger:           log,
	})
	if err != nil {
		return err
	}
	srv := server.HTTPServer(*addr, h)
	// Open watch streams end only with their operation; cancel their request
	// contexts when shutdown starts so Shutdown does not wait for them. The
	// operations' own work is detached from request contexts and keeps going.
	reqCtx, endRequests := context.WithCancel(context.Background())
	defer endRequests()
	srv.BaseContext = func(net.Listener) context.Context { return reqCtx }
	srv.RegisterOnShutdown(endRequests)

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
