package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/showmino/forge/internal/server"
)

// clusterFlags are forge-api's cluster capacity settings.
type clusterFlags struct {
	on            bool
	name          string
	grafanaURL    string
	prometheusURL string
	// remotes is --remote-clusters: names of Argo CD cluster Secrets in
	// argoNamespace, comma-separated. Each is a cluster of that name.
	remotes string
}

// argoNamespace holds Argo CD's cluster Secrets.
const argoNamespace = "argocd"

var clusterNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// clusters returns the cluster service, or nil when it is off. It reuses the
// previews' connection to Kubernetes when there is one.
func clusters(f clusterFlags, shared *server.Cluster, log *slog.Logger,
	connect func() (kubernetes.Interface, dynamic.Interface, error)) (*server.Clusters, error) {
	if !f.on {
		if f.remotes != "" {
			return nil, errors.New("--remote-clusters needs --clusters")
		}
		return nil, nil
	}
	if !clusterNameRE.MatchString(f.name) {
		return nil, fmt.Errorf("--cluster-name %q must be a lowercase DNS label of at most 40 characters", f.name)
	}
	var remotes []string
	if f.remotes != "" {
		seen := map[string]bool{f.name: true}
		for r := range strings.SplitSeq(f.remotes, ",") {
			if !clusterNameRE.MatchString(r) {
				return nil, fmt.Errorf("--remote-clusters: %q must be the name of an Argo CD cluster Secret, a lowercase DNS label of at most 40 characters", r)
			}
			if seen[r] {
				return nil, fmt.Errorf("--remote-clusters: %q is named twice, or is --cluster-name", r)
			}
			seen[r] = true
			remotes = append(remotes, r)
		}
	}
	if f.grafanaURL != "" {
		u, err := url.Parse(f.grafanaURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("--grafana-url %q must be an http or https address, like https://grafana.example.com", f.grafanaURL)
		}
	}
	var prom *server.Prometheus
	if f.prometheusURL != "" {
		u, err := url.Parse(f.prometheusURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("--prometheus-url %q must be an http or https address, like http://prometheus.monitoring.svc:9090", f.prometheusURL)
		}
		prom = &server.Prometheus{URL: f.prometheusURL}
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
	o := server.ClusterOptions{Name: f.name, GrafanaURL: f.grafanaURL, Prometheus: prom, Kube: kube, Dynamic: dyn, Log: log}
	for _, r := range remotes {
		a := &argoCluster{local: kube, namespace: argoNamespace, secret: r, log: log}
		o.Remotes = append(o.Remotes, server.RemoteCluster{Name: r, Connect: a.connect})
	}
	return server.NewClusters(o), nil
}

// argoLabel marks a Secret as an Argo CD cluster.
const argoLabel = "argocd.argoproj.io/secret-type"

// argoCluster connects to a cluster with the credential Argo CD holds for
// it. It reads the Secret on every connect and builds new clients only when
// the Secret changed, so a rotated credential is picked up.
type argoCluster struct {
	local     kubernetes.Interface
	namespace string
	secret    string
	log       *slog.Logger

	mu      sync.Mutex
	version string
	kube    kubernetes.Interface
	dyn     dynamic.Interface
}

func (a *argoCluster) connect(ctx context.Context) (kubernetes.Interface, dynamic.Interface, error) {
	where := a.namespace + "/" + a.secret
	s, err := a.local.CoreV1().Secrets(a.namespace).Get(ctx, a.secret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil, fmt.Errorf("there is no Argo CD cluster Secret %s", where)
	case apierrors.IsForbidden(err):
		return nil, nil, fmt.Errorf("forge-api may not read Argo CD's cluster Secret %s: apply its Role forge-api-remote-clusters", where)
	case err != nil:
		return nil, nil, fmt.Errorf("reading Argo CD's cluster Secret %s: %w", where, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kube != nil && s.ResourceVersion == a.version {
		return a.kube, a.dyn, nil
	}
	cfg, err := argoRESTConfig(s)
	if err != nil {
		return nil, nil, fmt.Errorf("the Argo CD cluster Secret %s: %w", where, err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err == nil {
		var dyn dynamic.Interface
		if dyn, err = dynamic.NewForConfig(cfg); err == nil {
			a.version, a.kube, a.dyn = s.ResourceVersion, kube, dyn
			return kube, dyn, nil
		}
	}
	// The library's text stays in the log: it is about certificates.
	if a.log != nil {
		a.log.WarnContext(ctx, "building a client from an Argo CD cluster Secret failed", "secret", where, "error", err)
	}
	return nil, nil, fmt.Errorf("the credential in the Argo CD cluster Secret %s is not usable", where)
}

// argoConfig is the part of an Argo CD cluster Secret's "config" that
// forge-api uses; the rest makes it refuse the Secret.
type argoConfig struct {
	BearerToken     string `json:"bearerToken"`
	TLSClientConfig struct {
		Insecure   bool   `json:"insecure"`
		ServerName string `json:"serverName"`
		CAData     []byte `json:"caData"`
		CertData   []byte `json:"certData"`
		KeyData    []byte `json:"keyData"`
	} `json:"tlsClientConfig"`
	Username           string          `json:"username"`
	ExecProviderConfig json.RawMessage `json:"execProviderConfig"`
	AWSAuthConfig      json.RawMessage `json:"awsAuthConfig"`
}

// argoRESTConfig turns an Argo CD cluster Secret into a client config. It
// takes a bearer token or a client certificate over verified TLS, nothing
// else: no exec plugins (they would run a program in forge-api), no cloud
// logins, no skipped verification.
func argoRESTConfig(s *corev1.Secret) (*rest.Config, error) {
	if s.Labels[argoLabel] != "cluster" {
		return nil, fmt.Errorf("it is not labelled %s=cluster", argoLabel)
	}
	server := string(s.Data["server"])
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("its server is not an https address")
	}
	var c argoConfig
	if err := json.Unmarshal(s.Data["config"], &c); err != nil {
		return nil, errors.New("its config is not JSON")
	}
	switch {
	case len(c.ExecProviderConfig) > 0 && string(c.ExecProviderConfig) != "null":
		return nil, errors.New("it logs in with an exec plugin, which forge-api does not run")
	case len(c.AWSAuthConfig) > 0 && string(c.AWSAuthConfig) != "null":
		return nil, errors.New("it logs in to AWS, which forge-api does not do")
	case c.Username != "":
		return nil, errors.New("it logs in with a password, which forge-api does not do")
	case c.TLSClientConfig.Insecure:
		return nil, errors.New("it skips TLS verification, which forge-api refuses")
	case c.BearerToken == "" && len(c.TLSClientConfig.CertData) == 0:
		return nil, errors.New("it has neither a bearer token nor a client certificate")
	}
	return &rest.Config{
		Host:        server,
		BearerToken: c.BearerToken,
		TLSClientConfig: rest.TLSClientConfig{
			ServerName: c.TLSClientConfig.ServerName,
			CAData:     c.TLSClientConfig.CAData,
			CertData:   c.TLSClientConfig.CertData,
			KeyData:    c.TLSClientConfig.KeyData,
		},
		// Within the server's 10-second bound on one read: the version call
		// has no context of its own.
		Timeout: 10 * time.Second,
	}, nil
}
