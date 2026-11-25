package cron

import (
	"context"
	"fmt"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	if execution == nil || anyutil.StringOr(execution["status"], "") != "unknown" {
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
			text = anyutil.StringOr(execution["error"], "")
		}
		if text == "" {
			text = "the run ended in an unknown state with no recorded error"
		}
		s.log.Error(fmt.Sprintf("Job %q: %s", job.ID, text))
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
	status := "an unknown status"
	if returncode != nil {
		status = fmt.Sprintf("status %d", *returncode)
	}
	return fmt.Sprintf("Restart-safe cron worker exited with %s after adopting this execution, without writing a durable terminal state; whether side effects ran is unknown.", status)
}
