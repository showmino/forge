package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/showmino/forge/internal/run"
)

// Where a runner finds its token, and what the token's Secret calls it.
const (
	runTokenDir = "/var/run/forge/run" //nolint:gosec // a path, not a credential
	runTokenKey = "token"
)

// JobLauncher is the run.Launcher of a cluster: every run is a Kubernetes
// Job in Namespace, with the run's token in a Secret of the same name that
// only that Job mounts. forge-api needs, in Namespace only: create, get and
// delete on jobs; create, get, update and delete on secrets; list on pods;
// get on pods/log.
type JobLauncher struct {
	Kube kubernetes.Interface
	// Namespace holds the Jobs (forge-runner).
	Namespace string
	// Image is the runner image.
	Image string
	// ServiceAccount is what the runner acts as on the cluster.
	ServiceAccount string
	// APIURL is forge-api's runner listener as the Job reaches it, e.g.
	// http://forge-api-runner.forge.svc:8081.
	APIURL string
	// PullSecret, when set, returns the dockerconfigjson the runner image is
	// pulled with; it is copied into Namespace as "forge-pull".
	PullSecret func(ctx context.Context) ([]byte, error)
	// StopTimeout is how long Stop waits for the runner to exit (default 3m).
	StopTimeout time.Duration
	// Poll is how often Stop looks (default 1s).
	Poll time.Duration
	// StartGrace is how long a runner's pod may take to start before a pod
	// that cannot start counts as a failed run (default 2m).
	StartGrace time.Duration
	Now        func() time.Time
}

// stopGrace is how long a runner's pod gets after SIGTERM: the runner gives
// OpenTofu 45s to save its state, then reports.
const stopGrace = 120

// runName is the name of a run's Job and Secret: "run-<plan id>-<mode>".
func runName(plan, mode string) (string, error) {
	id := strings.TrimPrefix(plan, planNamePrefix)
	if !planIDRE.MatchString(id) || (mode != run.ModePlan && mode != run.ModeApply) {
		return "", fmt.Errorf("no runner name for plan %q and mode %q", plan, mode)
	}
	return "run-" + id + "-" + mode, nil
}

func (l *JobLauncher) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Start implements run.Launcher. When it fails it leaves no Job and no
// Secret behind.
func (l *JobLauncher) Start(ctx context.Context, s run.Spec) error {
	name, err := runName(s.Plan, s.Mode)
	if err != nil {
		return err
	}
	labels := map[string]string{
		labelManagedBy: "forge", labelService: s.Service, labelDeployEnv: s.Env,
		"forge.dev/plan": strings.TrimPrefix(s.Plan, planNamePrefix), "forge.dev/mode": s.Mode,
	}
	if l.PullSecret != nil {
		if err := l.copyPullSecret(ctx); err != nil {
			return fmt.Errorf("copy the pull secret into %s: %w", l.Namespace, err)
		}
	}
	secrets := l.Kube.CoreV1().Secrets(l.Namespace)
	secret, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Data:       map[string][]byte{runTokenKey: []byte(s.Token)},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create the run's token Secret %s: %w", name, err)
	}
	// From here on a failure removes what was made, with a context of its own.
	undo := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = l.Kube.BatchV1().Jobs(l.Namespace).Delete(c, name, deleteNow())
		_ = secrets.Delete(c, name, metav1.DeleteOptions{})
	}
	job, err := l.Kube.BatchV1().Jobs(l.Namespace).Create(ctx, l.job(name, labels, s), metav1.CreateOptions{})
	if err != nil {
		undo()
		return fmt.Errorf("create the runner Job %s: %w", name, err)
	}
	// The Secret belongs to the Job: it goes when the Job is cleaned up.
	secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}}
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		undo()
		return fmt.Errorf("tie the token Secret %s to its Job: %w", name, err)
	}
	return nil
}

func deleteNow() metav1.DeleteOptions {
	background := metav1.DeletePropagationBackground
	return metav1.DeleteOptions{PropagationPolicy: &background}
}

// copyPullSecret writes the pull secret into the runner namespace.
func (l *JobLauncher) copyPullSecret(ctx context.Context) error {
	data, err := l.PullSecret(ctx)
	if err != nil {
		return err
	}
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName, Labels: map[string]string{labelManagedBy: "forge"}},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: data},
	}
	secrets := l.Kube.CoreV1().Secrets(l.Namespace)
	_, err = secrets.Create(ctx, want, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, want, metav1.UpdateOptions{})
	}
	return err
}

