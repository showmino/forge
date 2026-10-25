package main

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
		"bad name":         {clusterFlags{on: true, name: "Home Lab"}, "--cluster-name"},
		"no name":          {clusterFlags{on: true}, "--cluster-name"},
		"script grafana":   {clusterFlags{on: true, name: "default", grafanaURL: "javascript:alert(1)"}, "--grafana-url"},
		"no host":          {clusterFlags{on: true, name: "default", grafanaURL: "https://"}, "--grafana-url"},
		"bad prometheus":   {clusterFlags{on: true, name: "default", prometheusURL: "prometheus:9090"}, "--prometheus-url"},
		"no kube":          {clusterFlags{on: true, name: "default"}, "no kubeconfig"},
		"remotes when off": {clusterFlags{remotes: "aks"}, "needs --clusters"},
		"bad remote":       {clusterFlags{on: true, name: "default", remotes: "aks,AKS"}, `"AKS"`},
		"empty remote":     {clusterFlags{on: true, name: "default", remotes: "aks,"}, `""`},
		"remote twice":     {clusterFlags{on: true, name: "default", remotes: "aks,aks"}, "named twice"},
		"remote is local":  {clusterFlags{on: true, name: "default", remotes: "default"}, "named twice"},
	} {
		if _, err := clusters(tc.f, nil, log, noKube); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.msg)
		}
	}
	if c, err := clusters(clusterFlags{on: true, name: "production", remotes: "aks-production,lab"}, shared, log, noKube); c == nil || err != nil {
		t.Errorf("with remotes: %v %v", c, err)
	}
}

func argoSecret(server, config string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aks", Namespace: "argocd", ResourceVersion: "1", Labels: map[string]string{argoLabel: "cluster"}},
		Data:       map[string][]byte{"name": []byte("aks"), "server": []byte(server), "config": []byte(config)},
	}
}

func TestArgoRESTConfig(t *testing.T) {
	t.Parallel()
	ca := base64.StdEncoding.EncodeToString([]byte("CA"))
	cfg, err := argoRESTConfig(argoSecret("https://aks.example.com:443", `{"bearerToken":"tok","tlsClientConfig":{"insecure":false,"caData":"`+ca+`"}}`))
	if err != nil || cfg.Host != "https://aks.example.com:443" || cfg.BearerToken != "tok" || string(cfg.CAData) != "CA" || cfg.Insecure || cfg.Timeout == 0 {
		t.Fatalf("token and CA: %+v %v", cfg, err)
	}
	if _, err := argoRESTConfig(argoSecret("https://aks.example.com", `{"tlsClientConfig":{"certData":"`+ca+`","keyData":"`+ca+`"}}`)); err != nil {
		t.Errorf("client certificate: %v", err)
	}
	unlabelled := argoSecret("https://aks.example.com", `{"bearerToken":"tok"}`)
	unlabelled.Labels = nil
	for name, tc := range map[string]struct {
		s   *corev1.Secret
		msg string
	}{
		"not a cluster": {unlabelled, "not labelled"},
		"plain http":    {argoSecret("http://aks.example.com", `{"bearerToken":"tok"}`), "https"},
		"user in url":   {argoSecret("https://u:p@aks.example.com", `{"bearerToken":"tok"}`), "https"},
		"query in url":  {argoSecret("https://aks.example.com/?token=x", `{"bearerToken":"tok"}`), "https"},
		"no server":     {argoSecret("", `{"bearerToken":"tok"}`), "https"},
		"bad config":    {argoSecret("https://aks.example.com", `{`), "not JSON"},
		"exec":          {argoSecret("https://aks.example.com", `{"execProviderConfig":{"command":"sh"}}`), "exec plugin"},
		"aws":           {argoSecret("https://aks.example.com", `{"awsAuthConfig":{"clusterName":"x"}}`), "AWS"},
		"password":      {argoSecret("https://aks.example.com", `{"username":"admin","password":"x"}`), "password"},
		"insecure":      {argoSecret("https://aks.example.com", `{"bearerToken":"tok","tlsClientConfig":{"insecure":true}}`), "TLS verification"},
		"no credential": {argoSecret("https://aks.example.com", `{"tlsClientConfig":{"caData":"`+ca+`"}}`), "neither"},
	} {
		if _, err := argoRESTConfig(tc.s); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.msg)
		}
	}
}

func TestArgoClusterConnect(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := argoSecret("https://aks.example.com", `{"bearerToken":"tok"}`)
	local := fake.NewClientset(s)
	a := &argoCluster{local: local, namespace: "argocd", secret: "aks"}
	k1, _, err := a.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if k2, _, _ := a.connect(ctx); k2 != k1 {
		t.Error("an unchanged Secret keeps its clients")
	}
	s.ResourceVersion = "2"
	if _, err := local.CoreV1().Secrets("argocd").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if k3, _, _ := a.connect(ctx); k3 == k1 {
		t.Error("a changed Secret gets new clients")
	}

	missing := &argoCluster{local: fake.NewClientset(), namespace: "argocd", secret: "aks"}
	if _, _, err := missing.connect(ctx); err == nil || !strings.Contains(err.Error(), "no Argo CD cluster Secret argocd/aks") {
		t.Errorf("missing: %v", err)
	}
	denied := fake.NewClientset()
	denied.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "aks", nil)
	})
	if _, _, err := (&argoCluster{local: denied, namespace: "argocd", secret: "aks"}).connect(ctx); err == nil || !strings.Contains(err.Error(), "forge-api-remote-clusters") {
		t.Errorf("forbidden: %v", err)
	}
	bad := &argoCluster{local: fake.NewClientset(argoSecret("http://aks.example.com", `{"bearerToken":"tok"}`)), namespace: "argocd", secret: "aks"}
	if _, _, err := bad.connect(ctx); err == nil || !strings.Contains(err.Error(), "argocd/aks: its server") {
		t.Errorf("bad Secret: %v", err)
	}
}
