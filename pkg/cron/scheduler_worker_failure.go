package cron

import (
	"context"
	"fmt"
)

// Projects a recovered worker death's unknown outcome onto the job once, fenced against newer fires
func (s *Store) recordUnknownWorkerOutcome(ctx context.Context, job *Job, errText *string, adapters, loop any) (bool, error) {
	executionID := ""
	if job.ExecutionID != nil {
		executionID = *job.ExecutionID
	}
	execution, err := getExecution(ctx, s, executionID)
	if err != nil {
		return false, err
	}
	if execution == nil || pyStrOrEmpty(execution["status"]) != "unknown" {
		return false, nil
	}
	owner := ""
	if job.FireClaim != nil {
		owner = job.FireClaim.By
	}
	if owner == "" {
		return false, nil
	}
	err = s.fireClaimFence(ctx, job.ID, owner, func(ctx context.Context, owns bool) error {
		if !owns {
			return nil
		}
		text := ""
		if errText != nil && *errText != "" {
			text = *errText
		} else {
			text = pyStrOrEmpty(execution["error"])
		}
		s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, text))
		var deliveryError *string
		scopeCtx := s.installFireSecretScope(ctx)
		deliveryError, _ = deliverCrashFailureSeam(s, scopeCtx, job, text, adapters, loop)
		_, markErr := markJobRunSeam(s, ctx, job.ID, false, MarkOptions{
			Error: &text, DeliveryError: deliveryError, ExpectedFireOwner: &owner,
			LadderRung: unreachableRetryIsRetryRun(s, job),
		})
		return markErr
	})
	return true, err
}

// Cause for an attempt whose restart safe worker this process watched exit
func externalWorkerExitedReason(returncode *int) string {
	status := "None"
	if returncode != nil {
		status = fmt.Sprint(*returncode)
	}
	return fmt.Sprintf("Restart-safe cron worker exited with status %s after adopting this execution, without writing a durable terminal state; whether side effects ran is unknown.", status)
}
