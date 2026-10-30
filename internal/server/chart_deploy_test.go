package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/chartrepo"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/manifest"
)

const podinfoRepo = "oci://ghcr.io/stefanprodan/charts"

// fakeCharts has the charts "<repo>|<name>|<version>".
type fakeCharts map[string]bool

func (f fakeCharts) Has(_ context.Context, repo, name, version string) error {
	if f[repo+"|"+name+"|"+version] {
		return nil
	}
	return chartrepo.ErrNotFound
}

// newChartFixture is the deploy fixture with charts on and podinfo 6.7.1
// and 6.8.0 in its repository; tweak may change more.
func newChartFixture(t *testing.T, tweak func(*DeployOptions, *envtest.Argo)) *teardownFixture {
	t.Helper()
	f := &teardownFixture{deployFixture: newDeployFixture(t, false, func(o *DeployOptions, a *envtest.Argo) {
		o.Charts = true
		o.ChartChecker = fakeCharts{podinfoRepo + "|podinfo|6.7.1": true, podinfoRepo + "|podinfo|6.8.0": true, podinfoRepo + "|needs-admin|1.0.0": true}
		if tweak != nil {
			tweak(o, a)
		}
	})}
	f.catalog.EnableTeardown(f.deploys, nil, f.ops)
	return f
}

func podinfoRequest(id string) *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{
		ServiceId: id, OwnerTeam: "billing",
		Chart: &forgev1.ChartSource{
			Repo: podinfoRepo, Name: "podinfo", Version: "6.7.1",
			Values:           "replicaCount: 2\nui: {message: hello}\n",
			ProductionValues: "replicaCount: 3\n",
			ExposeService:    id, ExposePort: 9898,
		},
	}
}

