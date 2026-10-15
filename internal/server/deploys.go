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
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
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
	DeleteFile(ctx context.Context, parent, tree, path, message string) (string, error)
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
	// Releases, when set, gives every deployed commit a version: a tag and
	// a GitHub release in the service's repo.
	Releases ReleaseGitHub
	// Resources, when set, says which database a service has in an
	// environment, so its pods get DATABASE_URL.
	Resources store.ResourceStore
	// Org is the GitHub org services' repos must be in.
	Org string
	// Domain: hosts are <service>.<env>.<Domain>.
	Domain string
	// PublicDomains are the domains services get public addresses under;
	// the first is the default. Empty means internal addresses only.
	PublicDomains []PublicDomain
	// DNSTunnel names, to the platform's DNS records, the tunnel this
	// cluster is reached through (default "production").
	DNSTunnel string
	// HostTaken reports whether a public address is in use already (it
	// resolves); nil skips the check.
	HostTaken func(ctx context.Context, host string) (bool, error)
	// ArgoNamespace holds the Applications (default argocd).
	ArgoNamespace string
	// ArgoURL is Argo CD's web address, e.g. https://argocd.example.com;
	// "" leaves Deployment.argo_url empty.
	ArgoURL string
	// SecretVaults are the 1Password vaults run.secrets may read; empty
	// refuses every secret.
	SecretVaults []string
	// SecretDeniedItems are items in those vaults no service may read: the
	// platform's own credentials. Compared without regard to case.
	SecretDeniedItems []string
	// Defaults: 10m, 10m, 5m (a first deploy waits for Argo CD to poll git).
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
	// requests maps a caller's StartDeploy request_id (requestKey) to its operation.
	requests map[string]string
	// cutLocks maps "owner/repo" to the lock held while a version is cut there.
	cutLocks sync.Map
}

// NewDeploys returns the deploy service.
func NewDeploys(o DeployOptions) *Deploys {
	if o.ArgoNamespace == "" {
		o.ArgoNamespace = "argocd"
	}
	if o.DNSTunnel == "" {
		o.DNSTunnel = "production"
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.ImageTimeout, 10 * time.Minute}, {&o.SyncTimeout, 10 * time.Minute}, {&o.NamespaceTimeout, 5 * time.Minute}, {&o.ImagePoll, 10 * time.Second}, {&o.Poll, 2 * time.Second}} {
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
	// Version is the tag the commit ran as; empty before Forge cut versions.
	Version string `yaml:"version,omitempty"`
	// Domain and Host are run.domain and run.host of the commit's forge.yaml,
	// kept so that a promotion gives the next environment the address the
	// service asked for.
	Domain string `yaml:"domain,omitempty"`
	Host   string `yaml:"host,omitempty"`
	// StagingInternal is run.staging.public: false, kept with them.
	StagingInternal bool `yaml:"stagingInternal,omitempty"`
	// Replicas and AutoscaleMax are run.replicas and run.autoscaling.max, kept
	// so that a promotion gives production what the service asked for.
	Replicas     int `yaml:"replicas,omitempty"`
	AutoscaleMax int `yaml:"autoscaleMax,omitempty"`
	// Image and DeployCommit come from the commit, not the values file.
	Image        string `yaml:"-"`
	DeployCommit string `yaml:"-"`
}

// deployValues is services/<svc>/<env>/values.yaml.
type deployValues struct {
	Name     string `yaml:"name"`
	Env      string `yaml:"env"`
	Image    string `yaml:"image"`
	Port     int32  `yaml:"port"`
	Health   string `yaml:"health"`
	Replicas int    `yaml:"replicas"`
	Host     string `yaml:"host"`
	// Listener is the Gateway listener of the route when it is not the
	// environment's own, and DNS the record of a public address.
	Listener string     `yaml:"listener,omitempty"`
	DNS      *deployDNS `yaml:"dns,omitempty"`
	// Public marks a public address with no record of Forge's: the root of a domain.
	Public bool `yaml:"public,omitempty"`
	// Database is set when the service has a database in this environment.
	Database *deployDatabase `yaml:"database,omitempty"`
	// forge.yaml's run options; Autoscaling only in production.
	Resources   *manifest.Resources `yaml:"resources,omitempty"`
	Vars        map[string]string   `yaml:"vars,omitempty"`
	Secrets     []manifest.Secret   `yaml:"secrets,omitempty"`
	Jobs        []manifest.Job      `yaml:"jobs,omitempty"`
	Autoscaling *deployAutoscaling  `yaml:"autoscaling,omitempty"`
	Forge       deployRecord        `yaml:"forge"`
}

