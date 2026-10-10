package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// resourceFlags are forge-api's settings for resources and their runner.
type resourceFlags struct {
	on bool
	// runnerImage is the image of the runner Job; runnerNamespace and
	// runnerServiceAccount are where and as whom it runs; runnerAPIURL is
	// how it reaches forge-api's runner listener.
	runnerImage, runnerNamespace, runnerServiceAccount, runnerAPIURL string
}

var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// resources returns the resource service and the handler of its runner
// listener, or nils when resources are off. It needs deploys: the cluster
// connection, and the service namespaces deploys and databases share. The
// policy rules are compiled here, so rules that do not compile stop forge-api
// from starting.
func resources(ctx context.Context, f resourceFlags, st store.ResourceStore, cluster *server.Cluster, deploysOn bool,
	catalog *server.Catalog, ops *server.Operations, log *slog.Logger) (*server.Resources, http.Handler, error) {
	if !f.on {
		return nil, nil, nil
	}
	switch {
	case cluster == nil || !deploysOn:
		return nil, nil, errors.New("--resources needs --deploys (the cluster connection and the service namespaces)")
	case f.runnerImage == "":
		return nil, nil, errors.New("--resources needs --runner-image (FORGE_RUNNER_IMAGE)")
	case !k8sName.MatchString(f.runnerNamespace):
		return nil, nil, fmt.Errorf("--runner-namespace %q must be a namespace name", f.runnerNamespace)
	case !k8sName.MatchString(f.runnerServiceAccount):
		return nil, nil, fmt.Errorf("--runner-service-account %q must be a service account name", f.runnerServiceAccount)
	}
	if u, err := url.Parse(f.runnerAPIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, fmt.Errorf("--runner-api-url %q must be an http or https URL", f.runnerAPIURL)
	}
	rules, err := policy.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res, err := server.NewResources(server.ResourceOptions{
		Store: st, Catalog: catalog, Operations: ops, Policy: rules,
		Launcher: &server.JobLauncher{
			Kube: cluster.Kube, Namespace: f.runnerNamespace, Image: f.runnerImage, ServiceAccount: f.runnerServiceAccount,
			APIURL: f.runnerAPIURL, PullSecret: cluster.PullSecretData,
		},
		Namespaces: cluster, Health: cluster, Log: log,
	})
	if err != nil {
		return nil, nil, err
	}
	log.Info("resources are on", "policy_rules", rules.Fingerprint(), "runner_image", f.runnerImage, "runner_namespace", f.runnerNamespace)
	return res, server.RunnerHandler(res, log), nil
}
