package server

import (
	"context"
	"time"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

// RunSweeper sweeps now and then every interval, until ctx ends.
func (r *Resources) RunSweeper(ctx context.Context, every time.Duration) {
	if r == nil {
		return
	}
	r.Sweep(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep ends the runs that passed their deadline with nobody watching them
// (forge-api restarted, or the runner never reported), and marks the plans
// nobody applied within their time as expired.
func (r *Resources) Sweep(ctx context.Context) {
	if r == nil {
		return
	}
	states := []forgev1.ResourcePlan_State{
		forgev1.ResourcePlan_STATE_PLANNING, forgev1.ResourcePlan_STATE_APPLYING, forgev1.ResourcePlan_STATE_PLANNED,
	}
	var before int64
	for {
		recs, err := r.o.Store.ListResourcePlans(ctx, store.PlanQuery{Limit: r.o.SweepPage, BeforeSeq: before, States: states})
		if err != nil {
			r.o.Log.Error("resource sweep: listing plans", "error", err)
			return
		}
		r.sweepPage(ctx, recs)
		if len(recs) < r.o.SweepPage {
			return
		}
		before = recs[len(recs)-1].Seq
	}
}

func (r *Resources) sweepPage(ctx context.Context, recs []*store.PlanRecord) {
	now := r.o.Now()
	for _, rec := range recs {
		name := rec.Plan.GetName()
		mode := modeOf(rec.Plan.GetState())
		switch {
		case mode != "":
			// A run an operation of this process watches is ended by that operation.
			if now.Before(rec.Deadline) || r.isLive(name) {
				continue
			}
			won, err := r.endRun(ctx, name, mode, runEnd{code: opCodeRunTimeout, message: "the run did not finish before its deadline"})
			if err != nil {
				r.o.Log.Error("resource sweep: ending a run", "plan", name, "error", err)
			} else if won {
				// The runner may have outlived forge-api; stop it like watchRun does.
				if err := r.o.Launcher.Stop(context.WithoutCancel(ctx), name, mode); err != nil {
					r.o.Log.Warn("stopping a resource runner", "plan", name, "mode", mode, "error", err)
				}
			}
		case !now.Before(rec.Plan.GetExpireTime().AsTime()):
			if err := r.expire(ctx, name); err != nil {
				r.o.Log.Error("resource sweep: expiring a plan", "plan", name, "error", err)
			}
		}
	}
}

// isLive reports whether an operation of this process is watching plan's run.
func (r *Resources) isLive(planName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[planName]
	return ok
}

// expire marks a plan nobody applied in time. Its plan file goes; its
// summary and policy result stay.
func (r *Resources) expire(ctx context.Context, planName string) error {
	return r.o.Store.WriteResources(ctx, func(tx store.ResourceTx) error {
		cur, err := tx.GetResourcePlan(ctx, planName)
		if err != nil {
			return err
		}
		if cur.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			return nil
		}
		cur.Plan.State = forgev1.ResourcePlan_STATE_EXPIRED
		cur.PlanFile, cur.PlanJSON = nil, nil
		return tx.PutResourcePlan(ctx, cur)
	})
}