// deployAutoscaling is the range KEDA keeps the Deployment in.
type deployAutoscaling struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// setOptions puts forge.yaml's run options into the values; forEnv then
// works out what of them applies in the environment.
func (v *deployValues) setOptions(r *manifest.Run) {
	if r == nil {
		return
	}
	v.Resources, v.Vars, v.Secrets, v.Jobs = r.Resources, r.Env, r.Secrets, r.Jobs
	v.Forge.Replicas = r.Replicas
	if r.Autoscaling != nil {
		v.Forge.AutoscaleMax = r.Autoscaling.Max
	}
}

// forEnv sets the pod count of env: one in staging; in production
// run.replicas, grown by the autoscaler up to run.autoscaling.max.
func (v *deployValues) forEnv(env string) {
	v.Replicas, v.Autoscaling = 1, nil
	if env != finalEnv {
		return
	}
	v.Replicas = max(v.Forge.Replicas, 1)
	if v.Forge.AutoscaleMax > v.Replicas {
		v.Autoscaling = &deployAutoscaling{Min: v.Replicas, Max: v.Forge.AutoscaleMax}
	}
}

// secretsAllowed refuses a 1Password item outside the vaults the platform
// allows: any other vault may hold the platform's own secrets.
func (d *Deploys) secretsAllowed(v deployValues) error {
	for _, s := range v.Secrets {
		if !slices.Contains(d.o.SecretVaults, s.Vault()) {
			allowed := "none: this Forge API takes no 1Password secrets"
			if len(d.o.SecretVaults) > 0 {
				allowed = strings.Join(d.o.SecretVaults, ", ")
			}
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"run.secrets %s: the vault %q is not one services may read (allowed: %s)", s.Name, s.Vault(), allowed))
		}
		item := s.ItemName()
		// An item named by its ID would get past the deny list, which has names.
		if onePasswordID.MatchString(item) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"run.secrets %s: name the item %q by its title, not its 1Password ID", s.Name, item))
		}
		if slices.ContainsFunc(d.o.SecretDeniedItems, func(denied string) bool { return strings.EqualFold(denied, item) }) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"run.secrets %s: the item %q holds the platform's own credentials; services may not read it", s.Name, item))
		}
	}
	return nil
}

// onePasswordID is the form of a 1Password vault or item ID.
var onePasswordID = regexp.MustCompile(`^[a-z0-9]{26}$`)

// deployDatabase tells the chart where the app's DATABASE_URL comes from.
type deployDatabase struct {
	// Secret is the Secret CloudNativePG wrote; its "uri" key is the URL.
	Secret string `yaml:"secret"`
}

func (v deployValues) marshal() []byte {
	data, err := yaml.Marshal(v)
	if err != nil {
		panic(err) // a plain struct always marshals
	}
	return append([]byte("# Written by Forge; change it with 'forge deploy'.\n"), data...)
}

// isPublic reports whether the deployment is reachable from the internet.
func (v *deployValues) isPublic() bool { return v != nil && (v.DNS != nil || v.Public) }

