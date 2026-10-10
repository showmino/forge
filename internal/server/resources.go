package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/resource"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// Error codes a resource operation may end with, besides cancelled and internal.
const (
	OpCodePolicyDenied = "policy_denied"
	opCodeRunFailed    = "run_failed"
	opCodeRunTimeout   = "run_timeout"
	opCodeRunnerFailed = "runner_failed"
	opCodeLaunchFailed = "launch_failed"
)

// maxRunResult is the most bytes of plan file or plan JSON a runner may hand back.
const maxRunResult = 8 << 20

// ServiceNamespaces checks and makes the namespace <service>-<env>.
type ServiceNamespaces interface {
	// ServiceNamespaceFree fails when the namespace exists and is not Forge's.
	ServiceNamespaceFree(ctx context.Context, service, env string) error
	// EnsureServiceNamespace makes the namespace, with Forge's labels, when it is missing.
	EnsureServiceNamespace(ctx context.Context, service, env string) error
}

// DatabaseHealth reads a database's live health from the cluster.
type DatabaseHealth interface {
	DatabaseHealth(ctx context.Context, service, env, name string) (*forgev1.ResourceHealth, error)
}

// ResourceOptions configure resources.
type ResourceOptions struct {
	Store      store.ResourceStore
	Catalog    *Catalog
	Operations *Operations
	Policy     *policy.Engine
	Launcher   run.Launcher
	// Namespaces checks and makes service namespaces; nil skips both.
	Namespaces ServiceNamespaces
	// Health reads live health; nil reports "unknown".
	Health DatabaseHealth
	// RunTimeout is how long a run may take (default 15m).
	RunTimeout time.Duration
	// PlanTTL is how long a plan may wait to be applied (default 24h).
	PlanTTL time.Duration
	// Poll is how often a run's launcher is asked for its status (default 2s).
	Poll time.Duration
	// Grace is how long after a runner ended cleanly its result may still
	// arrive (default 10s).
	Grace time.Duration
	// SweepPage is how many plans the sweep reads per page (default 500).
	SweepPage int
	Now       func() time.Time
	Log       *slog.Logger
}

// Resources serves ResourceService and is the runner's API. A nil
// *Resources answers FAILED_PRECONDITION.
type Resources struct {
	o     ResourceOptions
	audit atomic.Pointer[auditor]

	mu sync.Mutex
	// running maps "<service>/<env>" to the operation running there.
	running map[string]string
	// requests maps a caller's PlanResources request_id (requestKey) to its plan.
	requests map[string]string
	// live maps a plan to the run an operation of this process is watching.
	live map[string]*liveRun
}

// liveRun connects a run to the operation that watches it.
type liveRun struct {
	emit  func(step, message string)
	ended chan struct{}
	once  sync.Once
}

// NewResources returns the resource service.
func NewResources(o ResourceOptions) (*Resources, error) {
	if o.Store == nil || o.Catalog == nil || o.Operations == nil || o.Policy == nil || o.Launcher == nil {
		return nil, errors.New("resources: Store, Catalog, Operations, Policy and Launcher are required")
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&o.RunTimeout, 15 * time.Minute}, {&o.PlanTTL, 24 * time.Hour}, {&o.Poll, 2 * time.Second}, {&o.Grace, 10 * time.Second}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if o.SweepPage <= 0 {
		o.SweepPage = 500
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Resources{o: o, running: map[string]string{}, requests: map[string]string{}, live: map[string]*liveRun{}}, nil
}

func (r *Resources) ready() error {
	if r == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("resources are not configured on this Forge API"))
	}
	return nil
}

// setAuditor makes ended runs append their outcome to the audit log.
func (r *Resources) setAuditor(a *auditor) { r.audit.Store(a) }

func precondition(format string, a ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, a...))
}

var planIDRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// parsePlanName accepts "resourcePlans/<id>" or "<id>".
func parsePlanName(name string) (string, error) {
	id := strings.TrimPrefix(name, planNamePrefix)
	if !planIDRE.MatchString(id) {
		return "", invalidArg("%q is not a plan name (resourcePlans/<id>)", name)
	}
	return planNamePrefix + id, nil
}