// job is the runner Job of one run.
func (l *JobLauncher) job(name string, labels map[string]string, s run.Spec) *batchv1.Job {
	var (
		noRetry     int32 = 0
		keep        int32 = 3600 // seconds a finished Job stays, for its logs
		grace       int64 = stopGrace
		user        int64 = 65532
		yes, no           = true, false
		tokenMode   int32 = 0o440
		tmpLimit          = resource.MustParse("512Mi")
		activeLimit       = int64(s.Deadline.Sub(l.now()).Seconds())
	)
	if activeLimit < 60 {
		activeLimit = 60
	}
	pod := corev1.PodSpec{
		ServiceAccountName:            l.ServiceAccount,
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: &grace,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user, FSGroup: &user,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Volumes: []corev1.Volume{
			{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: &tokenMode}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}},
		},
		Containers: []corev1.Container{{
			Name:  "runner",
			Image: l.Image,
			Env: []corev1.EnvVar{
				{Name: "FORGE_RUNNER_API", Value: l.APIURL},
				{Name: "FORGE_RUN_TOKEN_FILE", Value: runTokenDir + "/" + runTokenKey},
				{Name: "FORGE_RUN_DEADLINE", Value: s.Deadline.UTC().Format(time.RFC3339)},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "token", MountPath: runTokenDir, ReadOnly: true},
				{Name: "tmp", MountPath: "/tmp"},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		}},
	}
	if l.PullSecret != nil {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName}}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &noRetry,
			ActiveDeadlineSeconds:   &activeLimit,
			TTLSecondsAfterFinished: &keep,
			Template:                corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
		},
	}
}

// Status implements run.Launcher.
func (l *JobLauncher) Status(ctx context.Context, plan, mode string) (run.Status, error) {
	name, err := runName(plan, mode)
	if err != nil {
		return run.Status{}, err
	}
	job, err := l.Kube.BatchV1().Jobs(l.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return run.Status{Done: true, Failed: true, Log: "the runner's Job " + name + " is gone"}, nil
	}
	if err != nil {
		return run.Status{}, err
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return run.Status{Done: true}, nil
		case batchv1.JobFailed:
			log := strings.TrimSpace(c.Reason + ": " + c.Message)
			if tail := l.logTail(ctx, name); tail != "" {
				log += "\n" + tail
			}
			return run.Status{Done: true, Failed: true, Log: log}, nil
		}
	}
	if why := l.stuck(ctx, job); why != "" {
		return run.Status{Done: true, Failed: true, Stuck: true, Log: why}, nil
	}
	return run.Status{}, nil
}

// cannotStart are the reasons a container waits for that do not pass on
// their own: the image, or something the pod mounts, is not there.
var cannotStart = map[string]bool{
	"ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
}

// stuck says why job's runner will not start, or "" while it still may. A
// Job is stuck when, StartGrace after it was made, it has no pod (a quota
// or an admission rule refused it), its pod cannot be scheduled, or the
// pod's container cannot be started. Without this a run would wait for its
// whole timeout on a pod that never runs.
func (l *JobLauncher) stuck(ctx context.Context, job *batchv1.Job) string {
	grace := l.StartGrace
	if grace <= 0 {
		grace = 2 * time.Minute
	}
	made := job.CreationTimestamp.Time
	if made.IsZero() || l.now().Sub(made) < grace {
		return ""
	}
	pods, err := l.Kube.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job.Name})
	if err != nil {
		return ""
	}
	if len(pods.Items) == 0 {
		return fmt.Sprintf("no pod was made for the runner's Job %s within %s (a quota or an admission rule may refuse it; see the Job's events)", job.Name, grace)
	}
	why := ""
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodPending {
			return "" // one started, or ended: the Job's own conditions tell the rest
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				why = "the runner's pod cannot be scheduled: " + c.Message
			}
		}
		for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if w := cs.State.Waiting; w != nil && cannotStart[w.Reason] {
				why = strings.TrimSpace("the runner's pod cannot start: " + w.Reason + ": " + w.Message)
			}
		}
	}
	return why
}

// logTail is the end of the runner pod's output, or "" when it cannot be read.
func (l *JobLauncher) logTail(ctx context.Context, name string) string {
	pods, err := l.Kube.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	var lines, limit int64 = 20, 4 << 10
	data, err := l.Kube.CoreV1().Pods(l.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &lines, LimitBytes: &limit}).DoRaw(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Stop implements run.Launcher: it deletes the Job and waits until the Job
// and its pod are gone. The pod gets stopGrace seconds, in which the runner
// stops OpenTofu and reports.
func (l *JobLauncher) Stop(ctx context.Context, plan, mode string) error {
	name, err := runName(plan, mode)
	if err != nil {
		return err
	}
	timeout, poll := l.StopTimeout, l.Poll
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	if poll <= 0 {
		poll = time.Second
	}
	jobs := l.Kube.BatchV1().Jobs(l.Namespace)
	// Foreground: the Job object stays until its pod is gone.
	foreground := metav1.DeletePropagationForeground
	if err := jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the runner Job %s: %w", name, err)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		_, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("the runner Job %s was still stopping after %s", name, timeout)
		case <-time.After(poll):
		}
	}
	if err := l.Kube.CoreV1().Secrets(l.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return errors.Join(fmt.Errorf("delete the run's token Secret %s", name), err)
	}
	return nil
}