// setAddress puts an address into the values the chart reads.
func (v *deployValues) setAddress(a address) {
	v.Host, v.Listener, v.DNS, v.Public = a.host, a.listener, a.dns, a.public && a.dns == nil
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
	if r.Version != "" {
		svc += " " + r.Version
	}
	fmt.Fprintf(&b, "%s %s to %s: %s (%s)\n\n", r.Kind, svc, r.Env, r.Image, r.Actor)
	vals := []string{r.Kind, r.Service, r.Env, r.Image, r.Ref, r.Commit, r.Actor, r.Operation}
	for i, k := range trailerKeys {
		fmt.Fprintf(&b, "Forge-%s: %s\n", k, vals[i])
	}
	if r.Version != "" {
		fmt.Fprintf(&b, "Forge-Version: %s\n", r.Version)
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
		case "Version":
			r.Version = v
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
		Image: v.Image, Ref: v.Forge.Ref, Commit: v.Forge.Commit, Url: hostURL(v.Host, v.isPublic()),
		Sync: forgev1.Deployment_SYNC_STATUS_UNKNOWN, Health: forgev1.Deployment_HEALTH_UNKNOWN,
		Message: st.Message, DeployCommit: deployCommit, DeployedBy: v.Forge.Actor, Version: v.Forge.Version,
	}
	if s, ok := syncStates[st.Sync]; ok {
		dep.Sync = s
	}
	if d.o.ArgoURL != "" {
		dep.ArgoUrl = d.o.ArgoURL + "/applications/" + d.o.ArgoNamespace + "/" + svc + "-" + env
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
	if err != nil {
		return v, "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("deploy repo history: %w", err))
	}
	if len(hist) == 0 {
		return v, "", connect.NewError(connect.CodeInternal, fmt.Errorf("no history for %s", valuesPath(svc, env)))
	}
	return v, hist[0].SHA, nil
}

func (d *Deploys) GetDeployment(ctx context.Context, req *connect.Request[forgev1.GetDeploymentRequest]) (*connect.Response[forgev1.GetDeploymentResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	if workloadOf(ctx) == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := ownDeployment(ctx, svc, env); err != nil {
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
	dep := d.deploymentFrom(svc, env, v, commit, st)
	if env != finalEnv {
		dep.Version = shownVersion(env, v, d.productionOf(ctx, svc))
	}
	return connect.NewResponse(&forgev1.GetDeploymentResponse{Deployment: dep}), nil
}

// productionOf is what svc runs in production; nil when that is nothing or
// cannot be read.
func (d *Deploys) productionOf(ctx context.Context, svc string) *deployValues {
	return d.currentValues(ctx, svc, finalEnv)
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
	ids, err := d.o.Catalog.ids(ctx)
	if err != nil {
		return nil, err
	}
	for _, svc := range ids {
		if s := req.Msg.GetService(); s != "" && s != servicePrefix+svc {
			continue
		}
		var staged *forgev1.Deployment
		var stagedValues deployValues
		var production *deployValues
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
			dep := d.deploymentFrom(svc, env, v, commit, st)
			if env == finalEnv {
				production = &v
			} else {
				staged, stagedValues = dep, v
			}
			out = append(out, dep)
		}
		if staged != nil {
			if production == nil && req.Msg.GetEnv() != "" {
				production = d.productionOf(ctx, svc)
			}
			staged.Version = shownVersion(staged.GetEnv(), stagedValues, production)
		}
	}
	return connect.NewResponse(&forgev1.ListDeploymentsResponse{Deployments: out}), nil
}

func (d *Deploys) ListDeployHistory(ctx context.Context, req *connect.Request[forgev1.ListDeployHistoryRequest]) (*connect.Response[forgev1.ListDeployHistoryResponse], error) {
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
	limit := int(req.Msg.GetLimit())
	switch {
	case limit == 0:
		limit = 20
	case limit < 0 || limit > 50:
		return nil, invalidArg("limit %d must be 1 to 50", limit)
	}
	recs, err := d.history(ctx, svc, env, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.DeployRecord, 0, len(recs))
	for _, r := range recs {
		pr := &forgev1.DeployRecord{DeployCommit: r.DeployCommit, Kind: r.Kind, Image: r.Image, Ref: r.Ref, Commit: r.Commit, Actor: r.Actor, Operation: r.Operation, Version: r.Version}
		if t, err := time.Parse(time.RFC3339, r.Time); err == nil {
			pr.Time = timestamppb.New(t)
		}
		out = append(out, pr)
	}
	return connect.NewResponse(&forgev1.ListDeployHistoryResponse{Records: out}), nil
}

// history returns Forge's records of svc/env, newest first.
func (d *Deploys) history(ctx context.Context, svc, env string, limit int) ([]deployRecord, error) {
	commits, err := d.o.Repo.History(ctx, valuesPath(svc, env), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("deploy repo history: %w", err))
	}
	var out []deployRecord
	for _, c := range commits {
		if r, ok := parseRecord(c.SHA, c.Message, c.Time); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *Deploys) RollbackDeploy(ctx context.Context, req *connect.Request[forgev1.RollbackDeployRequest]) (*connect.Response[forgev1.RollbackDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	svc, env, err := parseDeployName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	recs, err := d.history(ctx, svc, env, 50)
	if err != nil {
		return nil, err
	}
	var target *deployRecord
	switch to := req.Msg.GetTo(); {
	case to == "" && len(recs) < 2:
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s%s/%s has no earlier deploy to roll back to", deployNamePrefix, svc, env))
	case to == "":
		target = &recs[1]
	default:
		for i := range recs {
			if recs[i].DeployCommit == to || (len(to) >= 7 && strings.HasPrefix(recs[i].DeployCommit, to)) {
				target = &recs[i]
				break
			}
		}
		if target == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no deploy %s in the last 50 of %s%s/%s", to, deployNamePrefix, svc, env))
		}
	}
	data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), target.DeployCommit)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read %s: %w", shortSHA(target.DeployCommit), err))
	}
	v, err := parseValues(data)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	v.Forge.Kind, v.Forge.Actor = kindRollback, actorOf(ctx)
	// The address is worked out now, like the database: from the domain the
	// restored commit asked for, under today's settings. An older deploy's
	// address may be one the platform no longer hands out, or someone else's
	// by now, and a rollback must not take a public service off the internet.
	// Nor put an internal staging on it: run.staging.public: false stays.
	cur := d.currentValues(ctx, svc, env)
	if cur != nil && cur.Forge.StagingInternal {
		v.Forge.StagingInternal = true
	}
	addr, moved, err := d.addressFor(ctx, svc, env, wanted{domain: v.Forge.Domain, host: v.Forge.Host, stagingInternal: v.Forge.StagingInternal}, cur)
	if err != nil {
		return nil, err
	}
	v.setAddress(addr)
	// Like the address: under today's settings, not the restored deploy's.
	if err := d.secretsAllowed(v); err != nil {
		return nil, err
	}
	if err := d.o.Cluster.ServiceNamespaceFree(ctx, svc, env); err != nil {
		return nil, err
	}
	if err := d.o.Cluster.ApplicationFree(ctx, d.o.ArgoNamespace, svc+"-"+env); err != nil {
		return nil, err
	}
	preview := d.deploymentFrom(svc, env, v, "", AppStatus{})
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.RollbackDeployResponse{Deployment: preview}), nil
	}
	op, err := d.startMutation(ctx, kindRollback, svc, env, v, withAddressNote(moved, nil))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.RollbackDeployResponse{Operation: op, Deployment: preview}), nil
}

