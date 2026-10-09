package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// readyEnv creates environment id and waits until it is ready.
func (f *envFixture) readyEnv(t *testing.T, id string) {
	t.Helper()
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: id})
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
}

func TestGetListExtend(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	f.readyEnv(t, "b")
	ctx := context.Background()

	got, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	env := got.Msg.GetEnvironment()
	if env.GetState() != forgev1.Environment_STATE_READY || env.GetService() != "services/payments" || env.GetCommit() != envSHA ||
		env.GetUrl() != "http://a.preview.example" || env.GetNamespace() != "preview-a" || env.GetCreator() != "ana" ||
		env.GetOperation() == "" || env.GetExpireTime() == nil {
		t.Errorf("get: %v", env)
	}
	for _, tc := range []struct {
		name string
		code connect.Code
	}{{"environments/nope", connect.CodeNotFound}, {"a", connect.CodeInvalidArgument}, {"environments/A", connect.CodeInvalidArgument}} {
		if _, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: tc.name})); connect.CodeOf(err) != tc.code {
			t.Errorf("get %q: %v", tc.name, err)
		}
	}

	list, err := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{}))
	if err != nil || len(list.Msg.GetEnvironments()) != 2 || list.Msg.GetEnvironments()[0].GetName() != "environments/a" {
		t.Errorf("list: %v %v", list, err)
	}
	if l, _ := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{Service: "services/other"})); len(l.Msg.GetEnvironments()) != 0 {
		t.Errorf("list by service: %v", l.Msg)
	}

	ext, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: durationpb.New(72 * time.Hour)}))
	if err != nil || !ext.Msg.GetEnvironment().GetExpireTime().AsTime().Equal(f.clock.now().Add(72*time.Hour).Truncate(time.Second)) {
		t.Errorf("extend: %v %v", ext, err)
	}
	for _, ttl := range []*durationpb.Duration{nil, durationpb.New(10 * time.Minute), durationpb.New(200 * time.Hour)} {
		if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: ttl})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("extend ttl %v: %v", ttl, err)
		}
	}
	if _, err := f.client.ExtendEnvironment(ctx, as("reader", &forgev1.ExtendEnvironmentRequest{Name: "environments/a", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader extend: %v", err)
	}
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annState: stateDestroying})
	if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/b", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("extend destroying: %v", err)
	}
}

func TestGetReportsLiveProblem(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	var crashed atomic.Bool
	f.available(t, func(string) bool { return crashed.Load() })
	f.readyEnv(t, "a")
	ctx := context.Background()
	crashed.Store(true) // the fake controller stops making it available
	d, _ := f.cluster.Kube.AppsV1().Deployments("preview-a").Get(ctx, "payments", metav1.GetOptions{})
	d.Status.AvailableReplicas, d.Status.ReadyReplicas = 0, 0
	_, _ = f.cluster.Kube.AppsV1().Deployments("preview-a").UpdateStatus(ctx, d, metav1.UpdateOptions{})
	_, _ = f.cluster.Kube.CoreV1().Pods("preview-a").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-1", Labels: map[string]string{"app": "payments"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}, metav1.CreateOptions{})
	got, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	if env := got.Msg.GetEnvironment(); env.GetState() != forgev1.Environment_STATE_READY || env.GetStateMessage() != "CrashLoopBackOff" {
		t.Errorf("live problem: %v", env)
	}
}

func TestDestroyEnvironment(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	ctx := context.Background()

	dry, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetEnvironment().GetName() != "environments/a" {
		t.Errorf("validate_only: %v %v", dry, err)
	}
	if _, err := f.cluster.Env(ctx, "a"); err != nil {
		t.Error("validate_only destroyed it")
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("reader", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader destroy: %v", err)
	}
	resp, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	op := f.wait(t, resp.Msg.GetOperation())
	if op.GetError() != nil || op.GetKind() != "destroy_environment" || op.GetTarget() != "environments/a" {
		t.Errorf("destroy op: %v", op)
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("namespace still exists")
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("destroy again: %v", err)
	}
}

func TestDestroyCancelsRunningCreate(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil) // never available: the create waits in "ready"
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := f.cluster.Env(ctx, "a"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no namespace")
		}
	}
	d, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := f.wait(t, d.Msg.GetOperation()); op.GetError() != nil {
		t.Errorf("destroy: %v", op.GetError())
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("create: %v", op.GetError())
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("namespace left")
	}
}

func TestCancelledDestroyReportsCancelled(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	// Once stuck, the namespace never goes away: the destroy waits until
	// cancelled. (Reactors must be added before the clientset is shared.)
	var stuck atomic.Bool
	f.cluster.Kube.(*fake.Clientset).PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return stuck.Load(), nil, nil
	})
	f.available(t, nil)
	f.readyEnv(t, "a")
	stuck.Store(true)
	d, err := f.client.DestroyEnvironment(context.Background(), as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	f.ops.Cancel(d.Msg.GetOperation().GetName())
	if op := f.wait(t, d.Msg.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("cancelled destroy: %v", op.GetError())
	}
}

