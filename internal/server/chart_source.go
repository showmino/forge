package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"

	"github.com/showmino/forge/internal/chartrepo"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/policy"
)

// Services that run a published Helm chart (phase 11). Their forge.yaml, with
// chart:, lives in the deploy repo as an image service's does. Each
// environment's directory then holds, next to values.yaml (Forge's own chart:
// the address, the quota, the DNS record, and the record of the deploy),
// chart.json (where the chart is, read by the forge-charts ApplicationSet)
// and chart-values.yaml (its values). Argo CD runs the chart as
// <service>-<env>-chart, in the AppProject forge-charts, which allows only
// namespaced resources.

// ChartChecker finds a chart's version in its repository: nil when it is
// there, an error wrapping chartrepo.ErrNotFound when it is not.
type ChartChecker interface {
	Has(ctx context.Context, repo, name, version string) error
}

// deployChart is values.yaml's chart: what the environment runs. Forge's own
// chart reads only expose; the rest is the record a rollback restores.
type deployChart struct {
	Repo    string `yaml:"repo"`
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	// Values are the chart values of every environment, EnvValues this
	// environment's, merged over them.
	Values    map[string]any       `yaml:"values,omitempty"`
	EnvValues map[string]any       `yaml:"envValues,omitempty"`
	Expose    manifest.ChartExpose `yaml:"expose"`
}

func chartPath(svc, env string) string { return "services/" + svc + "/" + env + "/chart.json" }

func chartValuesPath(svc, env string) string {
	return "services/" + svc + "/" + env + "/chart-values.yaml"
}

// chartApp is the Argo CD application that runs the chart.
func chartApp(svc, env string) string { return svc + "-" + env + "-chart" }

// ref is how the chart is shown: oci://ghcr.io/acme/charts/podinfo:6.7.1.
func (c *deployChart) ref() string { return c.Repo + "/" + c.Name + ":" + c.Version }

// argoSource is chart.json: the source the forge-charts ApplicationSet
// gives the application. An OCI chart is its repository and name as one
// repoURL, at path "."; one from an HTTPS repository is repoURL plus chart.
func (c *deployChart) argoSource() []byte {
	src := map[string]string{"repoURL": c.Repo, "chart": c.Name, "chartPath": "", "version": c.Version}
	if strings.HasPrefix(c.Repo, "oci://") {
		src = map[string]string{"repoURL": c.Repo + "/" + c.Name, "chart": "", "chartPath": ".", "version": c.Version}
	}
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		panic(err) // strings always marshal
	}
	return append(b, '\n')
}

// values is chart-values.yaml: the environment's values over the shared ones.
func (c *deployChart) values() ([]byte, error) {
	merged := mergeValues(c.Values, c.EnvValues)
	if len(merged) == 0 {
		return []byte("{}\n"), nil
	}
	b, err := yaml.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by Forge from forge.yaml's chart values.\n"), b...), nil
}

// mergeValues is over merged into base, as Helm merges values files: maps
// key by key, anything else replaced. Neither argument is changed.
func mergeValues(base, over map[string]any) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range over {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = mergeValues(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// chartsReady says why this Forge API cannot run charts, or nil.
func (d *Deploys) chartsReady() error {
	if !d.o.Charts {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("charts are off on this Forge API (the chart value deploys.charts.enabled)"))
	}
	if d.o.ChartChecker == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge API cannot look up charts"))
	}
	return nil
}

// checkChart checks a chart against the chart rules and finds its version
// in its repository.
func (d *Deploys) checkChart(ctx context.Context, svc string, c *manifest.Chart) error {
	if err := d.chartsReady(); err != nil {
		return err
	}
	u, err := url.Parse(c.Repo)
	if err != nil {
		return invalidArg("chart.repo: %v", err)
	}
	if d.o.Policy != nil {
		res, err := d.o.Policy.EvaluateChart(ctx, policy.Chart{
			Service: svc, Repo: c.Repo, Host: u.Host, Name: c.Name, Version: c.Version,
			AllowedRepos: d.o.AllowedChartRepos, Actor: policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
		})
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("check the chart rules: %w", err))
		}
		noteAuditDetail(ctx, "policy_rules", d.o.Policy.Fingerprint())
		if res.Denied() {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the chart rules refuse it: %s", strings.Join(res.Deny, "; ")))
		}
	}
	err = d.o.ChartChecker.Has(ctx, c.Repo, c.Name, c.Version)
	switch {
	case errors.Is(err, chartrepo.ErrNotFound):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no chart %s at version %s: check the repository, the name and the version", c.Repo, c.Name, c.Version))
	case err != nil:
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("look up %s %s in %s: %w", c.Name, c.Version, c.Repo, err))
	}
	return nil
}