func (d *Deploys) PromoteDeploy(ctx context.Context, req *connect.Request[forgev1.PromoteDeployRequest]) (*connect.Response[forgev1.PromoteDeployResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	from, to := req.Msg.GetFrom(), req.Msg.GetTo()
	if from == "" {
		from = "staging"
	}
	if to == "" {
		to = "production"
	}
	if err := validEnv(from); err != nil {
		return nil, err
	}
	if err := validEnv(to); err != nil {
		return nil, err
	}
	if slices.Index(deployEnvs, to) != slices.Index(deployEnvs, from)+1 {
		return nil, invalidArg("promote goes one step forward: staging to production")
	}
	svc, ok := strings.CutPrefix(req.Msg.GetService(), servicePrefix)
	if !ok || svc == "" {
		return nil, invalidArg("service %q must be services/<name>", req.Msg.GetService())
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	src, _, err := d.current(ctx, svc, from)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has nothing in %s to promote", req.Msg.GetService(), from))
	}
	if err != nil {
		return nil, err
	}
	bump, err := bumpOf(req.Msg.GetBump(), to)
	if err != nil {
		return nil, err
	}
	addr, moved, err := d.addressFor(ctx, svc, to, wanted{domain: src.Forge.Domain, host: src.Forge.Host, stagingInternal: src.Forge.StagingInternal}, d.currentValues(ctx, svc, to))
	if err != nil {
		return nil, err
	}
	v := src
	v.Env = to
	v.setAddress(addr)
	v.forEnv(to)
	if err := d.secretsAllowed(v); err != nil {
		return nil, err
	}
	v.Forge.Env, v.Forge.Kind, v.Forge.Actor = to, kindPromote, actorOf(ctx)
	// Staging's candidate number is not production's version: that is cut
	// below, or there is none.
	v.Forge.Version = ""
	var cut func(context.Context, func(string, string)) (string, error)
	if d.o.Releases != nil {
		owner, repo, err := d.serviceRepo(ctx, svc)
		if err != nil {
			return nil, err
		}
		commit := v.Forge.Commit
		if v.Forge.Version, err = d.plannedVersion(ctx, owner, repo, commit, to, bump); err != nil {
			return nil, err
		}
		cut = func(ctx context.Context, emit func(string, string)) (string, error) {
			return d.cutVersion(ctx, owner, repo, commit, to, bump, emit)
		}
	}
	if err := d.o.Cluster.ServiceNamespaceFree(ctx, svc, to); err != nil {
		return nil, err
	}
	if err := d.o.Cluster.ApplicationFree(ctx, d.o.ArgoNamespace, svc+"-"+to); err != nil {
		return nil, err
	}
	preview := d.deploymentFrom(svc, to, v, "", AppStatus{})
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.PromoteDeployResponse{Deployment: preview}), nil
	}
	op, err := d.startMutation(ctx, kindPromote, svc, to, v, withAddressNote(moved, cut))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.PromoteDeployResponse{Operation: op, Deployment: preview}), nil
}

