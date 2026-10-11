package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/pkg/client"
)

// Audit event kinds and actor kinds.
const (
	auditRequest = "request"
	auditOutcome = "outcome"

	actorUser      = "user"
	actorWorkload  = "workload"
	actorSystem    = "system"
	actorAnonymous = "anonymous"
)

// auditFieldMax is the most characters of a caller-derived field an event keeps.
const auditFieldMax = 256

// auditMessageMax is the most characters of an error message an event keeps.
const auditMessageMax = 500

// auditRecord is the audit event a write call builds while it runs: the
// interceptor creates it, the auth interceptor adds the actor, handlers and
// Operations.Start add the service and operation.
type auditRecord struct {
	mu sync.Mutex
	ev *forgev1.AuditEvent
	// failure overrides an OK result: a stream that ends with a failure
	// message instead of an error (forge create) did not succeed.
	failure string
}

type auditKey struct{}

func newAuditRecord(action string) *auditRecord {
	return &auditRecord{ev: &forgev1.AuditEvent{Kind: auditRequest, Action: action, Actor: &forgev1.AuditActor{Kind: actorAnonymous}}}
}

func withAuditRecord(ctx context.Context, r *auditRecord) context.Context {
	return context.WithValue(ctx, auditKey{}, r)
}

// auditFrom returns the call's record, or nil outside an audited call.
func auditFrom(ctx context.Context) *auditRecord {
	r, _ := ctx.Value(auditKey{}).(*auditRecord)
	return r
}

func (r *auditRecord) setActor(p *Principal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := p.Workload; w != nil {
		r.ev.Actor = &forgev1.AuditActor{Kind: actorWorkload, Subject: p.Subject, Repository: w.Repository}
		return
	}
	r.ev.Actor = &forgev1.AuditActor{Kind: actorUser, Subject: p.Subject, Email: p.Email}
}

func (r *auditRecord) setSystem() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Actor = &forgev1.AuditActor{Kind: actorSystem}
}

// snapshot returns a copy of the event so far.
func (r *auditRecord) snapshot() *forgev1.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return proto.Clone(r.ev).(*forgev1.AuditEvent)
}

// noteAuditService records the service of the call, when the request did not
// name it (destroying an environment, cancelling an operation).
func noteAuditService(ctx context.Context, service string) {
	if r := auditFrom(ctx); r != nil && service != "" && service != servicePrefix {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ev.Service == "" {
			r.ev.Service = service
		}
	}
}

// noteAuditFailure records that the call failed although it returns no error:
// finish then stores result "failed" with message in place of "ok".
func noteAuditFailure(ctx context.Context, message string) {
	if r := auditFrom(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.failure = message
	}
}

// noteAuditOperation records the operation the call started.
func noteAuditOperation(ctx context.Context, name string) {
	if r := auditFrom(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ev.Operation = name
	}
}

// noteAuditDetail adds one detail to the call's event. Like the details of
// describe, value must be an identifier or a setting, never free text.
func noteAuditDetail(ctx context.Context, key, value string) {
	if r := auditFrom(ctx); r != nil && value != "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ev.Details == nil {
			r.ev.Details = map[string]string{}
		}
		r.ev.Details[key] = auditText(value, auditFieldMax)
	}
}

// noteAuditTarget replaces the call's target, when the request did not name it.
func noteAuditTarget(ctx context.Context, target string) {
	if r := auditFrom(ctx); r != nil && target != "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.ev.Target = auditText(target, auditFieldMax)
	}
}

