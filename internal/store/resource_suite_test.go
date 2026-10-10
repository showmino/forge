package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runResourceSuite checks a ResourceStore. open returns an empty store per subtest.
func runResourceSuite(t *testing.T, open func(t *testing.T) ResourceStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	plan := func(id, service, env string, state forgev1.ResourcePlan_State, subject string) *PlanRecord {
		return &PlanRecord{
			Plan: &forgev1.ResourcePlan{
				Name: "resourcePlans/" + id, Service: service, Env: env, State: state,
				Resources:  []*forgev1.ResourceSettings{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}},
				Creator:    subject + "@example.com",
				CreateTime: timestamppb.New(at), ExpireTime: timestamppb.New(at.Add(24 * time.Hour)),
			},
			CreatorSubject: subject, Digest: "digest-" + id,
		}
	}
	put := func(t *testing.T, s ResourceStore, recs ...*PlanRecord) {
		t.Helper()
		for _, r := range recs {
			if err := s.WriteResources(ctx, func(tx ResourceTx) error { return tx.PutResourcePlan(ctx, r) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := func(recs []*PlanRecord) string {
		out := ""
		for _, r := range recs {
			out += r.Plan.GetName()[len("resourcePlans/"):] + " "
		}
		return out
	}
	res := func(service, env, name string, state forgev1.Resource_State) *forgev1.Resource {
		return &forgev1.Resource{
			Name: "resources/" + service + "/" + env + "/" + name, Service: "services/" + service, Env: env, Type: "postgres",
			Settings: &forgev1.ResourceSettings{Name: name, Type: "postgres", StorageGi: 5, Instances: 1}, State: state,
			Secret: name + "-app", CreateTime: timestamppb.New(at),
		}
	}

	t.Run("a plan keeps every field and gets a sequence number", func(t *testing.T) {
		s := open(t)
		p := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")
		p.TokenHash, p.Deadline = "hash-a", at.Add(15*time.Minute)
		p.PlanFile, p.PlanJSON, p.Audit, p.StateVersion = []byte{0, 1, 2}, []byte(`{"a":1}`), []byte(`{"action":"resource.plan"}`), 7
		put(t, s, p)
		if p.Seq < 1 {
			t.Fatalf("Seq %d, want it set", p.Seq)
		}
		got, err := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got.Plan, p.Plan) || got.Seq != p.Seq || got.CreatorSubject != "ana" || got.Digest != "digest-aaaaaaaaaaaa" ||
			got.StateVersion != 7 || got.TokenHash != "hash-a" || !got.Deadline.Equal(p.Deadline) ||
			!bytes.Equal(got.PlanFile, p.PlanFile) || !bytes.Equal(got.PlanJSON, p.PlanJSON) || !bytes.Equal(got.Audit, p.Audit) {
			t.Errorf("got %+v, want %+v", got, p)
		}
		// What comes out is a copy.
		got.Plan.State = forgev1.ResourcePlan_STATE_FAILED
		again, _ := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa")
		if again.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNING {
			t.Error("the store shares its record with the caller")
		}

		byToken, err := s.ResourcePlanByToken(ctx, "hash-a")
		if err != nil || byToken.Plan.GetName() != p.Plan.GetName() {
			t.Errorf("by token: %v %v", byToken, err)
		}
		for _, hash := range []string{"hash-b", ""} {
			if _, err := s.ResourcePlanByToken(ctx, hash); !errors.Is(err, ErrNotFound) {
				t.Errorf("token %q: %v, want ErrNotFound", hash, err)
			}
		}
		if _, err := s.GetResourcePlan(ctx, "resourcePlans/bbbbbbbbbbbb"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a missing plan: %v", err)
		}
	})

	t.Run("replacing a plan keeps its sequence number and clears what was cleared", func(t *testing.T) {
		s := open(t)
		p := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")
		p.TokenHash, p.Deadline, p.PlanFile = "hash-a", at, []byte("file")
		put(t, s, p)
		seq := p.Seq
		p.Plan.State, p.TokenHash, p.Deadline, p.PlanFile = forgev1.ResourcePlan_STATE_PLANNED, "", time.Time{}, nil
		put(t, s, p)
		got, err := s.GetResourcePlan(ctx, p.Plan.GetName())
		if err != nil {
			t.Fatal(err)
		}
		if got.Seq != seq || got.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED || got.TokenHash != "" || !got.Deadline.IsZero() || len(got.PlanFile) != 0 {
			t.Errorf("after the replace: %+v", got)
		}
		if _, err := s.ResourcePlanByToken(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the old token still finds the plan: %v", err)
		}
	})

	t.Run("list filters and pages, newest first", func(t *testing.T) {
		s := open(t)
		a := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_APPLIED, "ana")
		b := plan("bbbbbbbbbbbb", "services/payments", "production", forgev1.ResourcePlan_STATE_PLANNED, "ana")
		c := plan("cccccccccccc", "services/ledger", "staging", forgev1.ResourcePlan_STATE_PLANNED, "bo")
		d := plan("dddddddddddd", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNED, "bo")
		put(t, s, a, b, c, d)
		for name, tc := range map[string]struct {
			q    PlanQuery
			want string
		}{
			"everything":      {PlanQuery{Limit: 10}, "dddddddddddd cccccccccccc bbbbbbbbbbbb aaaaaaaaaaaa "},
			"a service":       {PlanQuery{Service: "services/payments", Limit: 10}, "dddddddddddd bbbbbbbbbbbb aaaaaaaaaaaa "},
			"an environment":  {PlanQuery{Service: "services/payments", Env: "staging", Limit: 10}, "dddddddddddd aaaaaaaaaaaa "},
			"states":          {PlanQuery{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_APPLIED, forgev1.ResourcePlan_STATE_FAILED}, Limit: 10}, "aaaaaaaaaaaa "},
			"a creator":       {PlanQuery{CreatorSubject: "bo", Limit: 10}, "dddddddddddd cccccccccccc "},
			"a limit":         {PlanQuery{Limit: 2}, "dddddddddddd cccccccccccc "},
			"before a cursor": {PlanQuery{BeforeSeq: c.Seq, Limit: 10}, "bbbbbbbbbbbb aaaaaaaaaaaa "},
			"nothing":         {PlanQuery{Service: "services/none", Limit: 10}, ""},
		} {
			got, err := s.ListResourcePlans(ctx, tc.q)
			if err != nil || names(got) != tc.want {
				t.Errorf("%s: %q (%v), want %q", name, names(got), err, tc.want)
			}
		}
	})

	t.Run("a list carries no plan files", func(t *testing.T) {
		s := open(t)
		p := plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNED, "ana")
		p.PlanFile, p.PlanJSON, p.Audit, p.StateVersion = []byte{0, 1, 2}, []byte(`{"a":1}`), []byte(`{"action":"resource.plan"}`), 7
		p.TokenHash, p.Deadline = "hash-a", at.Add(15*time.Minute)
		put(t, s, p)
		got, err := s.ListResourcePlans(ctx, PlanQuery{Limit: 10})
		if err != nil || len(got) != 1 {
			t.Fatalf("list: %v (%v)", got, err)
		}
		if len(got[0].PlanFile) != 0 || len(got[0].PlanJSON) != 0 || len(got[0].Audit) != 0 {
			t.Errorf("a listed plan has files: %d %d %d bytes", len(got[0].PlanFile), len(got[0].PlanJSON), len(got[0].Audit))
		}
		if got[0].Digest != p.Digest || got[0].StateVersion != 7 || got[0].TokenHash != "hash-a" || !got[0].Deadline.Equal(p.Deadline) || got[0].CreatorSubject != "ana" {
			t.Errorf("a listed plan lost its other fields: %+v", got[0])
		}
		one, err := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa")
		if err != nil || !bytes.Equal(one.PlanFile, p.PlanFile) || !bytes.Equal(one.PlanJSON, p.PlanJSON) || !bytes.Equal(one.Audit, p.Audit) {
			t.Errorf("a read plan lost its files: %+v (%v)", one, err)
		}
	})

	t.Run("a failed transaction writes nothing", func(t *testing.T) {
		s := open(t)
		boom := errors.New("boom")
		err := s.WriteResources(ctx, func(tx ResourceTx) error {
			if err := tx.PutResourcePlan(ctx, plan("aaaaaaaaaaaa", "services/payments", "staging", forgev1.ResourcePlan_STATE_PLANNING, "ana")); err != nil {
				return err
			}
			if err := tx.PutResource(ctx, res("payments", "staging", "main", forgev1.Resource_STATE_APPLYING)); err != nil {
				return err
			}
			// Reads inside the transaction see its writes.
			if _, err := tx.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa"); err != nil {
				return fmt.Errorf("read own plan: %w", err)
			}
			if rs, err := tx.ListResources(ctx, "services/payments", ""); err != nil || len(rs) != 1 {
				return fmt.Errorf("read own resource: %v %w", rs, err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want the function's own error", err)
		}
		if _, err := s.GetResourcePlan(ctx, "resourcePlans/aaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the plan was written: %v", err)
		}
		if rs, _ := s.ListResources(ctx, "", ""); len(rs) != 0 {
			t.Errorf("the resource was written: %v", rs)
		}
	})

	t.Run("resources are put, listed by name, replaced and deleted", func(t *testing.T) {
		s := open(t)
		err := s.WriteResources(ctx, func(tx ResourceTx) error {
			for _, r := range []*forgev1.Resource{
				res("payments", "staging", "main", forgev1.Resource_STATE_READY),
				res("payments", "production", "main", forgev1.Resource_STATE_APPLYING),
				res("ledger", "staging", "main", forgev1.Resource_STATE_READY),
			} {
				if err := tx.PutResource(ctx, r); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		list := func(service, env string) string {
			rs, err := s.ListResources(ctx, service, env)
			if err != nil {
				t.Fatal(err)
			}
			out := ""
			for _, r := range rs {
				out += r.GetName() + " "
			}
			return out
		}
		if got := list("", ""); got != "resources/ledger/staging/main resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("all: %q", got)
		}
		if got := list("services/payments", ""); got != "resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("a service: %q", got)
		}
		if got := list("services/payments", "staging"); got != "resources/payments/staging/main " {
			t.Errorf("an environment: %q", got)
		}
		want := res("payments", "staging", "main", forgev1.Resource_STATE_READY)
		got, err := s.GetResource(ctx, want.GetName())
		if err != nil || !proto.Equal(got, want) {
			t.Errorf("get: %v %v", got, err)
		}
		err = s.WriteResources(ctx, func(tx ResourceTx) error {
			if err := tx.PutResource(ctx, res("payments", "staging", "main", forgev1.Resource_STATE_FAILED)); err != nil {
				return err
			}
			if r, err := tx.GetResource(ctx, want.GetName()); err != nil || r.GetState() != forgev1.Resource_STATE_FAILED {
				return fmt.Errorf("read own write: %v %w", r, err)
			}
			if err := tx.DeleteResource(ctx, "resources/ledger/staging/main"); err != nil {
				return err
			}
			return tx.DeleteResource(ctx, "resources/none/staging/main") // deleting nothing is fine
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := list("", ""); got != "resources/payments/production/main resources/payments/staging/main " {
			t.Errorf("after the delete: %q", got)
		}
		if _, err := s.GetResource(ctx, "resources/ledger/staging/main"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a deleted resource: %v", err)
		}
	})

	t.Run("state counts writes and honours locks", func(t *testing.T) {
		s := open(t)
		const key = "payments/staging"
		if st, err := s.GetResourceState(ctx, key); err != nil || st.Version != 0 || st.Data != nil || st.LockID != "" {
			t.Fatalf("no state yet: %+v %v", st, err)
		}
		if holder, err := s.LockResourceState(ctx, key, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil || holder != nil {
			t.Fatalf("first lock: %s %v", holder, err)
		}
		// Locking before the first write makes no version.
		if st, _ := s.GetResourceState(ctx, key); st.Version != 0 || st.LockID != "lock-1" {
			t.Errorf("locked, unwritten: %+v", st)
		}
		// The same id may lock again; another id is told who holds it.
		if _, err := s.LockResourceState(ctx, key, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil {
			t.Errorf("relock by the holder: %v", err)
		}
		holder, err := s.LockResourceState(ctx, key, "lock-2", []byte(`{"ID":"lock-2"}`))
		if !errors.Is(err, ErrStateLocked) || string(holder) != `{"ID":"lock-1"}` {
			t.Errorf("second lock: %s %v", holder, err)
		}
		if _, err := s.PutResourceState(ctx, key, "lock-2", []byte("x")); !errors.Is(err, ErrStateLocked) {
			t.Errorf("a write under another lock: %v", err)
		}
		if _, err := s.PutResourceState(ctx, key, "", []byte("x")); !errors.Is(err, ErrStateLocked) {
			t.Errorf("a write with no lock id while locked: %v", err)
		}
		v1, err := s.PutResourceState(ctx, key, "lock-1", []byte("one"))
		if err != nil || v1 != 1 {
			t.Fatalf("first write: %d %v", v1, err)
		}
		v2, err := s.PutResourceState(ctx, key, "lock-1", []byte("two"))
		if err != nil || v2 != 2 {
			t.Fatalf("second write: %d %v", v2, err)
		}
		if st, _ := s.GetResourceState(ctx, key); string(st.Data) != "two" || st.Version != 2 || st.LockID != "lock-1" {
			t.Errorf("after two writes: %+v", st)
		}
		if err := s.UnlockResourceState(ctx, key, "lock-2"); !errors.Is(err, ErrStateLocked) {
			t.Errorf("an unlock by another id: %v", err)
		}
		if err := s.UnlockResourceState(ctx, key, "lock-1"); err != nil {
			t.Fatal(err)
		}
		if st, _ := s.GetResourceState(ctx, key); st.LockID != "" || st.LockInfo != nil || st.Version != 2 {
			t.Errorf("after the unlock: %+v", st)
		}
		// Unlocked: a write needs no lock id, and "" force-releases any lock.
		if v, err := s.PutResourceState(ctx, key, "", []byte("three")); err != nil || v != 3 {
			t.Errorf("an unlocked write: %d %v", v, err)
		}
		if _, err := s.LockResourceState(ctx, key, "lock-3", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.UnlockResourceState(ctx, key, ""); err != nil {
			t.Errorf("a forced unlock: %v", err)
		}
		if err := s.UnlockResourceState(ctx, "ledger/staging", "lock-9"); err != nil {
			t.Errorf("unlocking a state that does not exist: %v", err)
		}
		if _, err := s.LockResourceState(ctx, key, "", nil); err == nil {
			t.Error("a lock with no id must fail")
		}
		// Keys are separate.
		if st, _ := s.GetResourceState(ctx, "payments/production"); st.Version != 0 {
			t.Errorf("another key: %+v", st)
		}
	})

	t.Run("a first write does not get past a lock taken just before it", func(t *testing.T) {
		s := open(t)
		for i := range 20 {
			key := fmt.Sprintf("race/%d", i)
			var lockErr, putErr error
			var wg sync.WaitGroup
			wg.Go(func() { _, lockErr = s.LockResourceState(ctx, key, "lock-A", []byte(`{"ID":"lock-A"}`)) })
			wg.Go(func() { _, putErr = s.PutResourceState(ctx, key, "lock-B", []byte("x")) })
			wg.Wait()
			if lockErr != nil {
				t.Fatalf("round %d: lock: %v", i, lockErr)
			}
			if putErr != nil && !errors.Is(putErr, ErrStateLocked) {
				t.Fatalf("round %d: put: %v", i, putErr)
			}
			st, err := s.GetResourceState(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			wrote := putErr == nil
			if wrote != (st.Version == 1) || wrote != (string(st.Data) == "x") {
				t.Errorf("round %d: put err %v, state version %d data %q", i, putErr, st.Version, st.Data)
			}
		}
	})
}
