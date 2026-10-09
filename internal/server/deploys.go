package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

const (
	deployNamePrefix = "deployments/"
	kindDeploy       = "deploy"
	kindRollback     = "rollback"
	kindPromote      = "promote"
	opDeploy         = "deploy"
)

// deployEnvs are the deploy environments, in promotion order.
var deployEnvs = []string{"staging", "production"}

// DeployRepo is Forge's GitOps repo (*github.RepoFiles on GitHub).
type DeployRepo interface {
	Read(ctx context.Context, path, ref string) ([]byte, error)
	Head(ctx context.Context) (string, string, error)
	CommitFile(ctx context.Context, parent, tree, path string, content []byte, message string) (string, error)
	History(ctx context.Context, path string, limit int) ([]github.CommitInfo, error)
}

// DeployOptions configure deploys.
type DeployOptions struct {
	Cluster    *Cluster
	GitHub     EnvGitHub
	Images     ImageChecker
	Repo       DeployRepo
	Catalog    *Catalog
	Operations *Operations
	// Org is the GitHub org services' repos must be in.
	Org string
	// Domain: hosts are <service>.<env>.<Domain>.
	Domain string
	// ArgoNamespace holds the Applications (default argocd).
	ArgoNamespace string
	// Defaults: 10m, 10m, 2m.
	ImageTimeout, SyncTimeout, NamespaceTimeout time.Duration
	// Defaults: 10s, 2s.
	ImagePoll, Poll time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Deploys serves DeployService. A nil *Deploys answers FAILED_PRECONDITION.
type Deploys struct {
	o DeployOptions

	mu sync.Mutex
	// running maps "<service>/<env>" to its running mutation.
	running map[string]string
	// requests maps StartDeploy request_ids to their operation.
	requests map[string]string
}

// NewDeploys returns the deploy service.
func NewDeploys(o DeployOptions) *Deploys {
	if o.ArgoNamespace == "" {
		o.ArgoNamespace = "argocd"
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.ImageTimeout, 10 * time.Minute}, {&o.SyncTimeout, 10 * time.Minute}, {&o.NamespaceTimeout, 2 * time.Minute}, {&o.ImagePoll, 10 * time.Second}, {&o.Poll, 2 * time.Second}} {
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
	return &Deploys{o: o, running: map[string]string{}, requests: map[string]string{}}
}

func (d *Deploys) ready() error {
	if d == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("deploys are not configured on this Forge API"))
	}
	return nil
}

func valuesPath(svc, env string) string { return "services/" + svc + "/" + env + "/values.yaml" }

func (d *Deploys) host(svc, env string) string { return svc + "." + env + "." + d.o.Domain }

func validEnv(env string) error {
	if !slices.Contains(deployEnvs, env) {
		return invalidArg("environment %q must be staging or production", env)
	}
	return nil
}

// parseDeployName turns "deployments/<svc>/<env>" into its parts.
func parseDeployName(name string) (string, string, error) {
	rest, ok := strings.CutPrefix(name, deployNamePrefix)
	svc, env, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || svc == "" {
		return "", "", invalidArg("%q is not a deployment name (deployments/<service>/<env>)", name)
	}
	return svc, env, validEnv(env)
}

// deployRecord is who changed an environment to what, and why.
type deployRecord struct {
	Service   string `yaml:"service"`
	Env       string `yaml:"env"`
	Ref       string `yaml:"ref"`
	Commit    string `yaml:"commit"`
	Actor     string `yaml:"actor"`
	Operation string `yaml:"operation"`
	Kind      string `yaml:"kind"`
	Time      string `yaml:"time"`
	// Image and DeployCommit come from the commit, not the values file.
	Image        string `yaml:"-"`
	DeployCommit string `yaml:"-"`
}

// deployValues is services/<svc>/<env>/values.yaml.
type deployValues struct {
	Name     string       `yaml:"name"`
	Env      string       `yaml:"env"`
	Image    string       `yaml:"image"`
	Port     int32        `yaml:"port"`
	Health   string       `yaml:"health"`
	Replicas int          `yaml:"replicas"`
	Host     string       `yaml:"host"`
	Forge    deployRecord `yaml:"forge"`
}

func (v deployValues) marshal() []byte {
	data, err := yaml.Marshal(v)
	if err != nil {
		panic(err) // a plain struct always marshals
	}
	return append([]byte("# Written by Forge; change it with 'forge deploy'.\n"), data...)
}

func parseValues(data []byte) (deployValues, error) {
	var v deployValues
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("deploy values: %w", err)
	}
	return v, nil
}

var trailerKeys = []string{"Kind", "Service", "Env", "Image", "Ref", "Commit", "Actor", "Operation"}

// commitMessage is the deploy repo commit message for r.
func commitMessage(r deployRecord) string {
	svc := strings.TrimPrefix(r.Service, servicePrefix)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s to %s: %s (%s)\n\n", r.Kind, svc, r.Env, r.Image, r.Actor)
	vals := []string{r.Kind, r.Service, r.Env, r.Image, r.Ref, r.Commit, r.Actor, r.Operation}
	for i, k := range trailerKeys {
		fmt.Fprintf(&b, "Forge-%s: %s\n", k, vals[i])
	}
	return b.String()
}

