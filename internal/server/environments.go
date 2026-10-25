package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

const (
	envNamePrefix  = "environments/"
	kindCreateEnv  = "create_environment"
	kindDestroyEnv = "destroy_environment"
	defaultTTL     = 24 * time.Hour
	minTTL         = time.Hour
	maxTTL         = 168 * time.Hour
)

var (
	envIDRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	refRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
)

// EnvGitHub is what environments need from GitHub (*github.Client as the App).
type EnvGitHub interface {
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	RunsFor(ctx context.Context, owner, repo, sha string) ([]github.WorkflowRun, error)
}

// ImageChecker reports whether an image exists in its registry.
type ImageChecker interface {
	Exists(ctx context.Context, image string) (bool, error)
}

// EnvOptions configure preview environments.
type EnvOptions struct {
	Cluster    *Cluster
	GitHub     EnvGitHub
	Images     ImageChecker
	Catalog    *Catalog
	Operations *Operations
	// Org is the GitHub org services' repos must be in.
	Org string
	// Domain: environment hosts are <environment>.<Domain>.
	Domain string
	// Registry is where the services' CI pushes images, as a host and an
	// optional path ("acr.example.io" or "registry.example/team"); images
	// are <Registry>/<owner>/<service>:<commit>. "" is ghcr.io.
	Registry string
	// Max is how many environments may exist at once (default 10).
	Max int
	// Step timeouts: defaults 10m, 5m and 3m.
	ImageTimeout, ReadyTimeout, DestroyTimeout time.Duration
	// How often to look for the image (default 10s) and at the cluster (default 2s).
	ImagePoll, Poll time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Environments serves EnvironmentService. A nil *Environments answers every
// call with FAILED_PRECONDITION: previews are off.
type Environments struct {
	o EnvOptions

	mu sync.Mutex
	// pending maps environments whose create has not made the namespace yet
	// to their service's id.
	pending map[string]string
	// requests maps a create's request_id to its operation.
	requests map[string]string
}

// NewEnvironments returns the environment service.
func NewEnvironments(o EnvOptions) *Environments {
	if o.Max == 0 {
		o.Max = 10
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.ImageTimeout, 10 * time.Minute}, {&o.ReadyTimeout, 5 * time.Minute}, {&o.DestroyTimeout, 3 * time.Minute}, {&o.ImagePoll, 10 * time.Second}, {&o.Poll, 2 * time.Second}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Environments{o: o, pending: map[string]string{}, requests: map[string]string{}}
}

func (e *Environments) ready() error {
	if e == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("previews are not configured on this Forge API"))
	}
	return nil
}

// url is where environment id is reached.
func (e *Environments) url(id string) string { return "http://" + id + "." + e.o.Domain }

func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// parseEnvName turns "environments/x" into "x".
func parseEnvName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, envNamePrefix)
	if !ok || !envIDRE.MatchString(id) {
		return "", invalidArg("%q is not an environment name (environments/<name>, a lowercase DNS label of at most 40 characters)", name)
	}
	return id, nil
}

// ttlOf checks a requested lifetime; nil means the default.
func ttlOf(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return defaultTTL, nil
	}
	t := d.AsDuration()
	if t < minTTL || t > maxTTL {
		return 0, invalidArg("ttl %s must be between 1h and 168h", t)
	}
	return t, nil
}

// envErr turns a cluster error into an RPC error.
func envErr(id string, err error) error {
	if errors.Is(err, errNoEnv) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s not found", envNamePrefix, id))
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("kubernetes: %w", err))
}