func newPlanID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newRunToken returns a run token and the hash forge-api stores. The token
// goes to the launcher only: never log it.
func newRunToken() (token, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func subjectOf(ctx context.Context) string {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.Subject
	}
	return ""
}

// modeOf is the run a plan in state s is in: plan, apply, or "" for none.
func modeOf(s forgev1.ResourcePlan_State) string {
	switch s {
	case forgev1.ResourcePlan_STATE_PLANNING:
		return run.ModePlan
	case forgev1.ResourcePlan_STATE_APPLYING:
		return run.ModeApply
	default:
		return ""
	}
}

// view is the plan as the API shows it: a ready plan past its expiry reads
// as expired, also before the sweep has marked it.
func (r *Resources) view(rec *store.PlanRecord) *forgev1.ResourcePlan {
	p := proto.Clone(rec.Plan).(*forgev1.ResourcePlan)
	if p.GetState() == forgev1.ResourcePlan_STATE_PLANNED && !r.o.Now().Before(p.GetExpireTime().AsTime()) {
		p.State = forgev1.ResourcePlan_STATE_EXPIRED
	}
	return p
}

// planAccess checks that the caller may plan service's resources: a writer,
// or the service's own CI without allow_destroy.
func planAccess(ctx context.Context, service string, allowDestroy bool) error {
	w := workloadOf(ctx)
	if w == nil {
		return require(ctx, PermServicesWrite)
	}
	if service != w.Service {
		return workloadDenied(w, "only plan the resources of %s%s, not of %s%s", servicePrefix, w.Service, servicePrefix, service)
	}
	if allowDestroy {
		return workloadDenied(w, "not plan with allow_destroy")
	}
	return nil
}

