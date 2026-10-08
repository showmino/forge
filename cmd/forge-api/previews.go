package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	max                         int
	imageTimeout, readyTimeout  string
}

// previews returns the environment service, or nil when previews are off.
// cluster connects to Kubernetes (kubeClients outside tests).
func previews(f previewFlags, app *github.App, catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger,
	cluster func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Environments, error) {
	if !f.on {
		return nil, nil
	}
	switch {
	case app == nil:
		return nil, errors.New("--previews needs the GitHub App (--github-app-id and --github-app-key-file)")
	case f.domain == "":
		return nil, errors.New("--previews needs --preview-domain (FORGE_PREVIEW_DOMAIN)")
	case !strings.Contains(f.gateway, "/"):
		return nil, fmt.Errorf("--preview-gateway %q must be namespace/name", f.gateway)
	case !strings.Contains(f.pullSecret, "/"):
		return nil, fmt.Errorf("--preview-pull-secret %q must be namespace/name", f.pullSecret)
	case f.max < 1:
		return nil, fmt.Errorf("--preview-max %d must be at least 1", f.max)
	}
	imageTimeout, err := positiveDuration("FORGE_PREVIEW_IMAGE_TIMEOUT", f.imageTimeout)
	if err != nil {
		return nil, err
	}
	readyTimeout, err := positiveDuration("FORGE_PREVIEW_READY_TIMEOUT", f.readyTimeout)
	if err != nil {
		return nil, err
	}
	kube, dyn, err := cluster()
	if err != nil {
		return nil, fmt.Errorf("previews: connect to Kubernetes: %w", err)
	}
	c := &server.Cluster{Kube: kube, Dynamic: dyn, Gateway: f.gateway, PullSecret: f.pullSecret}
	images := &registry.Client{Creds: func(ctx context.Context, host string) (string, string, error) {
		data, err := c.PullSecretData(ctx)
		if err != nil {
			return "", "", err
		}
		return registry.CredsFromDockerConfig(data, host)
	}}
	return server.NewEnvironments(server.EnvOptions{
		Cluster: c, GitHub: &github.Client{Token: app.Token}, Images: images,
		Catalog: catalog, Operations: ops, Org: org, Domain: f.domain, Max: f.max,
		ImageTimeout: imageTimeout, ReadyTimeout: readyTimeout, Log: log,
	}), nil
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