// chartSource is what a deploy of sid, a service that runs mf's chart, runs:
// the chart at version (default forge.yaml's), with values (default
// forge.yaml's chart.values). retag reports that version or values change
// forge.yaml, which the deploy writes there once it has succeeded.
func (d *Deploys) chartSource(ctx context.Context, sid string, mf *manifest.Manifest, version string, values map[string]any) (src source, retag bool, err error) {
	c := *mf.Chart
	if version != "" && version != c.Version {
		if err := manifest.ValidChartVersion(version); err != nil {
			return src, false, invalidArg("%v", err)
		}
		c.Version, retag = version, true
	}
	if values != nil {
		c.Values, retag = values, true
	}
	if err := d.checkChart(ctx, sid, &c); err != nil {
		return src, false, err
	}
	src = source{
		serviceID: sid, ref: c.Version, commit: c.Version,
		port:   int32(c.Expose.Port), //nolint:gosec // Parse checks the port is 1-65535
		domain: mf.RunDomain(), host: mf.RunHost(), stagingInternal: mf.StagingInternal(), run: mf.Run,
		chart: &c,
	}
	src.image = (&deployChart{Repo: c.Repo, Name: c.Name, Version: c.Version}).ref()
	return src, retag, nil
}

// deployChartFor is the chart record of a deploy of c to env.
func deployChartFor(c *manifest.Chart, env string) *deployChart {
	return &deployChart{Repo: c.Repo, Name: c.Name, Version: c.Version, Values: c.Values, EnvValues: c.EnvValues(env), Expose: c.Expose}
}

// rewriteChart writes the chart version and values a deploy ran into the
// forge.yaml of sid, as it is at the deploy repo's head.
func (d *Deploys) rewriteChart(ctx context.Context, sid string, c *manifest.Chart, message string, emit func(string, string)) error {
	build := func() ([]byte, error) {
		data, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", manifestPath(sid), err)
		}
		if data, err = manifest.SetChartVersion(data, c.Version); err != nil {
			return nil, err
		}
		return manifest.SetChartValues(data, c.Values)
	}
	sha, err := d.commit(ctx, manifestPath(sid), build, message, emit)
	if err != nil {
		return err
	}
	emit("manifest", fmt.Sprintf("forge-deploys %s: chart %s %s", shortSHA(sha), c.Name, c.Version))
	return nil
}

// manifestChart reads the chart: of the forge.yaml of sid in the deploy repo,
// nil when it runs none.
func (d *Deploys) manifestChart(ctx context.Context, sid string) (*manifest.Chart, error) {
	data, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
	if err != nil {
		return nil, err
	}
	mf, err := manifest.Parse(data)
	if err != nil {
		return nil, err
	}
	return mf.Chart, nil
}

// applyChart is apply for a service that runs a chart: values.yaml first, so
// that forge-services makes the namespace, the address and the quota; then,
// once the namespace is there and refuses privileged pods, the chart.
func (d *Deploys) applyChart(ctx context.Context, svc, env string, v deployValues, emit func(string, string)) error {
	chartValues, err := v.Chart.values()
	if err != nil {
		return fmt.Errorf("chart values: %w", err)
	}
	rec := v.Forge
	rec.Image = v.Image
	content := v.marshal()
	commit, err := d.commit(ctx, valuesPath(svc, env), func() ([]byte, error) { return content, nil }, commitMessage(rec), emit)
	if err != nil {
		return err
	}
	emit("commit", fmt.Sprintf("forge-deploys %s: %s", shortSHA(commit), v.Image))
	stands := func(err error) error {
		if ctx.Err() != nil {
			return &OpError{Code: OpCodeCancelled, Message: fmt.Sprintf("cancelled; the deploy commit %s stands and Argo CD keeps applying it", shortSHA(commit))}
		}
		return err
	}
	if err := d.waitNamespace(ctx, svc, env); err != nil {
		return stands(err)
	}
	if err := d.o.Cluster.RestrictPodSecurity(ctx, svc, env); err != nil {
		return stands(fmt.Errorf("pod security for %s-%s: %w", svc, env, err))
	}
	emit("namespace", svc+"-"+env+": pods may not be privileged or reach the node")
	source := v.Chart.argoSource()
	files := map[string][]byte{chartPath(svc, env): source, chartValuesPath(svc, env): chartValues}
	chartCommit, err := d.commitFiles(ctx, files, fmt.Sprintf("chart %s %s for %s in %s (%s)", v.Chart.Name, v.Chart.Version, svc, env, rec.Actor))
	if err != nil {
		return stands(err)
	}
	emit("chart", fmt.Sprintf("forge-deploys %s: %s", shortSHA(chartCommit), v.Chart.ref()))
	if err := d.awaitApp(ctx, svc, env, svc+"-"+env, valuesPath(svc, env), commit, content, "address", emit); err != nil {
		return stands(err)
	}
	return stands(d.awaitApp(ctx, svc, env, chartApp(svc, env), chartPath(svc, env), chartCommit, source, "sync", emit))
}