// describe fills service, target and details from the request message. The
// interceptor calls it before the handler runs, so an operation the handler
// starts copies them into its outcome template.
func (r *auditRecord) describe(act auditAction, msg any) {
	if msg == nil {
		return
	}
	service, target, det := act.describe(msg)
	for k, v := range det {
		det[k] = auditText(v, auditFieldMax)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if service != "" {
		r.ev.Service = auditText(service, auditFieldMax)
	}
	r.ev.Target, r.ev.Details = auditText(target, auditFieldMax), det
}

// safeDescribe is describe, but a panic in it leaves the event with its
// action only: the call still runs and is still recorded.
func (r *auditRecord) safeDescribe(act auditAction, msg any) {
	defer func() { _ = recover() }()
	r.describe(act, msg)
}

// finish sets the result and client and returns a copy of the event.
func (r *auditRecord) finish(h interface{ Get(string) string }, err error) *forgev1.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev.Result = &forgev1.AuditResult{Code: auditCode(err), Message: auditText(errMessage(err), auditMessageMax)}
	if err == nil && r.failure != "" {
		r.ev.Result = &forgev1.AuditResult{Code: "failed", Message: auditText(r.failure, auditMessageMax)}
	}
	if h != nil {
		r.ev.Client = &forgev1.AuditClient{Version: auditText(h.Get(client.HeaderClientVersion), auditFieldMax), RequestId: auditText(h.Get(client.HeaderRequestID), auditFieldMax)}
	}
	return proto.Clone(r.ev).(*forgev1.AuditEvent)
}

// auditCode is "ok" or the Connect code of err ("permission_denied", ...).
func auditCode(err error) string {
	if err == nil {
		return "ok"
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		switch {
		case errors.Is(err, context.Canceled):
			return connect.CodeCanceled.String()
		case errors.Is(err, context.DeadlineExceeded):
			return connect.CodeDeadlineExceeded.String()
		}
	}
	return connect.CodeOf(err).String()
}

func errMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// auditText makes caller-derived text safe to store and to print: valid
// UTF-8, no NUL bytes, control and bidi characters replaced by U+FFFD, at
// most max characters.
func auditText(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return '\uFFFD'
		}
		return r
	}, s)
	return clipRunes(s, max)
}

// anonymousPerWindow is the most events from anonymous callers stored per
// anonymousWindow; anyone can send those, and the log is append-only.
const (
	anonymousPerWindow = 60
	anonymousWindow    = time.Minute
)

// auditor appends events to the store, falling back to the log.
type auditor struct {
	store store.AuditStore
	log   *slog.Logger
	now   func() time.Time

	mu         sync.Mutex // guards the anonymous limiter
	windowEnd  time.Time
	windowN    int
	dropped    int64 // events not stored since start
	droppedLog time.Time
}

func (a *auditor) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// allow reports whether ev may be stored: events of anonymous callers are
// limited to anonymousPerWindow per window; the first one over the limit in
// a window is logged with the running count of dropped events.
func (a *auditor) allow(ev *forgev1.AuditEvent) bool {
	if ev.GetActor().GetKind() != actorAnonymous {
		return true
	}
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !now.Before(a.windowEnd) {
		a.windowEnd, a.windowN = now.Add(anonymousWindow), 0
	}
	if a.windowN < anonymousPerWindow {
		a.windowN++
		return true
	}
	a.dropped++
	if now.Sub(a.droppedLog) >= anonymousWindow {
		a.droppedLog = now
		a.log.Warn("anonymous audit events over the limit; not stored", "dropped_total", a.dropped, "limit_per_minute", anonymousPerWindow)
	}
	return false
}

