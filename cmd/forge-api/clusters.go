package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/showmino/forge/internal/server"
)

// clusterFlags are forge-api's cluster capacity settings.
type clusterFlags struct {
	on         bool
	name       string
	grafanaURL string
}

var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// clusters returns the cluster service, or nil when it is off. It reuses the
// previews' connection to Kubernetes when there is one.
func clusters(f clusterFlags, shared *server.Cluster, log *slog.Logger,
	connect func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Clusters, error) {
	if !f.on {
		return nil, nil
	}
	if !clusterNameRE.MatchString(f.name) {
		return nil, fmt.Errorf("--cluster-name %q must be a lowercase DNS label of at most 40 characters", f.name)
	}
	if f.grafanaURL != "" {
		u, err := url.Parse(f.grafanaURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("--grafana-url %q must be an http or https address, like https://grafana.example.com", f.grafanaURL)
		}
	}
	var (
		kube kubernetes.Interface
		dyn  dynamic.Interface
	)
	if shared != nil {
		kube, dyn = shared.Kube, shared.Dynamic
	} else {
		var err error
		if kube, dyn, err = connect(); err != nil {
			return nil, errors.Join(errors.New("clusters: connect to Kubernetes"), err)
		}
	}
	return server.NewClusters(server.ClusterOptions{Name: f.name, GrafanaURL: f.grafanaURL, Kube: kube, Dynamic: dyn, Log: log}), nil
}
