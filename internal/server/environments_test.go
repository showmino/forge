package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
)

const envSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// envFixture is a forge-api with previews on fakes: service "payments"
// (repo showmino/payments, forge.yaml with run port 9090 and health /ready),
// ref main at envSHA, and its image present unless noImage.
type envFixture struct {
	cluster *Cluster
	gh      *envtest.GitHub
	images  *envtest.Images
	ops     *Operations
	envs    *Environments
	client  forgev1connect.EnvironmentServiceClient
	clock   *clock
	image   string
}

func newEnvFixture(t *testing.T, noImage bool, tweak func(*EnvOptions)) *envFixture {
	t.Helper()
	f := &envFixture{
		cluster: &Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"},
		gh: &envtest.GitHub{
			Refs:  map[string]string{"main": envSHA},
			Files: map[string][]byte{envSHA + ":forge.yaml": []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  health: /ready\n")},
		},
		images: &envtest.Images{},
		ops:    NewOperations(nil),
		clock:  newClock(),
		image:  "ghcr.io/showmino/payments:1a2b3c4",
	}
	if !noImage {
		f.images.Add(f.image)
	}
	catalog := NewCatalog(nil)
	_, err := catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "norepo", Service: &forgev1.Service{OwnerTeam: "billing"},
	}))
	_, _ = catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "elsewhere", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/acme/elsewhere"},
	}))
	o := EnvOptions{
		Cluster: f.cluster, GitHub: f.gh, Images: f.images, Catalog: catalog, Operations: f.ops,
		Org: "showmino", Domain: "preview.example",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, ReadyTimeout: 5 * time.Second, DestroyTimeout: 5 * time.Second,
		Now: f.clock.now,
	}
	if tweak != nil {
		tweak(&o)
	}
	f.envs = NewEnvironments(o)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer:   Authorizer{WriterGroups: []string{"platform"}},
		Catalog:      catalog,
		Operations:   f.ops,
		Environments: f.envs,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewEnvironmentServiceClient(srv.Client(), srv.URL)
	return f
}

// available plays the Deployment controller for the test's lifetime.
func (f *envFixture) available(t *testing.T, skip func(string) bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { envtest.MarkAvailable(ctx, f.cluster.Kube, skip) })
	t.Cleanup(func() { cancel(); wg.Wait() })
}

func (f *envFixture) create(t *testing.T, req *forgev1.CreateEnvironmentRequest) (*forgev1.CreateEnvironmentResponse, error) {
	t.Helper()
	if req.Service == "" {
		req.Service = "services/payments"
	}
	resp, err := f.client.CreateEnvironment(context.Background(), as("writer", req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f *envFixture) wait(t *testing.T, op *forgev1.Operation) *forgev1.Operation {
	t.Helper()
	return wait(t, f.ops, op.GetName())
}

func eventSteps(op *forgev1.Operation) string {
	var steps []string
	for _, ev := range op.GetEvents() {
		if len(steps) == 0 || steps[len(steps)-1] != ev.GetStep() {
			steps = append(steps, ev.GetStep())
		}
	}
	return strings.Join(steps, ",")
}

func TestCreateEnvironmentReady(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-1", Ttl: durationpb.New(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	env := resp.GetEnvironment()
	if env.GetName() != "environments/pr-1" || env.GetCommit() != envSHA || env.GetImage() != f.image || env.GetUrl() != "http://pr-1.preview.example" {
		t.Errorf("response environment: %v", env)
	}
	op := f.wait(t, resp.GetOperation())
	if op.GetError() != nil {
		t.Fatalf("operation failed: %v", op.GetError())
	}
	if got := eventSteps(op); got != "resolve,image,namespace,apply,ready" {
		t.Errorf("steps %s", got)
	}
	ns, err := f.cluster.Env(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	a := ns.Annotations
	wantExpire := f.clock.now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if a[annState] != stateReady || a[annRef] != "main" || a[annCommit] != envSHA || a[annImage] != f.image || a[annCreator] != "ana" || a[annExpire] != wantExpire {
		t.Errorf("annotations: %v", a)
	}
	if ns.Labels[labelService] != "payments" {
		t.Errorf("labels: %v", ns.Labels)
	}
	d, err := f.cluster.Kube.AppsV1().Deployments("preview-pr-1").Get(context.Background(), "payments", metav1.GetOptions{})
	if err != nil || d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != 9090 || d.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Errorf("deployment from forge.yaml run: %v %v", d, err)
	}
}

func TestCreateEnvironmentDefaultsWithoutForgeYAML(t *testing.T) {
	t.Parallel()
	// Swap GitHub through the options, never by writing a fake the running
	// server reads: the race detector sees no ordering through loopback TCP.
	f := newEnvFixture(t, false, func(o *EnvOptions) { o.GitHub = &envtest.GitHub{Refs: map[string]string{"main": envSHA}} })
	f.available(t, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "pr-2"})
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	d, _ := f.cluster.Kube.AppsV1().Deployments("preview-pr-2").Get(context.Background(), "payments", metav1.GetOptions{})
	if c := d.Spec.Template.Spec.Containers[0]; c.Ports[0].ContainerPort != 8080 || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("defaults: %+v", c)
	}
	ns, _ := f.cluster.Env(context.Background(), "pr-2")
	if ns.Annotations[annExpire] != f.clock.now().Add(24*time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("default ttl: %v", ns.Annotations[annExpire])
	}
}