func (f *deployFixture) read(t *testing.T, path string) string {
	t.Helper()
	data, err := f.repo.Read(context.Background(), path, "")
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func chartValuesOf(t *testing.T, f *deployFixture, svc, env string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := yaml.Unmarshal([]byte(f.read(t, chartValuesPath(svc, env))), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestChartServiceLifecycle(t *testing.T) {
	t.Parallel()
	f := newChartFixture(t, nil)
	ctx := context.Background()
	msgs, err := f.create(t, podinfoRequest("podinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if got := doneSteps(msgs); got != "check_chart,write_manifest,register" {
		t.Errorf("steps %s", got)
	}
	m, err := manifest.Parse([]byte(f.read(t, "services/podinfo/forge.yaml")))
	if err != nil || m.Chart == nil || m.Chart.Version != "6.7.1" || m.Chart.Values["replicaCount"] != 2 || m.Chart.EnvValues("production")["replicaCount"] != 3 {
		t.Fatalf("forge.yaml: %+v %v", m, err)
	}

	// Staging: Forge's values first (the address), then the chart.
	op := f.deployImage(t, "podinfo", "staging", "")
	if got := eventSteps(op); got != "resolve,release,commit,namespace,chart,address,sync" {
		t.Errorf("steps %s", got)
	}
	v, _, err := f.deploys.current(ctx, "podinfo", "staging")
	if err != nil || v.Chart == nil || v.Chart.Version != "6.7.1" || v.Image != podinfoRepo+"/podinfo:6.7.1" || v.Port != 9898 ||
		v.Forge.Version != "6.7.1" || v.Forge.Commit != "6.7.1" || v.Replicas != 0 || v.Health != "" {
		t.Errorf("staging values: %+v %v", v, err)
	}
	var src map[string]string
	if err := json.Unmarshal([]byte(f.read(t, chartPath("podinfo", "staging"))), &src); err != nil ||
		src["repoURL"] != podinfoRepo+"/podinfo" || src["chart"] != "" || src["chartPath"] != "." || src["version"] != "6.7.1" {
		t.Errorf("chart.json: %v %v", src, err)
	}
	if cv := chartValuesOf(t, f.deployFixture, "podinfo", "staging"); cv["replicaCount"] != 2 {
		t.Errorf("staging chart values: %v", cv)
	}
	ns, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "podinfo-staging", metav1.GetOptions{})
	if err != nil || ns.Labels[podSecurityEnforce] != "baseline" || ns.Labels[podSecurityWarn] != "restricted" {
		t.Errorf("namespace labels: %v %v", ns.GetLabels(), err)
	}
	list, err := f.client.ListDeployments(ctx, as("reader", &forgev1.ListDeploymentsRequest{Service: "services/podinfo"}))
	if err != nil || len(list.Msg.GetDeployments()) != 1 || list.Msg.GetDeployments()[0].GetHealth() != forgev1.Deployment_HEALTH_HEALTHY {
		t.Errorf("list: %v %v", list, err)
	}

	// A new version and new values: deployed, then kept in forge.yaml.
	resp, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{
		Service: "services/podinfo", Env: "staging", Ref: "6.8.0", ChartValues: "replicaCount: 5\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("upgrade: %v", op.GetError())
	}
	if cv := chartValuesOf(t, f.deployFixture, "podinfo", "staging"); cv["replicaCount"] != 5 || cv["ui"] != nil {
		t.Errorf("upgraded chart values: %v", cv)
	}
	m, err = manifest.Parse([]byte(f.read(t, "services/podinfo/forge.yaml")))
	if err != nil || m.Chart.Version != "6.8.0" || m.Chart.Values["replicaCount"] != 5 || m.Chart.EnvValues("production")["replicaCount"] != 3 {
		t.Errorf("forge.yaml after the upgrade: %+v %v", m.Chart, err)
	}

	// Production: staging's version and shared values, its own values over them.
	p, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/podinfo"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, p.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("promote: %v", op.GetError())
	}
	if v, _, _ := f.deploys.current(ctx, "podinfo", "production"); v.Chart == nil || v.Chart.Version != "6.8.0" || v.Forge.Version != "6.8.0" || v.Forge.Kind != kindPromote {
		t.Errorf("production values: %+v", v)
	}
	if cv := chartValuesOf(t, f.deployFixture, "podinfo", "production"); cv["replicaCount"] != 3 {
		t.Errorf("production chart values: %v", cv)
	}

	// Rollback: staging runs 6.7.1 with its values again.
	r, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/podinfo/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, r.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("rollback: %v", op.GetError())
	}
	if !strings.Contains(f.read(t, chartPath("podinfo", "staging")), `"version": "6.7.1"`) {
		t.Errorf("chart.json after the rollback: %s", f.read(t, chartPath("podinfo", "staging")))
	}
	if cv := chartValuesOf(t, f.deployFixture, "podinfo", "staging"); cv["replicaCount"] != 2 {
		t.Errorf("chart values after the rollback: %v", cv)
	}

	// Delete: the charts go first, then the rest.
	del, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/podinfo"})
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, del.GetName()); op.GetError() != nil {
		t.Fatalf("delete: %v", op.GetError())
	}
	for _, p := range []string{chartPath("podinfo", "staging"), chartValuesPath("podinfo", "production"), "services/podinfo/forge.yaml"} {
		if _, err := f.repo.Read(ctx, p, ""); err == nil {
			t.Errorf("%s left in the deploy repo", p)
		}
	}
}

// A chart that makes something its project does not allow fails its
// deploy, with what it made and what to do.
func TestChartNotAllowed(t *testing.T) {
	t.Parallel()
	f := newChartFixture(t, func(_ *DeployOptions, a *envtest.Argo) {
		a.Refused = map[string]string{"admin-staging-chart": "resource rbac.authorization.k8s.io:ClusterRole is not permitted in project forge-chart-admin-staging."}
	})
	req := podinfoRequest("admin")
	req.Chart.Name = "needs-admin"
	req.Chart.Version = "1.0.0"
	if _, err := f.create(t, req); err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/admin", Env: "staging"}))
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, resp.Msg.GetOperation().GetName())
	if e := op.GetError(); e.GetCode() != "chart_not_allowed" || !strings.Contains(e.GetMessage(), "makes a ClusterRole (rbac.authorization.k8s.io)") {
		t.Errorf("error: %v", e)
	}
}