// parseRecord reads a deploy record back from a commit; ok is false for
// commits Forge did not write.
func parseRecord(sha, message string, t time.Time) (deployRecord, bool) {
	r := deployRecord{DeployCommit: sha, Time: t.UTC().Format(time.RFC3339)}
	found := 0
	for line := range strings.SplitSeq(message, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok || !strings.HasPrefix(k, "Forge-") {
			continue
		}
		found++
		switch strings.TrimPrefix(k, "Forge-") {
		case "Kind":
			r.Kind = v
		case "Service":
			r.Service = v
		case "Env":
			r.Env = v
		case "Image":
			r.Image = v
		case "Ref":
			r.Ref = v
		case "Commit":
			r.Commit = v
		case "Actor":
			r.Actor = v
		case "Operation":
			r.Operation = v
		}
	}
	return r, found > 0 && r.Kind != ""
}

var syncStates = map[string]forgev1.Deployment_SyncStatus{
	"Synced": forgev1.Deployment_SYNC_STATUS_SYNCED, "OutOfSync": forgev1.Deployment_SYNC_STATUS_OUT_OF_SYNC,
}

var healthStates = map[string]forgev1.Deployment_Health{
	"Healthy": forgev1.Deployment_HEALTH_HEALTHY, "Progressing": forgev1.Deployment_HEALTH_PROGRESSING,
	"Degraded": forgev1.Deployment_HEALTH_DEGRADED, "Missing": forgev1.Deployment_HEALTH_MISSING,
	"Suspended": forgev1.Deployment_HEALTH_SUSPENDED,
}

// deploymentFrom builds the API view of one environment.
func (d *Deploys) deploymentFrom(svc, env string, v deployValues, deployCommit string, st AppStatus) *forgev1.Deployment {
	dep := &forgev1.Deployment{
		Name: deployNamePrefix + svc + "/" + env, Service: servicePrefix + svc, Env: env,
		Image: v.Image, Ref: v.Forge.Ref, Commit: v.Forge.Commit, Url: "http://" + d.host(svc, env),
		Sync: forgev1.Deployment_SYNC_STATUS_UNKNOWN, Health: forgev1.Deployment_HEALTH_UNKNOWN,
		Message: st.Message, DeployCommit: deployCommit, DeployedBy: v.Forge.Actor,
	}
	if s, ok := syncStates[st.Sync]; ok {
		dep.Sync = s
	}
	if h, ok := healthStates[st.Health]; ok {
		dep.Health = h
	}
	if t, err := time.Parse(time.RFC3339, v.Forge.Time); err == nil {
		dep.DeployTime = timestamppb.New(t)
	}
	if ops := d.o.Operations.List(dep.Name, false); len(ops) > 0 {
		dep.Operation = ops[0].GetName()
	}
	return dep
}

// current reads an environment's values at the head and the commit that set them.
func (d *Deploys) current(ctx context.Context, svc, env string) (deployValues, string, error) {
	data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), "")
	if errors.Is(err, github.ErrNotFound) {
		return deployValues{}, "", connect.NewError(connect.CodeNotFound, fmt.Errorf("%s%s/%s has never been deployed", deployNamePrefix, svc, env))
	}
	if err != nil {
		return deployValues{}, "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo: %w", err))
	}
	v, err := parseValues(data)
	if err != nil {
		return v, "", connect.NewError(connect.CodeInternal, err)
	}
	hist, err := d.o.Repo.History(ctx, valuesPath(svc, env), 1)
	if err != nil || len(hist) == 0 {
		return v, "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("deploy repo history: %w", err))
	}
	return v, hist[0].SHA, nil
}

func (d *Deploys) GetDeployment(ctx context.Context, req *connect.Request[forgev1.GetDeploymentRequest]) (*connect.Response[forgev1.GetDeploymentResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	v, commit, err := d.current(ctx, svc, env)
	if err != nil {
		return nil, err
	}
	st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("argo cd: %w", err))
	}
	return connect.NewResponse(&forgev1.GetDeploymentResponse{Deployment: d.deploymentFrom(svc, env, v, commit, st)}), nil
}

func (d *Deploys) ListDeployments(ctx context.Context, req *connect.Request[forgev1.ListDeploymentsRequest]) (*connect.Response[forgev1.ListDeploymentsResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	if e := req.Msg.GetEnv(); e != "" {
		if err := validEnv(e); err != nil {
			return nil, err
		}
	}
	var out []*forgev1.Deployment
	for _, svc := range d.o.Catalog.ids() {
		if s := req.Msg.GetService(); s != "" && s != servicePrefix+svc {
			continue
		}
		for _, env := range deployEnvs {
			if e := req.Msg.GetEnv(); e != "" && e != env {
				continue
			}
			v, commit, err := d.current(ctx, svc, env)
			if connect.CodeOf(err) == connect.CodeNotFound {
				continue
			}
			if err != nil {
				return nil, err
			}
			st, err := d.o.Cluster.Application(ctx, d.o.ArgoNamespace, svc+"-"+env)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("argo cd: %w", err))
			}
			out = append(out, d.deploymentFrom(svc, env, v, commit, st))
		}
	}
	return connect.NewResponse(&forgev1.ListDeploymentsResponse{Deployments: out}), nil
}

// ListDeployHistory, RollbackDeploy and PromoteDeploy: Task 7.
func (d *Deploys) ListDeployHistory(context.Context, *connect.Request[forgev1.ListDeployHistoryRequest]) (*connect.Response[forgev1.ListDeployHistoryResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListDeployHistory is not implemented yet"))
}

func (d *Deploys) RollbackDeploy(context.Context, *connect.Request[forgev1.RollbackDeployRequest]) (*connect.Response[forgev1.RollbackDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("RollbackDeploy is not implemented yet"))
}

func (d *Deploys) PromoteDeploy(context.Context, *connect.Request[forgev1.PromoteDeployRequest]) (*connect.Response[forgev1.PromoteDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("PromoteDeploy is not implemented yet"))
}