func TestCreateEnvironmentValidation(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	for _, tc := range []struct {
		name string
		req  *forgev1.CreateEnvironmentRequest
		code connect.Code
	}{
		{"bad id", &forgev1.CreateEnvironmentRequest{EnvironmentId: "PR_1"}, connect.CodeInvalidArgument},
		{"41 chars", &forgev1.CreateEnvironmentRequest{EnvironmentId: strings.Repeat("a", 41)}, connect.CodeInvalidArgument},
		{"ttl too short", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ttl: durationpb.New(30 * time.Minute)}, connect.CodeInvalidArgument},
		{"ttl too long", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ttl: durationpb.New(169 * time.Hour)}, connect.CodeInvalidArgument},
		{"bad ref", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ref: "a b"}, connect.CodeInvalidArgument},
		{"bad service name", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "payments"}, connect.CodeInvalidArgument},
		{"unknown service", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/nope"}, connect.CodeNotFound},
		{"no repo", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/norepo"}, connect.CodeFailedPrecondition},
		{"repo in another org", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/elsewhere"}, connect.CodeFailedPrecondition},
		{"unknown ref", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Ref: "nope"}, connect.CodeNotFound},
	} {
		if _, err := f.create(t, tc.req); connect.CodeOf(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.code)
		}
	}
	bad := newEnvFixture(t, false, func(o *EnvOptions) {
		o.GitHub = &envtest.GitHub{Refs: map[string]string{"main": envSHA}, Files: map[string][]byte{envSHA + ":forge.yaml": []byte("run: [")}}
	})
	if _, err := bad.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("invalid forge.yaml: %v", err)
	}
	if _, err := f.client.CreateEnvironment(context.Background(), as("reader", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/payments"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader: %v", err)
	}
	if envs, _ := f.cluster.Envs(context.Background()); len(envs) != 0 {
		t.Errorf("a refused create left namespaces: %v", envs)
	}

	dry, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "dry", ValidateOnly: true})
	if err != nil || dry.GetOperation() != nil || dry.GetEnvironment().GetImage() != f.image || dry.GetEnvironment().GetNamespace() != "preview-dry" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if _, err := f.cluster.Env(context.Background(), "dry"); !errors.Is(err, errNoEnv) {
		t.Error("validate_only created a namespace")
	}
}

func TestCreateEnvironmentsOffAnswersFailedPrecondition(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Authenticator: StaticAuthenticator{"writer": {Subject: "ana"}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := forgev1connect.NewEnvironmentServiceClient(srv.Client(), srv.URL)
	_, err = c.CreateEnvironment(context.Background(), as("writer", &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", Service: "services/x"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "previews are not configured") {
		t.Errorf("previews off: %v", err)
	}
}

func TestCreateRefusesDuplicatesAndLimit(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, func(o *EnvOptions) { o.Max = 2 }) // no image: creates stay pending
	a, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate while pending: %v", err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "c"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("limit counts pending creates: %v", err)
	}
	f.ops.Cancel(a.GetOperation().GetName())
	f.wait(t, a.GetOperation())
	if _, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "c"}); err != nil {
		t.Errorf("after a cancel frees a slot: %v", err)
	}
	for _, op := range f.ops.List("", true) {
		f.ops.Cancel(op.GetName())
	}
}

