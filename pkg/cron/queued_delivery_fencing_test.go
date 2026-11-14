package cron

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Real target resolution and delivery for one case in place of the package recorders
func realDeliverySeams(t *testing.T) {
	t.Helper()
	targets, deliver := resolveDeliveryTargets, deliverResultSeam
	t.Cleanup(func() { resolveDeliveryTargets, deliverResultSeam = targets, deliver })
	resolveDeliveryTargets, deliverResultSeam = realResolveDeliveryTargets, realDeliverResult
}

func TestClaimStolenAfterTheOwnershipSampleQueuesNoGhostSend(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	realDeliverySeams(t)
	patchRunJob(t, jobRunOutcome{Success: true, Output: "raw", FinalResponse: "the result"}, nil)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h", Deliver: prompt("telegram:fixture")})
	fire, ok, err := s.claimJobForFire(ctx, job.ID, FireClaimOptions{Force: true})
	if err != nil || !ok || fire == nil || fire.FireClaim == nil {
		t.Fatalf("claim = %v %v %v", fire, ok, err)
	}
	var samples []bool
	real := fireOwnershipLost
	t.Cleanup(func() { fireOwnershipLost = real })
	fireOwnershipLost = func(f *fireOwnership, sampleCtx context.Context) bool {
		verdict := real(f, sampleCtx)
		samples = append(samples, verdict)
		if len(samples) == 2 {
			if _, err := s.updateJob(sampleCtx, job.ID, map[string]any{"fire_claim": map[string]any{"at": fire.FireClaim.At, "by": "replacement"}}); err != nil {
				t.Error(err)
			}
		}
		return verdict
	}
	if _, err := runOneJob(ctx, s, fire, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	execution := latestExecution(t, s, job.ID)
	if len(samples) < 2 || samples[0] || samples[1] {
		t.Fatalf("samples = %v", samples)
	}
	if status, err := openDeliveryQueue(s).GetStatus(ctx, execution["id"].(string)); err != nil || status != nil {
		t.Fatalf("queued = %v, %v", status, err)
	}
	if execution["status"] != "failed" {
		t.Fatalf("status = %v", execution["status"])
	}
}

func TestDeadDrainOwnerSettlesTheRunItFencedUnknown(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	realDeliverySeams(t)
	patchRunJob(t, jobRunOutcome{Success: true, Output: "raw", FinalResponse: "the result"}, nil)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h", Deliver: prompt("telegram:fixture")})
	if _, err := runOneJob(ctx, s, job, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	execution := latestExecution(t, s, job.ID)
	if status := deref(mustGet(t, s, job.ID).LastStatus); status != "delivery_queued" {
		t.Fatalf("last_status = %s", status)
	}
	q := openDeliveryQueue(s)
	claimed, err := q.ClaimNext(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	deliveryMu.Lock()
	delete(activeDeliveries, claimed["execution_id"].(string))
	deliveryMu.Unlock()
	if err := q.transaction(ctx, true, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE deliveries SET owner_process_id='gone', owner_pid=999999 WHERE execution_id=?", execution["id"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sent []recordedSend
	if _, err := q.Drain(ctx, recorder(&sent, nil), 0); err != nil {
		t.Fatal(err)
	}
	saved := mustGet(t, s, job.ID)
	if len(sent) != 0 {
		t.Fatalf("an uncertain send is never retried: %v", sent)
	}
	if deref(saved.LastStatus) != "delivery_failed" || !strings.Contains(deref(saved.LastDeliveryError), "unknown") {
		t.Fatalf("saved = %q %q", deref(saved.LastStatus), deref(saved.LastDeliveryError))
	}
	if row := executionRow(t, s, execution["id"].(string)); row["delivery_outcome"] != "failed" {
		t.Fatalf("delivery_outcome = %v", row["delivery_outcome"])
	}
}

func TestOriginWithNoResolvableTargetIsNotConfiguredNeverDelivered(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	realDeliverySeams(t)
	patchRunJob(t, jobRunOutcome{Success: true, Output: "raw", FinalResponse: "the result"}, nil)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h", Deliver: prompt("origin")})
	if _, err := runOneJob(ctx, s, job, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	execution := latestExecution(t, s, job.ID)
	if status, err := openDeliveryQueue(s).GetStatus(ctx, execution["id"].(string)); err != nil || status != nil {
		t.Fatalf("no target: nothing is queued, got %v, %v", status, err)
	}
	if execution["delivery_outcome"] != "not_configured" {
		t.Fatalf("delivery_outcome = %v", execution["delivery_outcome"])
	}
	if status := deref(mustGet(t, s, job.ID).LastStatus); status != "ok" {
		t.Fatalf("last_status = %s", status)
	}
}
