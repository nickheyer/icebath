package cron

import (
	"context"
	"fmt"
)

var ledgerDeliveryOutcome = map[string]string{"delivered": "delivered", "suppressed": "suppressed", "failed": "failed", "unknown": "failed"}

// Stamps the outcome onto the job only while the execution is its latest attempt and completion
func (s *Store) settleJobRecord(ctx context.Context, jobID, executionID string, deliveryError *string) (bool, error) {
	latest, err := openExecutionsLedger(s).Latest(ctx, jobID)
	if err != nil {
		return false, err
	}
	if latest == nil || latest["id"] != executionID {
		return false, nil
	}
	applied := false
	err = s.withJobsLock(func(tx *jobsTx) error {
		jobs, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.ID != jobID {
				continue
			}
			n := len(job.CanonicalCompletions)
			if n == 0 || job.CanonicalCompletions[n-1] != executionID {
				return nil
			}
			applied = true
			status := job.LastStatus
			if status != nil && *status == "delivery_queued" {
				next := "ok"
				if deliveryError != nil && *deliveryError != "" {
					next = "delivery_failed"
				}
				status = &next
			}
			if optEqual(status, job.LastStatus) && optEqual(deliveryError, job.LastDeliveryError) {
				return nil
			}
			job.LastStatus, job.LastDeliveryError = status, deliveryError
			return s.saveJobsLocked(tx, jobs, nil, false)
		}
		return nil
	})
	return applied, err
}

// Equality of two optional strings, nil equal only to nil
func optEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Ledger outcome and delivery error once the queue row is terminal, ok false while in flight
func (s *Store) terminalDelivery(ctx context.Context, executionID string) (string, *string, bool, error) {
	row, err := openDeliveryQueue(s).GetStatus(ctx, executionID)
	if err != nil || row == nil {
		return "", nil, false, err
	}
	status := recText(row, "status")
	outcome, known := ledgerDeliveryOutcome[status]
	if !known {
		return "", nil, false, nil
	}
	var deliveryError *string
	if status == "failed" || status == "unknown" {
		text := recText(row, "error")
		if text == "" {
			text = "delivery " + status
		}
		deliveryError = &text
	}
	return outcome, deliveryError, true, nil
}

// Ledger outcome a drain already reached for the execution, nil while queued
func (s *Store) settledOutcome(ctx context.Context, executionID string) (*string, error) {
	outcome, _, ok, err := s.terminalDelivery(ctx, executionID)
	if err != nil || !ok {
		return nil, err
	}
	return &outcome, nil
}

// Applies the queue's terminal outcome to the ledger and job, false while still in flight
func (s *Store) settleQueuedDelivery(ctx context.Context, jobID, executionID string) (bool, error) {
	if jobID == "" || executionID == "" {
		return false, nil
	}
	outcome, deliveryError, ok, err := s.terminalDelivery(ctx, executionID)
	if err != nil || !ok {
		return false, err
	}
	if _, err := openExecutionsLedger(s).SettleDeliveryOutcome(ctx, executionID, outcome); err != nil {
		return false, err
	}
	if _, err := s.settleJobRecord(ctx, jobID, executionID, deliveryError); err != nil {
		return false, err
	}
	return true, nil
}

// Settles the outcome and logs a bookkeeping failure without failing the run or the send
func (s *Store) settleQuietly(ctx context.Context, jobID, executionID string) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Warn(fmt.Sprintf("Cron delivery %s: outcome write-back failed: %v", executionID, r))
			}
		}()
		if _, err := s.settleQueuedDelivery(ctx, jobID, executionID); err != nil {
			s.log.Warn(fmt.Sprintf("Cron delivery %s: outcome write-back failed: %v", executionID, err))
		}
	}()
}
