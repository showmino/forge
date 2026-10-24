package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
)

func TestClusters(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	ok := clusterFlags{on: true, name: "default", grafanaURL: "https://grafana.example.com/d/k8s", prometheusURL: "http://prometheus-prometheus.monitoring.svc:9090"}
	noKube := func() (kubernetes.Interface, dynamic.Interface, error) { return nil, nil, errors.New("no kubeconfig") }

	if c, err := clusters(clusterFlags{}, nil, log, noKube); c != nil || err != nil {
		t.Errorf("off: %v %v", c, err)
	}
	if c, err := clusters(ok, nil, log, fakeKube); c == nil || err != nil {
		t.Errorf("on: %v %v", c, err)
	}
	shared := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic()}
	if c, err := clusters(ok, shared, log, noKube); c == nil || err != nil {
		t.Errorf("with the previews' connection: %v %v", c, err)
	}
	for name, tc := range map[string]struct {
		f   clusterFlags
		msg string
	}{
		"bad name":       {clusterFlags{on: true, name: "Home Lab"}, "--cluster-name"},
		"no name":        {clusterFlags{on: true}, "--cluster-name"},
		"script grafana": {clusterFlags{on: true, name: "default", grafanaURL: "javascript:alert(1)"}, "--grafana-url"},
		"no host":        {clusterFlags{on: true, name: "default", grafanaURL: "https://"}, "--grafana-url"},
		"bad prometheus": {clusterFlags{on: true, name: "default", prometheusURL: "prometheus:9090"}, "--prometheus-url"},
		"no kube":        {clusterFlags{on: true, name: "default"}, "no kubeconfig"},
	} {
		if _, err := clusters(tc.f, nil, log, noKube); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.msg)
		}
	}
}