// append stores ev. It does not depend on ctx's cancellation, so a caller
// that went away is still audited; a failed append is logged in full.
func (a *auditor) append(ctx context.Context, ev *forgev1.AuditEvent) {
	if a == nil {
		return
	}
	if !a.allow(ev) {
		return
	}
	if ev.Time == nil {
		ev.Time = timestamppb.New(a.clock())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.store.AppendAuditEvent(ctx, ev); err != nil {
		event := fmt.Sprintf("%v", ev)
		if b, merr := protojson.Marshal(ev); merr == nil {
			event = string(b)
		}
		a.log.Error("audit event not stored", "error", err, "event", event)
	}
}

// auditAction names a write procedure and reads its request.
type auditAction struct {
	name     string
	describe func(msg any) (service, target string, details map[string]string)
}

// serviceName turns "payments" or "services/payments" into "services/payments".
func serviceName(s string) string {
	if s == "" || strings.HasPrefix(s, servicePrefix) {
		return s
	}
	return servicePrefix + s
}

// deploymentService is the service of "deployments/<service>/<env>".
func deploymentService(name string) string {
	svc, _, ok := strings.Cut(strings.TrimPrefix(name, deployNamePrefix), "/")
	if !ok || svc == "" {
		return ""
	}
	return servicePrefix + svc
}

// details keeps the non-empty values of kv (key, value, key, value, ...).
func details(kv ...string) map[string]string {
	var m map[string]string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			if m == nil {
				m = map[string]string{}
			}
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func flag(b bool) string {
	if b {
		return strconv.FormatBool(b)
	}
	return ""
}

func ttl(d *durationpb.Duration) string {
	if d == nil || !d.IsValid() {
		return ""
	}
	return d.AsDuration().String()
}

// auditActions maps every write procedure to its action. The allowlisted
// details are identifiers and settings only, never free text from the caller.
var auditActions = map[string]auditAction{
	forgev1connect.ServiceCatalogServiceRegisterServiceProcedure: {"service.register", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.RegisterServiceRequest)
		s := serviceName(r.GetServiceId())
		return s, s, details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceUpdateServiceProcedure: {"service.update", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.UpdateServiceRequest)
		s := serviceName(r.GetService().GetName())
		return s, s, details("update_mask", strings.Join(r.GetUpdateMask().GetPaths(), ","), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceDeleteServiceProcedure: {"service.delete", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.DeleteServiceRequest)
		s := serviceName(r.GetName())
		return s, s, details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.ServiceCatalogServiceCreateServiceProcedure: {"service.create", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.CreateServiceRequest)
		s := serviceName(r.GetServiceId())
		return s, s, details("template", r.GetTemplate(), "template_version", r.GetTemplateVersion(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.EnvironmentServiceCreateEnvironmentProcedure: {"environment.create", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.CreateEnvironmentRequest)
		return serviceName(r.GetService()), envNamePrefix + r.GetEnvironmentId(),
			details("ref", r.GetRef(), "ttl", ttl(r.GetTtl()), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.EnvironmentServiceExtendEnvironmentProcedure: {"environment.extend", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.ExtendEnvironmentRequest)
		return "", r.GetName(), details("ttl", ttl(r.GetTtl()))
	}},
	forgev1connect.EnvironmentServiceDestroyEnvironmentProcedure: {"environment.destroy", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.DestroyEnvironmentRequest)
		return "", r.GetName(), details("validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServiceStartDeployProcedure: {"deploy.start", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.StartDeployRequest)
		s := serviceName(r.GetService())
		return s, deployNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + r.GetEnv(),
			details("env", r.GetEnv(), "ref", r.GetRef(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServiceRollbackDeployProcedure: {"deploy.rollback", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.RollbackDeployRequest)
		return deploymentService(r.GetName()), r.GetName(), details("to", r.GetTo(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.DeployServicePromoteDeployProcedure: {"deploy.promote", func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.PromoteDeployRequest)
		s := serviceName(r.GetService())
		to := r.GetTo()
		if to == "" {
			to = "production"
		}
		return s, deployNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + to,
			details("from", r.GetFrom(), "to", r.GetTo(), "validate_only", flag(r.GetValidateOnly()))
	}},
	forgev1connect.OperationServiceCancelOperationProcedure: {"operation.cancel", func(m any) (string, string, map[string]string) {
		return "", m.(*forgev1.CancelOperationRequest).GetName(), nil
	}},
	forgev1connect.ResourceServicePlanResourcesProcedure: {opResourcePlan, func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.PlanResourcesRequest)
		s := serviceName(r.GetService())
		return s, resourceNamePrefix + strings.TrimPrefix(s, servicePrefix) + "/" + r.GetEnv(),
			details("environment", r.GetEnv(), "allow_destroy", flag(r.GetAllowDestroy()))
	}},
	// The request names only the plan; the handler adds the service, the
	// target and the environment once it has read the plan.
	forgev1connect.ResourceServiceApplyResourcePlanProcedure: {opResourceApply, func(m any) (string, string, map[string]string) {
		r := m.(*forgev1.ApplyResourcePlanRequest)
		return "", r.GetName(), details("plan", r.GetName(), "validate_only", flag(r.GetValidateOnly()))
	}},
}

// auditReads lists every read procedure. A procedure in neither table fails
// TestAuditCoversEveryProcedure, so new RPCs are classified on purpose.
var auditReads = []string{
	forgev1connect.ServiceCatalogServiceListServicesProcedure,
	forgev1connect.ServiceCatalogServiceGetServiceProcedure,
	forgev1connect.AuthServiceGetServerInfoProcedure,
	forgev1connect.AuthServiceGetStatusProcedure,
	forgev1connect.TemplateServiceListTemplatesProcedure,
	forgev1connect.TemplateServiceGetTemplateProcedure,
	forgev1connect.TeamServiceListTeamsProcedure,
	forgev1connect.OperationServiceGetOperationProcedure,
	forgev1connect.OperationServiceListOperationsProcedure,
	forgev1connect.OperationServiceWatchOperationProcedure,
	forgev1connect.EnvironmentServiceGetEnvironmentProcedure,
	forgev1connect.EnvironmentServiceListEnvironmentsProcedure,
	forgev1connect.DeployServiceGetDeploymentProcedure,
	forgev1connect.DeployServiceListDeploymentsProcedure,
	forgev1connect.DeployServiceListDeployHistoryProcedure,
	forgev1connect.AuditServiceListAuditEventsProcedure,
	forgev1connect.AuditServiceGetAuditEventProcedure,
	forgev1connect.ResourceServiceGetResourcePlanProcedure,
	forgev1connect.ResourceServiceListResourcePlansProcedure,
	forgev1connect.ResourceServiceListResourcesProcedure,
	forgev1connect.ResourceServiceGetResourceProcedure,
}

// auditInterceptor records every write call; it runs outside authentication
// so refused calls are recorded too.
type auditInterceptor struct{ a *auditor }

func (i *auditInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		act, ok := auditActions[req.Spec().Procedure]
		if !ok {
			return next(ctx, req)
		}
		rec := newAuditRecord(act.name)
		rec.safeDescribe(act, req.Any())
		defer i.appendOnPanic(ctx, rec, req.Header())
		resp, err := next(withAuditRecord(ctx, rec), req)
		i.a.append(ctx, rec.finish(req.Header(), err))
		return resp, err
	}
}

// appendOnPanic is deferred by the wrappers: when the handler panicked it
// appends an "internal" event, then panics again with the same value.
func (i *auditInterceptor) appendOnPanic(ctx context.Context, rec *auditRecord, h interface{ Get(string) string }) {
	if v := recover(); v != nil {
		i.a.append(ctx, rec.finish(h, connect.NewError(connect.CodeInternal, fmt.Errorf("panic: %v", v))))
		panic(v)
	}
}

func (i *auditInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *auditInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		act, ok := auditActions[conn.Spec().Procedure]
		if !ok {
			return next(ctx, conn)
		}
		rec := newAuditRecord(act.name)
		sc := &auditConn{StreamingHandlerConn: conn, rec: rec, act: act}
		defer i.appendOnPanic(ctx, rec, conn.RequestHeader())
		err := next(withAuditRecord(ctx, rec), sc)
		i.a.append(ctx, rec.finish(conn.RequestHeader(), err))
		return err
	}
}

// auditConn describes the event from the first request message of a stream.
type auditConn struct {
	connect.StreamingHandlerConn
	rec  *auditRecord
	act  auditAction
	seen bool
}

func (c *auditConn) Receive(m any) error {
	err := c.StreamingHandlerConn.Receive(m)
	if err == nil && !c.seen {
		c.seen = true
		c.rec.safeDescribe(c.act, m)
	}
	return err
}
