package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// runSuite checks a Store. open returns an empty store for one subtest.
func runSuite(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc := func(id, team, repo string, deps ...string) *forgev1.Service {
		return &forgev1.Service{
			Name: "services/" + id, OwnerTeam: team, RepoUrl: repo, Dependencies: deps,
			CreateTime: timestamppb.New(at), UpdateTime: timestamppb.New(at), Etag: `W/"1"`,
		}
	}
	put := func(t *testing.T, s Store, svcs ...*forgev1.Service) {
		t.Helper()
		if err := s.Write(ctx, func(tx Tx) error {
			for _, v := range svcs {
				if err := tx.Put(ctx, v, 1); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(svcs []*forgev1.Service) []string {
		var out []string
		for _, v := range svcs {
			out = append(out, v.GetName())
		}
		return out
	}

	t.Run("get missing", func(t *testing.T) {
		s := open(t)
		if _, _, err := s.Get(ctx, "services/nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("round trip", func(t *testing.T) {
		s := open(t)
		in := svc("payments", "billing", "https://github.com/showmino/payments", "services/ledger")
		in.DisplayName, in.Owners, in.Lifecycle = "Payments", []string{"ana@example.com"}, forgev1.Lifecycle_LIFECYCLE_PRODUCTION
		in.Labels = map[string]string{"tier": "1"}
		put(t, s, in)
		got, rev, err := s.Get(ctx, "services/payments")
		if err != nil || rev != 1 || !proto.Equal(got, in) {
			t.Fatalf("got %v rev %d err %v, want %v", got, rev, err, in)
		}
		got.OwnerTeam = "changed" // the store keeps its own copy
		again, _, _ := s.Get(ctx, "services/payments")
		if again.GetOwnerTeam() != "billing" {
			t.Fatal("the store shares its service with the caller")
		}
	})

	t.Run("list, page and filter", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("a", "x", ""), svc("b", "y", ""), svc("c", "x", ""), svc("d", "x", ""), svc("e", "y", ""))
		got, err := s.List(ctx, ListQuery{Limit: 2})
		if err != nil || !slices.Equal(names(got), []string{"services/a", "services/b"}) {
			t.Fatalf("first page %v %v", names(got), err)
		}
		got, _ = s.List(ctx, ListQuery{After: "services/b", Limit: 10})
		if !slices.Equal(names(got), []string{"services/c", "services/d", "services/e"}) {
			t.Fatalf("after b %v", names(got))
		}
		got, _ = s.List(ctx, ListQuery{OwnerTeam: "x", After: "services/a", Limit: 10})
		if !slices.Equal(names(got), []string{"services/c", "services/d"}) {
			t.Fatalf("team x after a %v", names(got))
		}
		all, err := s.Names(ctx)
		if err != nil || !slices.Equal(all, []string{"services/a", "services/b", "services/c", "services/d", "services/e"}) {
			t.Fatalf("names %v %v", all, err)
		}
	})

	t.Run("repo lookup", func(t *testing.T) {
		s := open(t)
		put(t, s,
			svc("payments", "b", "https://github.com/ShowMino/Payments.git"),
			svc("twin-b", "b", "https://github.com/showmino/twin/"),
			svc("twin-a", "b", "https://github.com/showmino/twin"),
			svc("other", "b", "https://github.com/acme/payments"),
			svc("gitlab", "b", "https://gitlab.com/showmino/payments"),
			svc("none", "b", ""),
		)
		for _, tc := range []struct {
			owner, repo string
			want        []string
		}{
			{"showmino", "payments", []string{"services/payments"}},
			{"SHOWMINO", "PAYMENTS", []string{"services/payments"}},
			{"showmino", "twin", []string{"services/twin-a", "services/twin-b"}},
			{"showmino", "nope", nil},
		} {
			got, err := s.ForRepo(ctx, tc.owner, tc.repo)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ForRepo(%s, %s) = %v %v, want %v", tc.owner, tc.repo, got, err, tc.want)
			}
		}
		moved := svc("payments", "b", "https://github.com/showmino/payments-v2")
		if err := s.Write(ctx, func(tx Tx) error { return tx.Put(ctx, moved, 2) }); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.ForRepo(ctx, "showmino", "payments"); len(got) != 0 {
			t.Errorf("old repo still maps to %v", got)
		}
		if got, _ := s.ForRepo(ctx, "showmino", "payments-v2"); !slices.Equal(got, []string{"services/payments"}) {
			t.Errorf("new repo maps to %v", got)
		}

		// Inside a write, the lookup sees the write's own changes.
		err := s.Write(ctx, func(tx Tx) error {
			if err := tx.Put(ctx, svc("twin-c", "b", "https://github.com/showmino/Twin"), 1); err != nil {
				return err
			}
			if err := tx.Delete(ctx, "services/twin-a"); err != nil {
				return err
			}
			if err := tx.Put(ctx, svc("payments", "b", "https://github.com/showmino/twin"), 3); err != nil {
				return err
			}
			got, err := tx.ForRepo(ctx, "showmino", "twin")
			if want := []string{"services/payments", "services/twin-b", "services/twin-c"}; err != nil || !slices.Equal(got, want) {
				t.Errorf("tx.ForRepo = %v %v, want %v", got, err, want)
			}
			got, err = tx.ForRepo(ctx, "showmino", "payments-v2")
			if err != nil || len(got) != 0 {
				t.Errorf("tx.ForRepo(payments-v2) = %v %v, want none", got, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("update and delete", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("a", "x", ""))
		next := svc("a", "y", "")
		next.Etag = `W/"2"`
		if err := s.Write(ctx, func(tx Tx) error { return tx.Put(ctx, next, 2) }); err != nil {
			t.Fatal(err)
		}
		got, rev, _ := s.Get(ctx, "services/a")
		if rev != 2 || got.GetOwnerTeam() != "y" || got.GetEtag() != `W/"2"` {
			t.Fatalf("after update: %v rev %d", got, rev)
		}
		if err := s.Write(ctx, func(tx Tx) error { return tx.Delete(ctx, "services/a") }); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})

	t.Run("dependents see the transaction's writes", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("b", "x", ""), svc("a", "x", "", "services/b"), svc("c", "x", "", "services/b", "services/a"))
		if err := s.Write(ctx, func(tx Tx) error {
			got, err := tx.Dependents(ctx, "services/b")
			if err != nil || !slices.Equal(got, []string{"services/a", "services/c"}) {
				return fmt.Errorf("dependents of b: %v %w", got, err)
			}
			if err := tx.Delete(ctx, "services/a"); err != nil {
				return err
			}
			got, err = tx.Dependents(ctx, "services/b")
			if err != nil || !slices.Equal(got, []string{"services/c"}) {
				return fmt.Errorf("after deleting a: %v %w", got, err)
			}
			if _, _, err := tx.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("tx get of deleted a: %w", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failed write changes nothing", func(t *testing.T) {
		s := open(t)
		boom := errors.New("boom")
		err := s.Write(ctx, func(tx Tx) error {
			if err := tx.Put(ctx, svc("a", "x", ""), 1); err != nil {
				return err
			}
			if err := tx.Remember(ctx, "req-1", svc("a", "x", "")); err != nil {
				return err
			}
			return boom
		})
		if err != boom { //nolint:errorlint // Write must return fn's error itself
			t.Fatalf("Write returned %v, want fn's error unchanged", err)
		}
		if _, _, err := s.Get(ctx, "services/a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back put is visible: %v", err)
		}
		_ = s.Write(ctx, func(tx Tx) error {
			if _, err := tx.Recall(ctx, "req-1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("rolled-back remember is visible: %v", err)
			}
			return nil
		})
	})

	t.Run("request IDs", func(t *testing.T) {
		s := open(t)
		want := svc("a", "x", "")
		if err := s.Write(ctx, func(tx Tx) error {
			if _, err := tx.Recall(ctx, "req-1"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("recall before remember: %w", err)
			}
			return tx.Remember(ctx, "req-1", want)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Write(ctx, func(tx Tx) error {
			got, err := tx.Recall(ctx, "req-1")
			if err != nil || !proto.Equal(got, want) {
				return fmt.Errorf("recall: %v %w", got, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("writes do not overlap", func(t *testing.T) {
		s := open(t)
		put(t, s, svc("counter", "x", ""))
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := s.Write(ctx, func(tx Tx) error {
					cur, rev, err := tx.Get(ctx, "services/counter")
					if err != nil {
						return err
					}
					return tx.Put(ctx, cur, rev+1)
				}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if _, rev, _ := s.Get(ctx, "services/counter"); rev != 21 {
			t.Fatalf("revision %d after 20 increments from 1, want 21 (lost updates)", rev)
		}
	})
}
