package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runAuditSuite checks an AuditStore. open returns an empty store per subtest.
func runAuditSuite(t *testing.T, open func(t *testing.T) AuditStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	ev := func(min int, action, service, email, code string) *forgev1.AuditEvent {
		return &forgev1.AuditEvent{
			Time: timestamppb.New(at.Add(time.Duration(min) * time.Minute)), Kind: "request", Action: action,
			Actor:   &forgev1.AuditActor{Kind: "user", Subject: "sub-" + email, Email: email},
			Service: service, Target: service,
			Result:  &forgev1.AuditResult{Code: code},
			Details: map[string]string{"env": "staging"},
			Client:  &forgev1.AuditClient{Version: "0.3.0", RequestId: "req-1"},
		}
	}
	appendAll := func(t *testing.T, s AuditStore, evs ...*forgev1.AuditEvent) []*forgev1.AuditEvent {
		t.Helper()
		var out []*forgev1.AuditEvent
		for _, e := range evs {
			got, err := s.AppendAuditEvent(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, got)
		}
		return out
	}
	actions := func(evs []*forgev1.AuditEvent) []string {
		var out []string
		for _, e := range evs {
			out = append(out, e.GetAction())
		}
		return out
	}

	t.Run("append assigns increasing names and keeps fields", func(t *testing.T) {
		s := open(t)
		in := ev(0, "deploy.start", "services/payments", "ana@example.com", "ok")
		got := appendAll(t, s, in, ev(1, "deploy.promote", "services/payments", "ana@example.com", "ok"))
		a, okA := ParseAuditEventName(got[0].GetName())
		b, okB := ParseAuditEventName(got[1].GetName())
		if !okA || !okB || b <= a {
			t.Fatalf("names %q, %q: want increasing auditEvents/<id>", got[0].GetName(), got[1].GetName())
		}
		back, err := s.GetAuditEvent(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		in.Name = got[0].GetName()
		if !proto.Equal(back, in) {
			t.Fatalf("round trip:\n got %v\nwant %v", back, in)
		}
		if in.GetName() == "" || back == in {
			t.Fatal("the store must copy events in and out")
		}
	})

	t.Run("append sets a missing time", func(t *testing.T) {
		s := open(t)
		e := ev(0, "x", "", "", "ok")
		e.Time = nil
		got := appendAll(t, s, e)
		if got[0].GetTime() == nil || time.Since(got[0].GetTime().AsTime()) > time.Minute {
			t.Fatalf("time %v", got[0].GetTime())
		}
	})

	t.Run("get missing", func(t *testing.T) {
		s := open(t)
		if _, err := s.GetAuditEvent(ctx, 999999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("list filters, newest first", func(t *testing.T) {
		s := open(t)
		appendAll(t, s,
			ev(0, "service.register", "services/a", "ana@example.com", "ok"),
			ev(1, "deploy.start", "services/a", "bo@example.com", "permission_denied"),
			ev(2, "deploy.start", "services/b", "ana@example.com", "ok"),
			&forgev1.AuditEvent{Time: timestamppb.New(at.Add(3 * time.Minute)), Kind: "request", Action: "deploy.start",
				Actor:   &forgev1.AuditActor{Kind: "workload", Subject: "github:showmino/a", Repository: "showmino/a"},
				Service: "services/a", Result: &forgev1.AuditResult{Code: "ok"}},
			&forgev1.AuditEvent{Time: timestamppb.New(at.Add(4 * time.Minute)), Kind: "request", Action: "environment.expire",
				Actor: &forgev1.AuditActor{Kind: "system"}, Service: "services/b", Result: &forgev1.AuditResult{Code: "ok"}},
		)
		for _, tc := range []struct {
			name string
			q    AuditQuery
			want []string
		}{
			{"all", AuditQuery{Limit: 10}, []string{"environment.expire", "deploy.start", "deploy.start", "deploy.start", "service.register"}},
			{"limit", AuditQuery{Limit: 2}, []string{"environment.expire", "deploy.start"}},
			{"service", AuditQuery{Service: "services/b", Limit: 10}, []string{"environment.expire", "deploy.start"}},
			{"actor email", AuditQuery{Actor: "ana@example.com", Limit: 10}, []string{"deploy.start", "service.register"}},
			{"actor repository", AuditQuery{Actor: "showmino/a", Limit: 10}, []string{"deploy.start"}},
			{"actor kind", AuditQuery{Actor: "system", Limit: 10}, []string{"environment.expire"}},
			{"action", AuditQuery{Action: "service.register", Limit: 10}, []string{"service.register"}},
			{"failed only", AuditQuery{FailedOnly: true, Limit: 10}, []string{"deploy.start"}},
			{"since inclusive", AuditQuery{Since: at.Add(3 * time.Minute), Limit: 10}, []string{"environment.expire", "deploy.start"}},
			{"until exclusive", AuditQuery{Until: at.Add(1 * time.Minute), Limit: 10}, []string{"service.register"}},
		} {
			got, err := s.ListAuditEvents(ctx, tc.q)
			if err != nil || !slices.Equal(actions(got), tc.want) {
				t.Errorf("%s: %v %v, want %v", tc.name, actions(got), err, tc.want)
			}
		}
	})

	t.Run("paging is stable while events arrive", func(t *testing.T) {
		s := open(t)
		got := appendAll(t, s, ev(0, "a1", "", "", "ok"), ev(1, "a2", "", "", "ok"), ev(2, "a3", "", "", "ok"))
		first, _ := s.ListAuditEvents(ctx, AuditQuery{Limit: 2})
		if !slices.Equal(actions(first), []string{"a3", "a2"}) {
			t.Fatalf("first page %v", actions(first))
		}
		appendAll(t, s, ev(3, "a4", "", "", "ok")) // a new event before the next page
		last, _ := ParseAuditEventName(first[len(first)-1].GetName())
		next, _ := s.ListAuditEvents(ctx, AuditQuery{BeforeID: last, Limit: 2})
		if !slices.Equal(actions(next), []string{"a1"}) {
			t.Fatalf("next page %v, want [a1]", actions(next))
		}
		_ = got
	})
}