// PlanResources saves a plan record and starts the run that fills it.
func (r *Resources) PlanResources(ctx context.Context, req *connect.Request[forgev1.PlanResourcesRequest]) (*connect.Response[forgev1.PlanResourcesResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	m := req.Msg
	name, err := parseName(m.GetService())
	if err != nil {
		return nil, err
	}
	svc, env := strings.TrimPrefix(name, servicePrefix), m.GetEnv()
	if err := planAccess(ctx, svc, m.GetAllowDestroy()); err != nil {
		return nil, err
	}
	if err := validEnv(env); err != nil {
		return nil, err
	}
	if ok, err := r.o.Catalog.has(ctx, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, notFound(name)
	}
	settings, err := resource.Resolve(m.GetResources(), env)
	if err != nil {
		return nil, invalidArg("resources: %v", err)
	}
	if r.o.Namespaces != nil {
		if err := r.o.Namespaces.ServiceNamespaceFree(ctx, svc, env); err != nil {
			return nil, err
		}
	}

	target := svc + "/" + env
	r.mu.Lock()
	defer r.mu.Unlock()
	if key := m.GetRequestId(); key != "" {
		if planName, ok := r.requests[requestKey(ctx, key)]; ok {
			if rec, err := r.o.Store.GetResourcePlan(ctx, planName); err == nil {
				op, _ := r.o.Operations.Get(rec.Plan.GetOperation())
				return connect.NewResponse(&forgev1.PlanResourcesResponse{Operation: op, Plan: r.view(rec)}), nil
			}
		}
	}
	if running, ok := r.running[target]; ok {
		return nil, precondition("%s is already running for %s%s", running, resourceNamePrefix, target)
	}
	if other, err := r.storedRun(ctx, name, env); err != nil {
		return nil, err
	} else if other != "" {
		return nil, precondition("%s is still running for %s%s; it ends within %s", other, resourceNamePrefix, target, r.o.RunTimeout)
	}
	now := r.o.Now()
	token, hash := newRunToken()
	rec := &store.PlanRecord{
		Plan: &forgev1.ResourcePlan{
			Name: planNamePrefix + newPlanID(), Service: name, Env: env, State: forgev1.ResourcePlan_STATE_PLANNING,
			Resources: settings, AllowDestroy: m.GetAllowDestroy(), Creator: actorOf(ctx),
			CreateTime: timestamppb.New(now), ExpireTime: timestamppb.New(now.Add(r.o.PlanTTL)),
		},
		CreatorSubject: subjectOf(ctx), Digest: resource.Digest(settings),
		TokenHash: hash, Deadline: now.Add(r.o.RunTimeout),
	}
	noteAuditDetail(ctx, "plan", rec.Plan.GetName())
	op, err := r.startRun(ctx, rec, run.ModePlan, token, nil)
	if err != nil {
		return nil, err
	}
	if key := m.GetRequestId(); key != "" {
		r.requests[requestKey(ctx, key)] = rec.Plan.GetName()
	}
	return connect.NewResponse(&forgev1.PlanResourcesResponse{Operation: op, Plan: r.view(rec)}), nil
}

// GetResourcePlan returns one plan: to every member, and to a CI workload
// when the plan is its own service's.
func (r *Resources) GetResourcePlan(ctx context.Context, req *connect.Request[forgev1.GetResourcePlanRequest]) (*connect.Response[forgev1.GetResourcePlanResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	w := workloadOf(ctx)
	if w == nil {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
	}
	name, err := parsePlanName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	rec, err := r.o.Store.GetResourcePlan(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, storeErr(err)
	}
	if w != nil && rec.Plan.GetService() != servicePrefix+w.Service {
		return nil, workloadDenied(w, "only read the plans of %s%s", servicePrefix, w.Service)
	}
	return connect.NewResponse(&forgev1.GetResourcePlanResponse{Plan: r.view(rec)}), nil
}

// ListResourcePlans returns plans, newest first.
func (r *Resources) ListResourcePlans(ctx context.Context, req *connect.Request[forgev1.ListResourcePlansRequest]) (*connect.Response[forgev1.ListResourcePlansResponse], error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	q := store.PlanQuery{Env: m.GetEnv(), Limit: size + 1}
	if m.GetEnv() != "" {
		if err := validEnv(m.GetEnv()); err != nil {
			return nil, err
		}
	}
	if m.GetService() != "" {
		name, err := parseName(m.GetService())
		if err != nil {
			return nil, err
		}
		q.Service = name
	}
	if m.GetMine() {
		q.CreatorSubject = subjectOf(ctx)
	}
	if t := m.GetPageToken(); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err == nil {
			q.BeforeSeq, err = strconv.ParseInt(string(b), 10, 64)
		}
		if err != nil || q.BeforeSeq < 1 {
			return nil, invalid("page_token is malformed")
		}
	}
	// A ready plan past its expiry reads as expired, so the state filter is
	// applied to what the caller will see: stored "planned" plans are fetched
	// too when expired ones are wanted, and every plan is filtered as viewed.
	want := m.GetStates()
	q.States = slices.Clone(want)
	if slices.Contains(want, forgev1.ResourcePlan_STATE_EXPIRED) && !slices.Contains(want, forgev1.ResourcePlan_STATE_PLANNED) {
		q.States = append(q.States, forgev1.ResourcePlan_STATE_PLANNED)
	}
	resp := &forgev1.ListResourcePlansResponse{}
	var seqs []int64
	for len(resp.Plans) <= size {
		recs, err := r.o.Store.ListResourcePlans(ctx, q)
		if err != nil {
			return nil, storeErr(err)
		}
		for _, rec := range recs {
			if p := r.view(rec); len(want) == 0 || slices.Contains(want, p.GetState()) {
				resp.Plans = append(resp.Plans, p)
				seqs = append(seqs, rec.Seq)
			}
		}
		if len(recs) < q.Limit {
			break // the store has no more
		}
		q.BeforeSeq = recs[len(recs)-1].Seq
	}
	if len(resp.Plans) > size {
		resp.Plans = resp.Plans[:size]
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seqs[size-1], 10)))
	}
	return connect.NewResponse(resp), nil
}

// storedRun returns the plan of a run the store says is still going for
// service ("services/<id>") and env: one this process does not watch, such
// as a run started before a restart. Its deadline has not passed, so its
// runner may still be alive and writing state.
func (r *Resources) storedRun(ctx context.Context, service, env string) (string, error) {
	recs, err := r.o.Store.ListResourcePlans(ctx, store.PlanQuery{Service: service, Env: env, Limit: 10,
		States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_PLANNING, forgev1.ResourcePlan_STATE_APPLYING}})
	if err != nil {
		return "", storeErr(err)
	}
	now := r.o.Now()
	for _, rec := range recs {
		if now.Before(rec.Deadline) {
			return rec.Plan.GetName(), nil
		}
	}
	return "", nil
}