// repoOf returns owner and repo of a service's GitHub repo in org.
func repoOf(repoURL, org string) (string, string, bool) {
	rest, ok := strings.CutPrefix(repoURL, "https://github.com/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(rest, "/"), ".git"), "/")
	if !ok || !strings.EqualFold(owner, org) || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

var states = map[string]forgev1.Environment_State{
	stateCreating:   forgev1.Environment_STATE_CREATING,
	stateReady:      forgev1.Environment_STATE_READY,
	stateFailed:     forgev1.Environment_STATE_FAILED,
	stateDestroying: forgev1.Environment_STATE_DESTROYING,
}

// envFromNamespace reads an environment from its namespace.
func (e *Environments) envFromNamespace(ns *corev1.Namespace) *forgev1.Environment {
	id := ns.Labels[labelEnvironment]
	a := ns.Annotations
	env := &forgev1.Environment{
		Name:         envNamePrefix + id,
		Service:      servicePrefix + ns.Labels[labelService],
		Ref:          a[annRef],
		Commit:       a[annCommit],
		Image:        a[annImage],
		Url:          e.url(id),
		State:        states[a[annState]],
		StateMessage: a[annStateMessage],
		Namespace:    ns.Name,
		Creator:      a[annCreator],
		CreateTime:   timestamppb.New(ns.CreationTimestamp.Time),
	}
	if t, err := time.Parse(time.RFC3339, a[annExpire]); err == nil {
		env.ExpireTime = timestamppb.New(t)
	}
	if ops := e.o.Operations.List(env.Name, false); len(ops) > 0 {
		env.Operation = ops[0].GetName()
	}
	return env
}

// countEnvs is how many environments exist or are being created.
func (e *Environments) countEnvs(nss []corev1.Namespace) int {
	n := len(e.pending)
	for i := range nss {
		if _, pending := e.pending[nss[i].Labels[labelEnvironment]]; !pending {
			n++
		}
	}
	return n
}

func (e *Environments) GetEnvironment(ctx context.Context, req *connect.Request[forgev1.GetEnvironmentRequest]) (*connect.Response[forgev1.GetEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	ns, err := e.o.Cluster.Env(ctx, id)
	if err != nil {
		return nil, envErr(id, err)
	}
	env := e.envFromNamespace(ns)
	if env.GetState() == forgev1.Environment_STATE_READY {
		if ready, problem, err := e.o.Cluster.Status(ctx, id, ns.Labels[labelService]); err == nil && !ready {
			env.StateMessage = problem
		}
	}
	return connect.NewResponse(&forgev1.GetEnvironmentResponse{Environment: env}), nil
}

func (e *Environments) ListEnvironments(ctx context.Context, req *connect.Request[forgev1.ListEnvironmentsRequest]) (*connect.Response[forgev1.ListEnvironmentsResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	nss, err := e.o.Cluster.Envs(ctx)
	if err != nil {
		return nil, envErr("", err)
	}
	var out []*forgev1.Environment
	for i := range nss {
		env := e.envFromNamespace(&nss[i])
		if s := req.Msg.GetService(); s != "" && env.GetService() != s {
			continue
		}
		out = append(out, env)
	}
	slices.SortFunc(out, func(a, b *forgev1.Environment) int { return strings.Compare(a.GetName(), b.GetName()) })
	return connect.NewResponse(&forgev1.ListEnvironmentsResponse{Environments: out}), nil
}

func (e *Environments) ExtendEnvironment(ctx context.Context, req *connect.Request[forgev1.ExtendEnvironmentRequest]) (*connect.Response[forgev1.ExtendEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, require(ctx, PermServicesWrite)
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetTtl() == nil {
		return nil, invalidArg("ttl is required")
	}
	ttl, err := ttlOf(req.Msg.GetTtl())
	if err != nil {
		return nil, err
	}
	ns, err := e.o.Cluster.Env(ctx, id)
	if err != nil {
		return nil, envErr(id, err)
	}
	noteAuditService(ctx, servicePrefix+ns.Labels[labelService])
	if err := authorizePreview(ctx, ActEnvironmentExtend, e.envFromNamespace(ns)); err != nil {
		return nil, err
	}
	if ns.Annotations[annState] == stateDestroying {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s is being destroyed", envNamePrefix, id))
	}
	if err := e.o.Cluster.Annotate(ctx, id, map[string]string{annExpire: e.o.Now().Add(ttl).UTC().Format(time.RFC3339)}); err != nil {
		return nil, envErr(id, err)
	}
	if ns, err = e.o.Cluster.Env(ctx, id); err != nil {
		return nil, envErr(id, err)
	}
	return connect.NewResponse(&forgev1.ExtendEnvironmentResponse{Environment: e.envFromNamespace(ns)}), nil
}

func (e *Environments) DestroyEnvironment(ctx context.Context, req *connect.Request[forgev1.DestroyEnvironmentRequest]) (*connect.Response[forgev1.DestroyEnvironmentResponse], error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, require(ctx, PermServicesWrite)
	}
	id, err := parseEnvName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	var env *forgev1.Environment
	ns, err := e.o.Cluster.Env(ctx, id)
	switch {
	case err == nil:
		env = e.envFromNamespace(ns)
	case errors.Is(err, errNoEnv) && e.creating(id):
		// A create still waiting for its image: no namespace yet.
		env = &forgev1.Environment{
			Name: envNamePrefix + id, Namespace: envNamespace(id), Url: e.url(id),
			State: forgev1.Environment_STATE_DESTROYING,
		}
	default:
		return nil, envErr(id, err)
	}
	noteAuditService(ctx, env.GetService())
	if err := authorizePreview(ctx, ActEnvironmentDestroy, env); err != nil {
		return nil, err
	}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.DestroyEnvironmentResponse{Environment: env}), nil
	}
	creator := ""
	if _, ok := PrincipalFrom(ctx); ok {
		creator = actorOf(ctx)
	}
	op := e.startDestroy(ctx, id, creator)
	if op == nil {
		return nil, envErr(id, errNoEnv)
	}
	return connect.NewResponse(&forgev1.DestroyEnvironmentResponse{Operation: op, Environment: env}), nil
}

// authorizePreview checks that the caller may do action on a preview: the
// person who made it may, and so may the team of its service.
func authorizePreview(ctx context.Context, action string, env *forgev1.Environment) error {
	if _, ok := PrincipalFrom(ctx); ok && env.GetCreator() != "" && env.GetCreator() == actorOf(ctx) {
		return nil
	}
	return authorize(ctx, action, env.GetService())
}
