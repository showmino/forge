package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/showmino/forge/internal/run"
)

const launchToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testLauncher(kube *fake.Clientset) (*JobLauncher, run.Spec) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	l := &JobLauncher{
		Kube: kube, Namespace: "forge-runner", Image: "ghcr.io/showmino/forge-runner:abc1234", ServiceAccount: "forge-runner",
		APIURL:     "http://forge-api-runner.forge.svc:8081",
		PullSecret: func(context.Context) ([]byte, error) { return []byte(`{"auths":{}}`), nil },
		Poll:       time.Millisecond, StopTimeout: 100 * time.Millisecond,
		Now: func() time.Time { return now },
	}
	return l, run.Spec{Plan: "resourcePlans/0123456789ab", Mode: run.ModeApply, Service: "payments", Env: "staging",
		Token: launchToken, Deadline: now.Add(15 * time.Minute)}
}

func TestJobLauncherStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	l, spec := testLauncher(kube)
	if err := l.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	const name = "run-0123456789ab-apply"
	job, err := kube.BatchV1().Jobs("forge-runner").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 900 || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("job limits: backoff %d, deadline %d, ttl %d", *job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds, *job.Spec.TTLSecondsAfterFinished)
	}
	if l := job.Labels; l["app.kubernetes.io/managed-by"] != "forge" || l["forge.dev/plan"] != "0123456789ab" || l["forge.dev/mode"] != "apply" ||
		l["forge.dev/service"] != "payments" || l["forge.dev/deploy-env"] != "staging" {
		t.Errorf("job labels: %v", l)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "forge-runner" || pod.RestartPolicy != corev1.RestartPolicyNever || *pod.TerminationGracePeriodSeconds != 120 ||
		len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "forge-pull" {
		t.Errorf("pod: %+v", pod)
	}
	if sc := pod.SecurityContext; !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security: %+v", sc)
	}
	c := pod.Containers[0]
	if c.Image != "ghcr.io/showmino/forge-runner:abc1234" || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem ||
		len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container: %+v", c)
	}
	if got := c.Resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("memory limit %s", got)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["FORGE_RUNNER_API"] != "http://forge-api-runner.forge.svc:8081" || env["FORGE_RUN_TOKEN_FILE"] != "/var/run/forge/run/token" ||
		env["FORGE_RUN_DEADLINE"] != "2026-10-10T12:15:00Z" || len(env) != 3 {
		t.Errorf("env: %v", env)
	}
	// The token is in the Secret only: not in the Job's env, args or labels.
	if raw, err := json.Marshal(job); err != nil || strings.Contains(string(raw), launchToken) {
		t.Errorf("the Job holds the token (%v)", err)
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["token"] != "/var/run/forge/run" || mounts["tmp"] != "/tmp" || pod.Volumes[0].Secret.SecretName != name {
		t.Errorf("volumes: %v %+v", mounts, pod.Volumes)
	}
	secret, err := kube.CoreV1().Secrets("forge-runner").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["token"]) != launchToken || len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" || secret.OwnerReferences[0].Name != name {
		t.Errorf("token secret: data keys %d, owners %+v", len(secret.Data), secret.OwnerReferences)
	}
	pull, err := kube.CoreV1().Secrets("forge-runner").Get(ctx, "forge-pull", metav1.GetOptions{})
	if err != nil || pull.Type != corev1.SecretTypeDockerConfigJson || string(pull.Data[corev1.DockerConfigJsonKey]) != `{"auths":{}}` {
		t.Errorf("pull secret: %+v %v", pull, err)
	}

	// A second start for the same run is refused and changes nothing.
	again := spec
	again.Token = "another-token"
	if err := l.Start(ctx, again); err == nil || strings.Contains(err.Error(), "another-token") {
		t.Errorf("a second start: %v", err)
	}
	if s, _ := kube.CoreV1().Secrets("forge-runner").Get(ctx, name, metav1.GetOptions{}); string(s.Data["token"]) != launchToken {
		t.Error("the second start replaced the first run's token")
	}
	// A plan run gets its own Job; a short deadline still gives the Job a minute.
	short := spec
	short.Mode, short.Deadline = run.ModePlan, l.Now().Add(5*time.Second)
	if err := l.Start(ctx, short); err != nil {
		t.Fatal(err)
	}
	if j, _ := kube.BatchV1().Jobs("forge-runner").Get(ctx, "run-0123456789ab-plan", metav1.GetOptions{}); *j.Spec.ActiveDeadlineSeconds != 60 {
		t.Errorf("a short deadline: %d", *j.Spec.ActiveDeadlineSeconds)
	}
	for _, bad := range []run.Spec{{Plan: "plans/x", Mode: run.ModePlan}, {Plan: "resourcePlans/0123456789ab", Mode: "destroy"}, {}} {
		if err := l.Start(ctx, bad); err == nil {
			t.Errorf("start %+v: want an error", bad)
		}
	}
}

func TestJobLauncherStartLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, fail := range map[string]func(*fake.Clientset){
		"the Job cannot be created": func(k *fake.Clientset) {
			k.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("quota exceeded")
			})
		},
		"the Secret cannot be tied to the Job": func(k *fake.Clientset) {
			k.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("conflict")
			})
		},
	} {
		kube := fake.NewSimpleClientset()
		l, spec := testLauncher(kube)
		l.PullSecret = nil // so the only Secret is the token's
		fail(kube)
		err := l.Start(ctx, spec)
		if err == nil || strings.Contains(err.Error(), launchToken) {
			t.Errorf("%s: %v", name, err)
		}
		secrets, _ := kube.CoreV1().Secrets("forge-runner").List(ctx, metav1.ListOptions{})
		jobs, _ := kube.BatchV1().Jobs("forge-runner").List(ctx, metav1.ListOptions{})
		if len(secrets.Items) != 0 || len(jobs.Items) != 0 {
			t.Errorf("%s: %d secrets and %d jobs left behind", name, len(secrets.Items), len(jobs.Items))
		}
	}
	// No pull secret wanted: none copied, none referenced.
	kube := fake.NewSimpleClientset()
	l, spec := testLauncher(kube)
	l.PullSecret = nil
	if err := l.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	job, _ := kube.BatchV1().Jobs("forge-runner").Get(ctx, "run-0123456789ab-apply", metav1.GetOptions{})
	if _, err := kube.CoreV1().Secrets("forge-runner").Get(ctx, "forge-pull", metav1.GetOptions{}); !apierrors.IsNotFound(err) || len(job.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("an unwanted pull secret: %v %+v", err, job.Spec.Template.Spec.ImagePullSecrets)
	}
	// A pull secret that cannot be read fails the start before anything is made.
	kube = fake.NewSimpleClientset()
	l, spec = testLauncher(kube)
	l.PullSecret = func(context.Context) ([]byte, error) { return nil, errors.New("no such secret") }
	if err := l.Start(ctx, spec); err == nil || !strings.Contains(err.Error(), "copy the pull secret into forge-runner: no such secret") {
		t.Errorf("an unreadable pull secret: %v", err)
	}
	if jobs, _ := kube.BatchV1().Jobs("forge-runner").List(ctx, metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Error("a Job was made without its pull secret")
	}
}

func TestJobLauncherStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	l, spec := testLauncher(kube)
	if err := l.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	const name = "run-0123456789ab-apply"
	status := func() run.Status {
		t.Helper()
		st, err := l.Status(ctx, spec.Plan, spec.Mode)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status(); st.Done || st.Failed {
		t.Errorf("a running Job: %+v", st)
	}
	set := func(conds ...batchv1.JobCondition) {
		t.Helper()
		job, _ := kube.BatchV1().Jobs("forge-runner").Get(ctx, name, metav1.GetOptions{})
		job.Status.Conditions = conds
		if _, err := kube.BatchV1().Jobs("forge-runner").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A condition that is not True does not count.
	set(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionFalse})
	if st := status(); st.Done {
		t.Errorf("a condition that is False: %+v", st)
	}
	set(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	if st := status(); !st.Done || st.Failed || st.Log != "" {
		t.Errorf("a complete Job: %+v", st)
	}
	// A failed Job reports why, with the end of the pod's output.
	if _, err := kube.CoreV1().Pods("forge-runner").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-abcde", Labels: map[string]string{"job-name": name}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	set(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"})
	if st := status(); !st.Done || !st.Failed || !strings.HasPrefix(st.Log, "DeadlineExceeded: Job was active longer than specified deadline\n") || !strings.Contains(st.Log, "fake logs") {
		t.Errorf("a failed Job: %+v", st)
	}
	// A Job that is gone is a failed run, not an error to retry forever.
	if err := kube.BatchV1().Jobs("forge-runner").Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := status(); !st.Done || !st.Failed || st.Log != "the runner's Job "+name+" is gone" {
		t.Errorf("a missing Job: %+v", st)
	}
	if _, err := l.Status(ctx, "x", "plan"); err == nil {
		t.Error("a bad plan name: want an error")
	}
}

func TestJobLauncherStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	l, spec := testLauncher(kube)
	if err := l.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var policy metav1.DeletionPropagation
	kube.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.DeleteAction).GetDeleteOptions().PropagationPolicy; p != nil {
			policy = *p
		}
		return false, nil, nil // let the tracker delete it
	})
	if err := l.Stop(ctx, spec.Plan, spec.Mode); err != nil {
		t.Fatal(err)
	}
	if policy != metav1.DeletePropagationForeground {
		t.Errorf("propagation %q, want Foreground (wait for the pod)", policy)
	}
	const name = "run-0123456789ab-apply"
	if _, err := kube.BatchV1().Jobs("forge-runner").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Job after Stop: %v", err)
	}
	if _, err := kube.CoreV1().Secrets("forge-runner").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the token Secret after Stop: %v", err)
	}
	// Stopping a runner that is gone is not an error.
	if err := l.Stop(ctx, spec.Plan, spec.Mode); err != nil {
		t.Errorf("a second stop: %v", err)
	}
	if err := l.Stop(ctx, "x", "plan"); err == nil {
		t.Error("a bad plan name: want an error")
	}

	// A Job that does not go away in time is reported, and Stop does not hang.
	kube = fake.NewSimpleClientset()
	l, spec = testLauncher(kube)
	if err := l.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	kube.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // accepted, but the Job stays (its pod is still stopping)
	})
	start := time.Now()
	err := l.Stop(ctx, spec.Plan, spec.Mode)
	if err == nil || !strings.Contains(err.Error(), "was still stopping after 100ms") || time.Since(start) > 5*time.Second {
		t.Errorf("a Job that stays: %v after %s", err, time.Since(start))
	}
	// A cancelled context ends the wait.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.Stop(cctx, spec.Plan, spec.Mode); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled stop: %v", err)
	}
}