func TestCreateEnvironmentRequestID(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, nil)
	req := &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", RequestId: "k1"}
	first, err := f.create(t, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a", RequestId: "k1"})
	if err != nil || again.GetOperation().GetName() != first.GetOperation().GetName() {
		t.Errorf("retry: %v %v", again, err)
	}
	f.ops.Cancel(first.GetOperation().GetName())
	f.wait(t, first.GetOperation())
}

func TestCreateWaitsForImageAndCI(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, nil)
	f.available(t, nil)
	f.gh.SetRuns(envSHA, []github.WorkflowRun{{Name: "ci", Status: "in_progress"}})
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	name := resp.GetOperation().GetName()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		op, _ := f.ops.Get(name)
		if n := len(op.GetEvents()); n > 1 && strings.Contains(op.GetEvents()[n-1].GetMessage(), "CI: in_progress") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no CI status event: %v", op.GetEvents())
		}
	}
	if _, err := f.cluster.Env(context.Background(), "a"); !errors.Is(err, errNoEnv) {
		t.Error("namespace created before the image exists")
	}
	f.images.Add(f.image)
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Errorf("after the image appeared: %v", op.GetError())
	}

	failing := newEnvFixture(t, true, nil)
	failing.gh.SetRuns(envSHA, []github.WorkflowRun{
		{Name: "ci", Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/showmino/payments/actions/runs/7"},
		{Name: "ci", Status: "completed", Conclusion: "success"}, // older run of the same workflow
	})
	resp, err = failing.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	op := failing.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != "ci_failed" || !strings.Contains(op.GetError().GetMessage(), "actions/runs/7") {
		t.Errorf("ci failure: %v", op.GetError())
	}

	slow := newEnvFixture(t, true, func(o *EnvOptions) { o.ImageTimeout = 30 * time.Millisecond })
	resp, err = slow.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if op := slow.wait(t, resp.GetOperation()); op.GetError().GetCode() != "image_timeout" {
		t.Errorf("image timeout: %v", op.GetError())
	}
	for _, fx := range []*envFixture{failing, slow} {
		if envs, _ := fx.cluster.Envs(context.Background()); len(envs) != 0 {
			t.Errorf("failure before the namespace left one: %v", envs)
		}
	}
}

func TestCreateNotReady(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, func(o *EnvOptions) { o.ReadyTimeout = 100 * time.Millisecond })
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// The pod cannot pull its image.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := f.cluster.Kube.AppsV1().Deployments("preview-a").Get(context.Background(), "payments", metav1.GetOptions{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no deployment")
		}
	}
	_, _ = f.cluster.Kube.CoreV1().Pods("preview-a").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	op := f.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != "not_ready" || !strings.Contains(op.GetError().GetMessage(), "ImagePullBackOff") {
		t.Errorf("not ready: %v", op.GetError())
	}
	ns, err := f.cluster.Env(context.Background(), "a")
	if err != nil || ns.Annotations[annState] != stateFailed || ns.Annotations[annStateMessage] == "" {
		t.Errorf("failed env kept for debugging: %v %v", ns, err)
	}
}

func TestCancelCreate(t *testing.T) {
	t.Parallel()
	// Cancelled while waiting for the image: nothing to remove.
	f := newEnvFixture(t, true, nil)
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	f.ops.Cancel(resp.GetOperation().GetName())
	if op := f.wait(t, resp.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("cancel while waiting: %v", op.GetError())
	}

	// Cancelled after the namespace exists (never ready): it is removed.
	g := newEnvFixture(t, false, nil)
	resp, err = g.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		op, _ := g.ops.Get(resp.GetOperation().GetName())
		if strings.Contains(eventSteps(op), "apply") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never applied")
		}
	}
	g.ops.Cancel(resp.GetOperation().GetName())
	op := g.wait(t, resp.GetOperation())
	if op.GetError().GetCode() != OpCodeCancelled || !strings.Contains(eventSteps(op), "cancel") {
		t.Errorf("cancel after namespace: %v %s", op.GetError(), eventSteps(op))
	}
	if gone, _ := g.cluster.EnvGone(context.Background(), "b"); !gone {
		t.Error("namespace left after cancel")
	}
}
