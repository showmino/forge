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
		admins       = flag.String("admin-groups", env("FORGE_ADMIN_GROUPS", ""), "comma-separated groups that may do everything on every service (empty = the writer groups); a service's owner team may work on that service")
		writers      = flag.String("writer-groups", env("FORGE_WRITER_GROUPS", ""), "older name of --admin-groups, used when that is empty")
		dbURL        = flag.String("database-url", "", "PostgreSQL URL for the service catalog, or FORGE_DATABASE_URL (empty = in memory, lost on restart)")
		minClient    = flag.String("min-client-version", env("FORGE_MIN_CLIENT_VERSION", "0.1.0"), "oldest CLI version accepted")
		webClientID  = flag.String("oidc-web-client-id", env("FORGE_OIDC_WEB_CLIENT_ID", ""), "OIDC client ID of the web dashboard; tokens issued to it are accepted too (empty: only the CLI's)")
		corsOrigins  = flag.String("cors-origins", env("FORGE_CORS_ORIGINS", ""), "comma-separated origins of the web dashboard that may call the API from a browser, for example https://forge.example.com (empty: none)")
		insecureDev  = flag.Bool("insecure-dev-token", false, "DEVELOPMENT ONLY: accept the token 'dev' as user dev@example.com instead of verifying OIDC tokens")
		logJSON      = flag.Bool("log-json", env("FORGE_LOG_FORMAT", "") == "json", "log as JSON")
		printVersion = flag.Bool("version", false, "print the version and exit")

		templatesDir  = flag.String("templates-dir", env("FORGE_TEMPLATES_DIR", ""), "serve templates from this local checkout as version 'dev' (development)")
		templatesRepo = flag.String("templates-repo", env("FORGE_TEMPLATES_REPO", ""), "GitHub repo (owner/name) whose semver tags are template releases; required with the GitHub App unless --templates-dir is set")
		appID         = flag.String("github-app-id", env("FORGE_GITHUB_APP_ID", ""), "GitHub App ID forge-api acts as")
		appKey        = flag.String("github-app-key-file", env("FORGE_GITHUB_APP_KEY_FILE", ""), "file with the GitHub App's private key (PEM)")
		githubOrg     = flag.String("github-org", env("FORGE_GITHUB_ORG", ""), "GitHub org the App is installed on; required with the GitHub App")

		previewsOn     = flag.Bool("previews", env("FORGE_PREVIEWS", "") == "1" || env("FORGE_PREVIEWS", "") == "true", "serve preview environments (needs the GitHub App and Kubernetes access)")
		previewDomain  = flag.String("preview-domain", env("FORGE_PREVIEW_DOMAIN", ""), "preview hosts are <environment>.<domain>, for example preview.10.150.1.222.nip.io")
		previewGateway = flag.String("preview-gateway", env("FORGE_PREVIEW_GATEWAY", "forge/forge-previews"), "Gateway (namespace/name) that routes previews")
		imageRegistry  = flag.String("image-registry", env("FORGE_IMAGE_REGISTRY", server.DefaultRegistry), "where the services' CI pushes images (a host and an optional path); deploys and previews run <registry>/<owner>/<service>:<commit>")
		previewPull    = flag.String("preview-pull-secret", env("FORGE_PREVIEW_PULL_SECRET", ""), "dockerconfigjson Secret (namespace/name) copied into previews and used to check images; required with --previews")
		previewMax     = flag.Int("preview-max", envInt("FORGE_PREVIEW_MAX", 10), "most preview environments at once")

		deploysOn     = flag.Bool("deploys", env("FORGE_DEPLOYS", "") == "1" || env("FORGE_DEPLOYS", "") == "true", "serve deploys to staging and production through Argo CD (needs --previews)")
		deployRepo    = flag.String("deploy-repo", env("FORGE_DEPLOY_REPO", ""), "GitOps repo (owner/name) Forge commits deploys to; required with --deploys")
		deployDomain  = flag.String("deploy-domain", env("FORGE_DEPLOY_DOMAIN", ""), "deploy hosts are <service>.<env>.<domain> (default: the preview domain without 'preview.')")
		publicDomains = flag.String("public-domains", env("FORGE_PUBLIC_DOMAINS", ""), "domains services get public addresses under, each with its DNS zone's name, the default first: example.com=example-com,other.io=other-io (empty: internal addresses only)")
		publicTunnel  = flag.String("public-dns-tunnel", env("FORGE_PUBLIC_DNS_TUNNEL", "production"), "which of the platform's tunnels public DNS records point at")
		argoURL       = flag.String("argocd-url", env("FORGE_ARGOCD_URL", ""), "Argo CD's web address, e.g. https://argocd.example.com: deployments link to their Application there (empty: no link)")
		secretVaults  = flag.String("secret-vaults", env("FORGE_SECRET_VAULTS", ""), "secret groups a service's run.secrets may read, comma separated (empty: none); never a group with the platform's own secrets")
		secretDenied  = flag.String("secret-denied-items", env("FORGE_SECRET_DENIED_ITEMS", ""), "items in those groups no service may read, comma separated: the platform's own credentials")
		secretBackend = flag.String("secret-backend", env("FORGE_SECRET_BACKEND", "onepassword"), "which operator populates a run.secrets entry's Secret (must match the chart's secretBackend value); \"onepassword\" accepts only vaults/<vault>/items/<item> items")

		resourcesOn  = flag.Bool("resources", env("FORGE_RESOURCES", "") == "1" || env("FORGE_RESOURCES", "") == "true", "serve resources (forge resource): databases made by OpenTofu runs in Kubernetes Jobs (needs --deploys and --runner-image)")
		runnerImage  = flag.String("runner-image", env("FORGE_RUNNER_IMAGE", ""), "image of the resource runner Job")
		runnerAddr   = flag.String("runner-addr", env("FORGE_RUNNER_ADDR", ":8081"), "listen address of the runner API; reachable from the runner Jobs only, never public")
		runnerNS     = flag.String("runner-namespace", env("FORGE_RUNNER_NAMESPACE", "forge-runner"), "namespace the runner Jobs run in")
		runnerSA     = flag.String("runner-service-account", env("FORGE_RUNNER_SERVICE_ACCOUNT", "forge-runner"), "service account the runner Jobs run as")
		runnerAPIURL = flag.String("runner-api-url", env("FORGE_RUNNER_API_URL", "http://forge-api-runner.forge.svc:8081"), "URL at which the runner Jobs reach --runner-addr")

		clustersOn  = flag.Bool("clusters", env("FORGE_CLUSTERS", "") == "1" || env("FORGE_CLUSTERS", "") == "true", "serve the cluster's capacity to admins (forge cluster): nodes from Kubernetes, use from metrics-server")
		clusterName = flag.String("cluster-name", env("FORGE_CLUSTER_NAME", "default"), "the cluster's name in clusters/<name>")
		grafanaURL  = flag.String("grafana-url", env("FORGE_GRAFANA_URL", ""), "Grafana's address, linked from the cluster (empty: no link)")
		promURL     = flag.String("prometheus-url", env("FORGE_PROMETHEUS_URL", ""), "Prometheus's address, for node disk and volume use in forge cluster (empty: none)")
		remoteCls   = flag.String("remote-clusters", env("FORGE_REMOTE_CLUSTERS", ""), "more clusters to show in forge cluster: names of Argo CD cluster Secrets in namespace argocd, comma-separated; each is read with its Secret's credential")

		cliDir  = flag.String("cli-dir", env("FORGE_CLI_DIR", ""), "directory of forge CLI builds served at /cli/forge-linux-<arch> for CI jobs")
		cliRepo = flag.String("cli-repo", env("FORGE_CLI_REPO", ""), "GitHub repo (owner/name) whose releases 'forge upgrade' downloads through /cli/releases (empty: that proxy is off)")
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
	origins, err := server.ParseOrigins(*corsOrigins)
	if err != nil {
		return fmt.Errorf("--cors-origins: %w", err)
	}
	if *clientID == "" && !*insecureDev {
		return errors.New("--oidc-client-id (or FORGE_OIDC_CLIENT_ID) must not be empty")
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
			authn, err = server.NewOIDCAuthenticator(ctx, *issuer, *clientID, *webClientID)
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
	if oidc, ok := authn.(*server.OIDCAuthenticator); ok {
		oidc.SetWebClient(*webClientID)
	}

	groups := splitList(*admins)
	if len(groups) == 0 {
		groups = splitList(*writers)
	}
	if len(groups) == 0 && !*insecureDev {
		log.Warn("no --admin-groups: every authenticated user is an admin, who may change every service")
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
	// Also with resources off: records made while they were on keep their service.
	catalog.GuardResources(st)
	if app != nil {
		// Short: these calls run while the catalog's write lock is held.
		catalog.ManageRepoAccess(&github.Client{Token: app.Token, HTTP: &http.Client{Timeout: 15 * time.Second}}, *githubOrg)
	}
	envs, cluster, images, err := previews(previewFlags{
		on: *previewsOn, domain: *previewDomain, gateway: *previewGateway, pullSecret: *previewPull, registry: *imageRegistry, max: *previewMax,
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
		publicDomains: *publicDomains, publicTunnel: *publicTunnel, argoURL: *argoURL, secretVaults: *secretVaults, secretDenied: *secretDenied, secretBackend: *secretBackend,
		namespaceTimeout: env("FORGE_DEPLOY_NAMESPACE_TIMEOUT", "5m"), resources: st, registry: *imageRegistry,
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
	res, runnerAPI, err := resources(ctx, resourceFlags{
		on: *resourcesOn, runnerImage: *runnerImage, runnerNamespace: *runnerNS, runnerServiceAccount: *runnerSA, runnerAPIURL: *runnerAPIURL,
	}, st, cluster, deps != nil, catalog, ops, log)
	if err != nil {
		return err
	}
	if res == nil {
		log.Info("resources are off; set --resources")
	} else {
		// Ends runs nobody watches any more (a restart) and expires old plans.
		go res.RunSweeper(ctx, time.Minute)
	}
	// Deleting a service takes down its previews and deployments first.
	catalog.EnableTeardown(deps, envs, ops)
	cl, err := clusters(clusterFlags{on: *clustersOn, name: *clusterName, grafanaURL: *grafanaURL, prometheusURL: *promURL, remotes: *remoteCls}, cluster, log, kubeClients)
	if err != nil {
		return err
	}
	if cl == nil {
		log.Info("cluster capacity is off; set --clusters")
	}
	// A login becomes a session of Forge's own, which lasts without the
	// identity provider; forge-api keeps its person's teams current itself.
	// Only with the GitHub App: without it nobody could tell that a
	// session's person has left, and the CLI keeps the tokens of its login.
	var sessions *server.Sessions
	if app != nil {
		for _, g := range groups {
			if org, slug, ok := strings.Cut(g, ":"); !ok || slug == "" || !strings.EqualFold(org, *githubOrg) {
				log.Warn("an admin group is not a team of the GitHub organization: a session keeps it as its login gave it", "group", g, "org", *githubOrg)
			}
		}
		sessions = server.NewSessions(server.SessionOptions{Store: st, Members: members(app), Org: *githubOrg, Teams: groups, OwningTeams: catalog.OwnerTeams, Log: log})
		go sessions.RunSweeper(ctx, time.Hour)
	} else {
		log.Info("sessions are off: they need the GitHub App to keep memberships current")
	}
	h, err := server.New(server.Options{
		Catalog:          catalog,
		Operations:       ops,
		Environments:     envs,
		Deploys:          deps,
		Resources:        res,
		Clusters:         cl,
		CLIDir:           *cliDir,
		CLIReleases:      cliReleases(app),
		CLIRepo:          *cliRepo,
		Templates:        templates,
		Teams:            teamSource(app),
		Docs:             docsSource(app),
		Sessions:         sessions,
		Repos:            repoHost(app),
		GitHubOrg:        *githubOrg,
		MinClientVersion: *minClient,
		OIDC:             server.OIDC{Issuer: *issuer, ClientID: *clientID, WebClientID: *webClientID},
		Authenticator:    authn,
		Authorizer:       server.Authorizer{AdminGroups: groups, Org: *githubOrg},
		Audit:            st,
		Logger:           log,
	})
	if err != nil {
		return err
	}
	if len(origins) > 0 {
		log.Info("the web dashboard may call the API from a browser", "origins", origins, "client", *webClientID)
	}
	// A dashboard on the API's own host needs no CORS origins.
	if len(origins) > 0 && *webClientID == "" {
		log.Warn("--cors-origins is set without --oidc-web-client-id, so the web dashboard cannot log in")
	}
	srv := server.HTTPServer(*addr, server.CORS(origins, h))
	// Open watch streams end only with their operation; cancel their request
	// contexts when shutdown starts so Shutdown does not wait for them. The
	// operations' own work is detached from request contexts and keeps going.
	reqCtx, endRequests := context.WithCancel(context.Background())
	defer endRequests()
	srv.BaseContext = func(net.Listener) context.Context { return reqCtx }
	srv.RegisterOnShutdown(endRequests)
	servers := []*http.Server{srv}

	errc := make(chan error, 2)
	go func() {
		log.Info("forge-api listening", "addr", *addr, "version", version.Get().Version, "issuer", *issuer)
		errc <- srv.ListenAndServe()
	}()
	if runnerAPI != nil {
		// The runner API has its own listener: the public one never serves it.
		runnerSrv := server.HTTPServer(*runnerAddr, runnerAPI)
		servers = append(servers, runnerSrv)
		go func() {
			log.Info("runner API listening", "addr", *runnerAddr)
			errc <- runnerSrv.ListenAndServe()
		}()
	}
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var errs []error
		for _, s := range servers {
			errs = append(errs, s.Shutdown(shutdownCtx))
		}
		return errors.Join(errs...)
	}
	return nil
}

// splitList splits a comma-separated flag, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