// A refusal of an earlier sync, still in the app's status, does not fail a
// deploy that Argo CD has since synced.
func TestChartStaleRefusal(t *testing.T) {
	t.Parallel()
	f := newChartFixture(t, func(_ *DeployOptions, a *envtest.Argo) {
		a.StaleRefusal = map[string]string{"podinfo-staging-chart": "resource rbac.authorization.k8s.io:ClusterRole is not permitted in project forge-chart-podinfo-staging."}
	})
	if _, err := f.create(t, podinfoRequest("podinfo")); err != nil {
		t.Fatal(err)
	}
	f.deployImage(t, "podinfo", "staging", "")
}

func TestChartRefusedMessages(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]string{
		"resource rbac.authorization.k8s.io:ClusterRole is not permitted in project forge-chart-a-staging.": "makes a ClusterRole (rbac.authorization.k8s.io)",
		"Resource :Namespace is not permitted in project forge-chart-a-staging":                             "makes a Namespace",
		"namespace payments-production is not permitted in project 'forge-chart-a-staging'":                 "writes into the namespace payments-production",
	} {
		err := chartRefused("a", "staging", msg)
		var oe *OpError
		if !errors.As(err, &oe) || oe.Code != "chart_not_allowed" || !strings.Contains(oe.Message, want) {
			t.Errorf("%q: %v, want %q", msg, err, want)
		}
	}
	if err := chartRefused("a", "staging", "one or more objects failed to apply"); err != nil {
		t.Errorf("another message: %v", err)
	}
}

func TestChartRefusals(t *testing.T) {
	t.Parallel()
	f := newChartFixture(t, func(o *DeployOptions, _ *envtest.Argo) {
		o.AllowedChartRepos = []string{podinfoRepo}
	})
	for name, tc := range map[string]struct {
		mod  func(*forgev1.CreateServiceRequest)
		code connect.Code
		want string
	}{
		"a version the repository lacks": {func(r *forgev1.CreateServiceRequest) { r.Chart.Version = "9.9.9" }, connect.CodeFailedPrecondition, "has no chart podinfo at version 9.9.9"},
		"a repository not allowed":       {func(r *forgev1.CreateServiceRequest) { r.Chart.Repo = "https://charts.example.com" }, connect.CodeFailedPrecondition, "charts may only come from " + podinfoRepo},
		"a version range":                {func(r *forgev1.CreateServiceRequest) { r.Chart.Version = "^6.7.0" }, connect.CodeInvalidArgument, "one exact version"},
		"bad values":                     {func(r *forgev1.CreateServiceRequest) { r.Chart.Values = "- a list" }, connect.CodeInvalidArgument, "YAML of the chart's values"},
		"run options":                    {func(r *forgev1.CreateServiceRequest) { r.RunOptions = &forgev1.RunOptions{Replicas: 2} }, connect.CodeInvalidArgument, "takes only the address"},
		"an image too":                   {func(r *forgev1.CreateServiceRequest) { r.Image = "nginx:1.27" }, connect.CodeInvalidArgument, "one of a template, an image, a repo or a chart"},
	} {
		req := podinfoRequest("p" + strconv.Itoa(len(name)) + strings.ReplaceAll(name, " ", "")[:3])
		tc.mod(req)
		_, err := f.create(t, req)
		if connect.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %v with %q", name, err, tc.code, tc.want)
		}
	}

	// Charts off: refused before anything is written.
	off := newChartFixture(t, func(o *DeployOptions, _ *envtest.Argo) { o.Charts = false })
	if _, err := off.create(t, podinfoRequest("podinfo")); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "charts are off") {
		t.Errorf("charts off: %v", err)
	}

	// Chart values are for a chart service only.
	if _, err := f.create(t, podinfoRequest("podinfo")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ChartValues: "a: 1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("chart values for a repo's service: %v", err)
	}
	if _, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/podinfo", Env: "staging", ChartValues: "[1, 2]"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("chart values that are a list: %v", err)
	}
	// Values that, with the environments', would not fit forge.yaml.
	big := "a: " + strings.Repeat("x", manifest.MaxChartValues-5) + "\n"
	if _, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/podinfo", Env: "staging", ChartValues: big})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(fmt.Sprint(err), "at most") {
		t.Errorf("values too big with the environments': %v", err)
	}
}
