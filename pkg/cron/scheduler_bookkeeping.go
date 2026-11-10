package cron

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const emptyResponseError = "Agent completed but produced empty response (model error, timeout, or misconfiguration)"

// A successful run with a blank answer becomes the soft empty response failure
func failEmptyResponse(d *runDelivery, finalResponse string) {
	if d.success && pystr.Strip(finalResponse) == "" {
		d.success = false
		d.err = strPtr(emptyResponseError)
	}
}

// Shutdown already advanced this run, records the failed notice without spending another fire
func (s *Store) finishInterruptedRun(ctx context.Context, job *Job, executionID string, deliveryError *string) error {
	if deliveryError != nil && *deliveryError != "" {
		if _, err := s.updateJob(home.WithOverride(ctx, s.home), job.ID, map[string]any{"last_delivery_error": *deliveryError}); err != nil {
			s.log.Debug(fmt.Sprintf("Failed recording delivery_error for interrupted job %s: %v", job.ID, err))
		}
	}
	_, err := openExecutionsLedger(s).Finish(ctx, executionID, false, strPtr("Interrupted by gateway shutdown before terminal completion."), nil, false)
	return err
}

// Ledger delivery outcome of a finished run from what its notice did
func classifyDeliveryOutcome(deliveryError *string, shouldDeliver, unresolvedOrigin bool, normalizedDeliver string, incidentAcked, success bool, deliveryQueued any, notificationSuppressed bool) string {
	switch {
	case deliveryError != nil && *deliveryError != "":
		return "failed"
	case shouldDeliver && pyTruthy(deliveryQueued):
		return "queued"
	case notificationSuppressed:
		return "suppressed"
	case shouldDeliver && unresolvedOrigin:
		return "not_configured"
	case shouldDeliver && normalizedDeliver != "local":
		return "delivered"
	case incidentAcked && !success:
		return "suppressed_acked"
	}
	return "suppressed"
}

// Owner fenced mark plus the ledger row for a run that reached delivery, false while a foreign firer owns the row
func (s *Store) finishCompletedRun(ctx context.Context, d *runDelivery, fireOwner *string, executionID string, recovered bool) (bool, error) {
	job := d.job
	if !d.shouldDeliver && pyTruthy(job.LastDeliveryQueued) {
		if _, err := s.updateJob(home.WithOverride(ctx, s.home), job.ID, map[string]any{"last_delivery_queued": nil}); err != nil {
			return false, err
		}
		job.LastDeliveryQueued = nil
	}
	opts := MarkOptions{Error: d.err, DeliveryError: d.deliveryError}
	journal := s.journalPath(job.ID, executionID)
	if !job.NoAgent {
		opts.ExecutionID = strPtr(executionID)
	}
	if !d.success && job.run.modelUnreachable {
		opts.ModelUnreachable = true
	}
	job.run.modelUnreachable = false
	if unreachableRetryIsRetryRun(s, job) {
		opts.LadderRung = true
	}
	hold := job.run.quotaHoldSeconds
	job.run.quotaHoldSeconds = 0
	if !d.success && hold != 0 {
		opts.QuotaHoldSeconds = hold
		opts.RecoverConsumedFire = job.ScheduledInstant != nil && *job.ScheduledInstant != ""
	}
	if d.success && d.deliveryError == nil && d.shouldDeliver && pyTruthy(job.LastDeliveryQueued) {
		opts.Status = strPtr("delivery_queued")
	}
	if fireOwner != nil {
		opts.ExpectedFireOwner = fireOwner
	}
	if d.blockedConfig {
		opts.Status = strPtr("blocked_config")
	}
	ledger := openExecutionsLedger(s)
	marked, err := s.selfRemovalDeliveryAllowed(ctx, nil, job.ID)
	if err != nil {
		return false, err
	}
	if !marked {
		if marked, err = s.markJobRun(ctx, job.ID, d.success, opts); err != nil {
			return false, err
		}
	}
	if fireOwner != nil && !marked {
		_, err := ledger.Finish(ctx, executionID, false, strPtr("Fire claim ownership lost before terminal completion."), nil, false)
		return true, err
	}
	outcome := classifyDeliveryOutcome(d.deliveryError, d.shouldDeliver, d.unresolvedOrigin,
		normalizeDeliverValue(deliveryLaneValue(job, !d.success)), d.incidentAcked, d.success,
		job.LastDeliveryQueued, job.run.notificationAllTargetsSuppressed)
	if outcome == "queued" {
		settled, err := s.settledOutcome(ctx, executionID)
		if err != nil {
			return false, err
		}
		if settled != nil && *settled != "" {
			outcome = *settled
		}
	}
	if (outcome == "delivered" || outcome == "not_configured") && !d.success {
		s.markIncidentAlerted(d.failureIncidentID)
	}
	var finished map[string]any
	if recovered {
		finished, err = ledger.RecoverReceipted(ctx, executionID, job.ID, d.success, d.err, &outcome)
	} else {
		finished, err = ledger.Finish(ctx, executionID, d.success, d.err, &outcome, false)
	}
	if err != nil {
		return false, err
	}
	if recovered && finished == nil {
		current, err := ledger.Get(ctx, executionID)
		if err != nil {
			return false, err
		}
		if current != nil && !FirerFinishedStates[pyStrOrEmpty(current["status"])] {
			return false, nil
		}
	}
	if pyTruthy(job.LastDeliveryQueued) {
		s.settleQuietly(ctx, job.ID, executionID)
	}
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}