// commitFiles writes files in one commit on the deploy repo's head,
// retrying when the branch moved (3 attempts).
func (d *Deploys) commitFiles(ctx context.Context, files map[string][]byte, message string) (string, error) {
	for range 3 {
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return "", fmt.Errorf("deploy repo head: %w", err)
		}
		sha, err := d.o.Repo.CommitFiles(ctx, head, tree, files, message)
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		return sha, err
	}
	return "", &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; try again"}
}

// removeChartFiles takes the chart out of svc/env: Argo CD then removes what
// it made. Safe to repeat.
func (d *Deploys) removeChartFiles(ctx context.Context, svc, env, actor string) (bool, error) {
	for range 3 {
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return false, fmt.Errorf("deploy repo head: %w", err)
		}
		files := map[string][]byte{}
		for _, p := range []string{chartPath(svc, env), chartValuesPath(svc, env)} {
			if _, err := d.o.Repo.Read(ctx, p, head); err == nil {
				files[p] = nil
			} else if !errors.Is(err, github.ErrNotFound) {
				return false, fmt.Errorf("read %s: %w", p, err)
			}
		}
		if len(files) == 0 {
			return false, nil
		}
		_, err = d.o.Repo.CommitFiles(ctx, head, tree, files, fmt.Sprintf("remove the chart of %s from %s (%s)", svc, env, actor))
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		return err == nil, err
	}
	return false, &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; try again"}
}

// notPermitted is Argo CD's message for an object its project does not
// allow, such as "resource rbac.authorization.k8s.io:ClusterRole is not
// permitted in project forge-chart-a-staging"; otherNamespace its message
// for one in a namespace the project does not allow.
var (
	notPermitted   = regexp.MustCompile(`(?i)resource (\S*):(\S+?) is not permitted in project`)
	otherNamespace = regexp.MustCompile(`(?i)namespace (\S+) is not permitted in project`)
)

// chartRefused turns Argo CD's refusal of a chart's object into what to do,
// or nil when message is no such refusal.
func chartRefused(svc, env, message string) error {
	var what string
	if m := notPermitted.FindStringSubmatch(message); m != nil {
		what = "makes a " + m[2]
		if m[1] != "" {
			what += " (" + m[1] + ")"
		}
	} else if m := otherNamespace.FindStringSubmatch(message); m != nil {
		what = "writes into the namespace " + strings.Trim(m[1], `"'`)
	} else {
		return nil
	}
	return &OpError{Code: "chart_not_allowed", Message: fmt.Sprintf(
		"the chart of %s in %s %s, which Forge does not let a chart do: a chart may only make resources in its own namespace (%s-%s), and not routes, quotas, DNS records or secrets of the platform. Turn it off in the chart's values, or ask the platform team to install the chart by hand. Argo CD: %s",
		svc, env, what, svc, env, clip(message))}
}

// appStatus is Argo CD's status of what runs svc in env: for a service that
// runs a chart, the worse of its two applications.
func (d *Deploys) appStatus(ctx context.Context, svc, env string, v deployValues) (AppStatus, error) {
	st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
	if err != nil || v.Chart == nil {
		return st, err
	}
	cst, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, chartApp(svc, env))
	if err != nil {
		return st, err
	}
	return mergeStatus(st, cst), nil
}

// healthRank orders Argo CD's health states from best to worst.
var healthRank = map[string]int{"Healthy": 0, "Suspended": 1, "Progressing": 2, "Unknown": 3, "Missing": 4, "Degraded": 5}

func mergeStatus(a, b AppStatus) AppStatus {
	out := a
	if healthRank[b.Health] > healthRank[a.Health] {
		out.Health = b.Health
	}
	if b.Sync != "Synced" {
		out.Sync = b.Sync
	}
	switch {
	case a.Message == "":
		out.Message = b.Message
	case b.Message != "":
		out.Message = a.Message + "; chart: " + b.Message
	}
	return out
}

// parseChartValues reads chart values given as YAML: a mapping, or nothing.
func parseChartValues(field, text string) (map[string]any, error) {
	if len(text) > manifest.MaxChartValues {
		return nil, invalidArg("%s takes %d bytes; at most %d", field, len(text), manifest.MaxChartValues)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(text), &values); err != nil {
		return nil, invalidArg("%s must be YAML of the chart's values: %v", field, err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// chartAllowed checks, for a promotion or a rollback, the chart a deploy
// record names under today's chart rules, and that nothing else holds its
// application's name.
func (d *Deploys) chartAllowed(ctx context.Context, svc, env string, c *deployChart) error {
	if err := d.checkChart(ctx, svc, &manifest.Chart{Repo: c.Repo, Name: c.Name, Version: c.Version}); err != nil {
		return err
	}
	return d.o.Cluster.ApplicationFree(ctx, d.o.ArgoNamespace, chartApp(svc, env))
}