func TestDestroyCancelsCreateWaitingForImage(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, true, nil) // no image: the create waits before making the namespace
	resp, err := f.create(t, &forgev1.CreateEnvironmentRequest{EnvironmentId: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dry, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetEnvironment().GetState() != forgev1.Environment_STATE_DESTROYING {
		t.Fatalf("validate_only: %v %v", dry, err)
	}
	if op, _ := f.ops.Get(resp.GetOperation().GetName()); op.GetDone() {
		t.Fatalf("validate_only stopped the create: %v", op)
	}
	d, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"}))
	if err != nil {
		t.Fatal(err)
	}
	env := d.Msg.GetEnvironment()
	if env.GetName() != "environments/a" || env.GetNamespace() != "preview-a" || env.GetUrl() != "http://a.preview.example" ||
		env.GetState() != forgev1.Environment_STATE_DESTROYING {
		t.Errorf("environment: %v", env)
	}
	if op := f.wait(t, d.Msg.GetOperation()); op.GetError() != nil {
		t.Errorf("destroy: %v", op.GetError())
	}
	if op := f.wait(t, resp.GetOperation()); op.GetError().GetCode() != OpCodeCancelled {
		t.Errorf("create: %v", op.GetError())
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("namespace exists")
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/a"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("destroy again: %v", err)
	}
}

func TestGuardOnRPCs(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	ctx := context.Background()
	_, _ = f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-x"}}, metav1.CreateOptions{})
	if _, err := f.client.GetEnvironment(ctx, as("reader", &forgev1.GetEnvironmentRequest{Name: "environments/x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("get: %v", err)
	}
	if _, err := f.client.ExtendEnvironment(ctx, as("writer", &forgev1.ExtendEnvironmentRequest{Name: "environments/x", Ttl: durationpb.New(time.Hour)})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("extend: %v", err)
	}
	if _, err := f.client.DestroyEnvironment(ctx, as("writer", &forgev1.DestroyEnvironmentRequest{Name: "environments/x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("destroy: %v", err)
	}
	if l, _ := f.client.ListEnvironments(ctx, as("reader", &forgev1.ListEnvironmentsRequest{})); len(l.Msg.GetEnvironments()) != 0 {
		t.Errorf("list: %v", l.Msg)
	}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "preview-x", metav1.GetOptions{}); err != nil {
		t.Errorf("preview-x touched: %v", err)
	}
}

func TestReaper(t *testing.T) {
	t.Parallel()
	f := newEnvFixture(t, false, nil)
	f.available(t, nil)
	f.readyEnv(t, "a")
	f.readyEnv(t, "b")
	ctx := context.Background()
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annExpire: f.clock.now().Add(72 * time.Hour).UTC().Format(time.RFC3339)})

	if err := f.envs.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if ops := f.ops.List("", true); len(ops) != 0 {
		t.Errorf("reaped before expiry: %v", ops)
	}
	f.clock.add(25 * time.Hour)
	if err := f.envs.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.envs.Reap(ctx); err != nil { // a second pass must not start a second destroy
		t.Fatal(err)
	}
	destroys := 0
	for _, op := range f.ops.List("environments/a", false) {
		if op.GetKind() == "destroy_environment" {
			destroys++
			if op.GetCreator() != "forge-reaper" {
				t.Errorf("creator %q", op.GetCreator())
			}
			wait(t, f.ops, op.GetName())
		}
	}
	// A pass that listed a before the destroy finished must not start another.
	if op := f.envs.startDestroy(ctx, "a", "forge-reaper"); op != nil {
		t.Errorf("destroy started for a gone environment: %v", op)
	}
	destroys = 0
	for _, op := range f.ops.List("environments/a", false) {
		if op.GetKind() == "destroy_environment" {
			destroys++
		}
	}
	if destroys != 1 {
		t.Errorf("%d destroys for a, want 1", destroys)
	}
	if gone, _ := f.cluster.EnvGone(ctx, "a"); !gone {
		t.Error("expired a not removed")
	}
	if _, err := f.cluster.Env(ctx, "b"); err != nil {
		t.Error("b (72h) removed early")
	}

	// A malformed expiry counts as creation time + 168h.
	_ = f.cluster.Annotate(ctx, "b", map[string]string{annExpire: "soon"})
	ns, _ := f.cluster.Env(ctx, "b")
	if !expiry(ns).Equal(ns.CreationTimestamp.Add(168 * time.Hour)) {
		t.Errorf("malformed expiry: %v", expiry(ns))
	}
}
