package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// Rules with modes (phase 15, milestone D): which rules Forge checks, and
// the mode admins set for the switchable ones.

const rulePrefix = "rules/"

// ruleModes gives the policy engine the modes stored by admins.
type ruleModes struct{ st store.RuleStore }

func (m ruleModes) RuleModes(ctx context.Context) (map[string]string, error) {
	set, err := m.st.RuleModes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(set))
	for id, r := range set {
		out[id] = r.Mode
	}
	return out, nil
}

// useRuleModes makes the engines read their modes from st. A mode that
// cannot be read is its default, and the failure is logged.
func useRuleModes(st store.RuleStore, log *slog.Logger, engines ...*policy.Engine) {
	for _, e := range engines {
		if e == nil {
			continue
		}
		e.Modes = ruleModes{st}
		e.OnModesError = func(err error) {
			log.Warn("reading the rules' modes failed; every rule has its default", "error", err)
		}
	}
}

// policyService serves PolicyService.
type policyService struct {
	st  store.RuleStore
	now func() time.Time
}

func (s *policyService) ListRules(ctx context.Context, _ *connect.Request[forgev1.ListRulesRequest]) (*connect.Response[forgev1.ListRulesResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	set, err := s.modes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*forgev1.Rule, 0, len(policy.Rules))
	for _, r := range policy.Rules {
		out = append(out, ruleProto(r, set[r.ID]))
	}
	return connect.NewResponse(&forgev1.ListRulesResponse{Rules: out}), nil
}

func (s *policyService) modes(ctx context.Context) (map[string]store.RuleMode, error) {
	if s.st == nil {
		return map[string]store.RuleMode{}, nil
	}
	set, err := s.st.RuleModes(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the rules' modes: %w", err))
	}
	return set, nil
}

func ruleProto(r policy.Rule, set store.RuleMode) *forgev1.Rule {
	out := &forgev1.Rule{
		Name: rulePrefix + r.ID, Kind: r.Kind, Title: r.Title, Description: r.Description,
		Switchable: r.Switchable, Mode: policy.ModeEnforce, DefaultMode: policy.ModeEnforce,
	}
	if !r.Switchable {
		return out
	}
	out.DefaultMode, out.Mode = r.Default, r.Default
	if policy.ValidMode(set.Mode) {
		out.Mode, out.UpdatedBy, out.UpdateTime = set.Mode, set.UpdatedBy, timestamppb.New(set.UpdatedAt)
	}
	return out
}

// UpdateRule sets the mode of a switchable rule. Admins only: a rule decides
// what may reach production for every team.
func (s *policyService) UpdateRule(ctx context.Context, req *connect.Request[forgev1.UpdateRuleRequest]) (*connect.Response[forgev1.UpdateRuleResponse], error) {
	in := req.Msg.GetRule()
	id, ok := strings.CutPrefix(in.GetName(), rulePrefix)
	if !ok || id == "" {
		return nil, invalid(fmt.Sprintf("rule.name %q must look like rules/{rule}", in.GetName()))
	}
	if err := authorizeTeam(ctx, ActRuleUpdate, ""); err != nil {
		return nil, err
	}
	if paths := req.Msg.GetUpdateMask().GetPaths(); !slices.Equal(paths, []string{"mode"}) {
		return nil, invalid(fmt.Sprintf("update_mask must be [mode], got %q", paths))
	}
	r, ok := policy.RuleByID(id)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s is not a rule (forge policy list)", in.GetName()))
	}
	if !r.Switchable {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s guards Forge itself and is always enforced", in.GetName()))
	}
	if !policy.ValidMode(in.GetMode()) {
		return nil, invalid(fmt.Sprintf("mode %q must be enforce, warn or off", in.GetMode()))
	}
	if s.st == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge keeps no rule modes"))
	}
	set := store.RuleMode{Mode: in.GetMode(), UpdatedBy: actorOf(ctx), UpdatedAt: s.now()}
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.UpdateRuleResponse{Rule: ruleProto(r, set)}), nil
	}
	// The default is stored as no mode, so a later change of the default
	// reaches a rule nobody chose a mode for.
	stored := set.Mode
	if stored == r.Default {
		stored = ""
	}
	if err := s.st.SetRuleMode(ctx, r.ID, stored, set.UpdatedBy, set.UpdatedAt); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("save the rule's mode: %w", err))
	}
	if stored == "" {
		set = store.RuleMode{}
	}
	return connect.NewResponse(&forgev1.UpdateRuleResponse{Rule: ruleProto(r, set)}), nil
}