// currentValues is what svc runs in env now, nil when that is nothing or
// cannot be read.
func (d *Deploys) currentValues(ctx context.Context, svc, env string) *deployValues {
	data, err := d.o.Repo.Read(ctx, valuesPath(svc, env), "")
	if err != nil {
		return nil
	}
	v, err := parseValues(data)
	if err != nil {
		return nil
	}
	return &v
}

// withAddressNote tells the operation's reader that the address changes,
// before whatever next does. next may be nil.
func withAddressNote(note string, next func(context.Context, func(string, string)) (string, error)) func(context.Context, func(string, string)) (string, error) {
	if note == "" {
		return next
	}
	return func(ctx context.Context, emit func(string, string)) (string, error) {
		emit("address", note)
		if next == nil {
			return "", nil
		}
		return next(ctx, emit)
	}
}

// startMutation locks svc/env and starts an operation that applies v. cut,
// when set, first makes sure the commit has its version and returns it.
func (d *Deploys) startMutation(ctx context.Context, kind, svc, env string, v deployValues, cut func(context.Context, func(string, string)) (string, error)) (*forgev1.Operation, error) {
	target := svc + "/" + env
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.claim(ctx, svc, env); err != nil {
		return nil, err
	}
	op := d.o.Operations.Start(ctx, kind, deployNamePrefix+target, v.Forge.Actor, func(ctx context.Context, emit func(string, string)) error {
		defer d.unlock(target)
		if cut != nil {
			version, err := cut(ctx, emit)
			if err != nil {
				return err
			}
			v.Forge.Version = version
		}
		return d.apply(ctx, svc, env, v, emit)
	})
	d.running[target] = op.GetName()
	return op, nil
}
