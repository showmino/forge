package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/registry"
	"github.com/showmino/forge/internal/server"
)

// previewFlags are forge-api's preview environment settings.
type previewFlags struct {
	on                          bool
	domain, gateway, pullSecret string
	// registry is --image-registry, where the services' CI pushes images.
	registry                   string
	max                        int
	imageTimeout, readyTimeout string
}

// previews returns the environment service, or nil when previews are off. It
// also returns the cluster connection and image checker, which deploys share.
// cluster connects to Kubernetes (kubeClients outside tests).
func previews(f previewFlags, app *github.App, catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger,
	cluster func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Environments, *server.Cluster, server.ImageChecker, error) {
	if !f.on {
		return nil, nil, nil, nil
	}
	switch {
	case app == nil:
		return nil, nil, nil, errors.New("--previews needs the GitHub App (--github-app-id and --github-app-key-file)")
	case f.domain == "":
		return nil, nil, nil, errors.New("--previews needs --preview-domain (FORGE_PREVIEW_DOMAIN)")
	case !strings.Contains(f.gateway, "/"):
		return nil, nil, nil, fmt.Errorf("--preview-gateway %q must be namespace/name", f.gateway)
	case !strings.Contains(f.pullSecret, "/"):
		return nil, nil, nil, fmt.Errorf("--preview-pull-secret %q must be namespace/name", f.pullSecret)
	case f.max < 1:
		return nil, nil, nil, fmt.Errorf("--preview-max %d must be at least 1", f.max)
	case !validRegistry(f.registry):
		return nil, nil, nil, fmt.Errorf("--image-registry %q must be a lower-case registry host with an optional path, such as ghcr.io or myregistry.azurecr.io/team", f.registry)
	}
	imageTimeout, err := positiveDuration("FORGE_PREVIEW_IMAGE_TIMEOUT", f.imageTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	readyTimeout, err := positiveDuration("FORGE_PREVIEW_READY_TIMEOUT", f.readyTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	kube, dyn, err := cluster()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("previews: connect to Kubernetes: %w", err)
	}
	c := &server.Cluster{Kube: kube, Dynamic: dyn, Gateway: f.gateway, PullSecret: f.pullSecret}
	images := &registry.Client{Creds: func(ctx context.Context, host string) (string, string, error) {
		data, err := c.PullSecretData(ctx)
		if err != nil {
			return "", "", err
		}
		return registry.CredsFromDockerConfig(data, host)
	}}
	envs := server.NewEnvironments(server.EnvOptions{
		Cluster: c, GitHub: &github.Client{Token: app.Token}, Images: images,
		Catalog: catalog, Operations: ops, Org: org, Registry: f.registry, Domain: f.domain, Max: f.max,
		ImageTimeout: imageTimeout, ReadyTimeout: readyTimeout, Log: log,
	})
	return envs, c, images, nil
}

// registryRE is a registry host (with a dot, or a port) and an optional
// path: "ghcr.io", "acr.example.io", "registry.example:5000/team".
var registryRE = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*(:[0-9]+)?(/[a-z0-9._-]+)*$`)

// validRegistry reports whether r may go in front of "/<owner>/<service>".
// Empty is ghcr.io.
func validRegistry(r string) bool {
	if r == "" {
		return true
	}
	host, _, _ := strings.Cut(r, "/")
	// A dot in the host, as image references need (registry.ParseRef).
	return registryRE.MatchString(r) && strings.Contains(host, ".")
}

func positiveDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive duration such as 10m", name, v)
	}
	return d, nil
}

// kubeClients connects with the pod's service account, or with KUBECONFIG
// (default ~/.kube/config) outside a cluster.
func kubeClients() (kubernetes.Interface, dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if errors.Is(err, rest.ErrNotInCluster) {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
	}
	if err != nil {
		return nil, nil, err
	}
	// A hung API server must not stall creates and destroys for long.
	cfg.Timeout = 30 * time.Second
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return kube, dyn, nil
}
