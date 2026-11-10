package cron

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// MarkOptions carries the optional outcome fields of one finished run
type MarkOptions struct {
	Error               *string
	DeliveryError       *string
	Status              *string
	ExpectedFireOwner   *string
	ExecutionID         *string
	ModelUnreachable    bool
	LadderRung          bool
	QuotaHoldSeconds    float64
	RecoverConsumedFire bool
}

// FireClaimOptions tunes one external fire claim
type FireClaimOptions struct {
	// Claim age past which an existing claim is stale, the fire claim TTL when nil
	TTLSeconds *float64
	Force      bool
	Manual     bool
}

// Stamps one completed run onto the record with status, failure streak, alert markers and cleared claims
func recordRunOutcome(job *Job, success bool, errText, deliveryError, status *string, now string) {
	job.LastRunAt = strPtr(now)
	job.ManualRunAt, job.ManualRunRestoreAt, job.ManualRunPrompt = nil, nil, nil
	deliveryFailed := deliveryError != nil && pystr.Strip(*deliveryError) != ""
	switch {
	case deref(status) != "":
		job.LastStatus = strPtr(*status)
	case !success:
		job.LastStatus = strPtr("error")
	case deliveryFailed:
		job.LastStatus = strPtr("delivery_failed")
	default:
		job.LastStatus = strPtr("ok")
	}
	cancelled := deref(status) == "cancelled"
	if success || cancelled {
		job.LastError = nil
	} else {
		job.LastError = errText
	}
	if success {
		job.PreflightAlerted = false
		job.LastFireError = nil
		job.FailureStreak = 0
	} else if !cancelled {
		job.FailureStreak++
		detail := deref(errText)
		if detail == "" {
			detail = deref(status)
		}
		if detail == "" {
			detail = "run failed"
		}
		job.LastFailure = &Stamp{At: now, Detail: detail}
	}
	job.LastDeliveryError = deliveryError
	job.FireClaim = nil
	job.PendingSlot = nil
	job.RunClaim = nil
}

// Bumps the repeat count and recomputes the next run, retiring the record when nothing remains
func (s *Store) advanceAfterRun(job *Job, now string, ladderRung bool) error {
	kind := job.Kind()
	if repeat := job.Repeat; repeat != nil {
		finite := repeat.Times != nil && *repeat.Times > 0
		completed := repeat.Completed
		if !ladderRung && !(kind == "once" && finite && completed > 0) {
			completed++
			repeat.Completed = completed
		}
		if finite && completed >= *repeat.Times {
			completeJobRecord(job)
			return nil
		}
	}
	next, err := s.computeNextRun(job.scheduleOrEmpty(), &now, job.Timezone)
	if err != nil {
		return err
	}
	job.NextRunAt = next
	switch {
	case next != nil:
		if job.State != "paused" {
			job.State = "scheduled"
		}
	case kind == "cron" || kind == "interval":
		job.State = "error"
		if deref(job.LastError) == "" {
			job.LastError = strPtr("Failed to compute next run for recurring schedule (is the 'croniter' package " +
				"installed in the gateway's Python env?)")
		}
		s.log.Error(fmt.Sprintf("Job '%s' (%s) could not compute next_run_at; leaving enabled and marking "+
			"state=error so the job is not silently disabled.", job.Label(), kind))
	default:
		completeJobRecord(job)
	}
	return nil
}

// Records a finished run under the fire fence, false when the fence, the job or the claim owner is lost
func (s *Store) markJobRun(ctx context.Context, jobID string, success bool, o MarkOptions) (bool, error) {
	return s.underFireFence(ctx, jobID, func(ctx context.Context) (bool, error) {
		ok, found, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
			if o.ExecutionID != nil && containsString(job.CanonicalCompletions, *o.ExecutionID) {
				return true, nil
			}
			if o.ExpectedFireOwner != nil && (job.FireClaim == nil || job.FireClaim.By != *o.ExpectedFireOwner) {
				s.log.Warn(fmt.Sprintf("mark_job_run: job_id %s fire claim owner changed; discarding stale completion", jobID))
				return false, nil
			}
			now := s.clk.nowISO()
			extra := extraFireIsExtra(job)
			var held map[string]any
			if extra {
				held = extraFireHeldMarkers(job)
			}
			recordRunOutcome(job, success, o.Error, o.DeliveryError, o.Status, now)
			if err := setJobFields(job, held); err != nil {
				return false, err
			}
			if o.ExecutionID != nil {
				job.CanonicalCompletions = append(replayableCompletions(s, jobID, job.CanonicalCompletions), *o.ExecutionID)
			}
			if extra {
				if err := extraFireSettle(s, job, now); err != nil {
					return false, err
				}
			} else if err := s.advanceAfterRun(job, now, o.LadderRung); err != nil {
				return false, err
			}
			if !success && o.ModelUnreachable && !isTerminalJob(job) {
				unreachableRetryPlanRetry(s, job)
			} else {
				unreachableRetryClearState(job)
			}
			if !success && o.QuotaHoldSeconds != 0 && !isTerminalJob(job) {
				if _, err := quotaHoldPlanHold(s, job, o.QuotaHoldSeconds, o.RecoverConsumedFire); err != nil {
					return false, err
				}
			} else {
				quotaHoldClearState(job)
			}
			return true, s.saveJobsLocked(tx, jobs, nil, false)
		})
		if err == nil && !found {
			s.log.Warn(fmt.Sprintf("mark_job_run: job_id %s not found, skipping save", jobID))
		}
		return ok, err
	})
}

// Claims a finite one shot dispatch before execution so a dying tick cannot lose or repeat it
func (s *Store) claimDispatch(ctx context.Context, jobID string) (bool, error) {
	claimed, found, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		repeat := job.Repeat
		if repeat == nil {
			repeat = &Repeat{}
		}
		if job.Kind() != "once" || repeat.Times == nil || *repeat.Times <= 0 {
			return true, nil
		}
		if extraFireIsExtra(job) {
			return true, nil
		}
		times, completed, label := *repeat.Times, repeat.Completed, job.Label()
		if completed >= times {
			if job.LastRunAt != nil {
				completeJobRecord(job)
				if err := s.saveJobsLocked(tx, jobs, nil, false); err != nil {
					return false, err
				}
				s.log.Info(fmt.Sprintf("Job '%s': dispatch limit reached (%d/%d) — marking completed", label, completed, times))
				return false, nil
			}
			kept := append(append([]*Job{}, jobs[:i]...), jobs[i+1:]...)
			if err := s.saveJobsLocked(tx, kept, []string{jobID}, false); err != nil {
				return false, err
			}
			writeWedged(home.WithOverride(ctx, s.home), s, job)
			s.log.Info(fmt.Sprintf("Job '%s': dispatch limit reached (%d/%d) — removing", label, completed, times))
			return false, nil
		}
		repeat.Completed = completed + 1
		job.Repeat = repeat
		if err := s.saveJobsLocked(tx, jobs, nil, false); err != nil {
			return false, err
		}
		s.log.Debug(fmt.Sprintf("Job '%s': claimed dispatch %d/%d", label, repeat.Completed, times))
		return true, nil
	})
	if err == nil && !found {
		s.log.Debug(fmt.Sprintf("claim_dispatch: job_id %s not in store — proceeding without claim "+
			"(handed-in job dict; nothing to persist a claim against)", jobID))
		return true, nil
	}
	return claimed, err
}

// Refreshes a claim stamp only while the expected owner still holds it
func (s *Store) refreshClaim(tx *jobsTx, jobs []*Job, claim *Claim, owner string) (bool, error) {
	if claim == nil || claim.By != owner {
		return false, nil
	}
	claim.At = s.clk.nowISO()
	return true, s.saveJobsLocked(tx, jobs, nil, false)
}

// Keeps a live one shot run claim fresh for its owner
func (s *Store) heartbeatRunClaim(jobID, owner string) (bool, error) {
	ok, _, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		if job.Kind() != "once" {
			return false, nil
		}
		return s.refreshClaim(tx, jobs, job.RunClaim, owner)
	})
	return ok, err
}

// Clears a one shot run claim when dispatch fails, keeping only an outage marker
func (s *Store) clearRunClaim(jobID string) (bool, error) {
	ok, _, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		claim := job.RunClaim
		if job.Kind() != "once" || claim == nil || *claim == (Claim{Outage: true}) {
			return false, nil
		}
		if claim.Outage {
			job.RunClaim = &Claim{Outage: true}
		} else {
			job.RunClaim = nil
		}
		return true, s.saveJobsLocked(tx, jobs, nil, false)
	})
	return ok, err
}

// Advances next_run_at of every listed recurring job with one load and at most one save
func (s *Store) advanceNextRuns(jobIDs []string) (int, error) {
	if len(jobIDs) == 0 {
		return 0, nil
	}
	advanced := 0
	err := s.withJobsLock(func(tx *jobsTx) error {
		jobs, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		now := s.clk.nowISO()
		for _, job := range jobs {
			if !containsString(jobIDs, job.ID) || (isTerminalJob(job) && !isRecoverableErrorJob(job)) ||
				!isRecurring(job) || extraFireQueued(job) {
				continue
			}
			next, err := s.computeNextRun(job.Schedule, &now, job.Timezone)
			if err != nil {
				return err
			}
			if next != nil && *next != "" && (job.NextRunAt == nil || *next != *job.NextRunAt) {
				job.NextRunAt = next
				advanced++
			}
		}
		if advanced > 0 {
			return s.saveJobsLocked(tx, jobs, nil, false)
		}
		return nil
	})
	return advanced, err
}

// Advances one recurring job before it runs so a crash cannot re-fire it
func (s *Store) advanceNextRun(jobID string) (bool, error) {
	n, err := s.advanceNextRuns([]string{jobID})
	return n >= 1, err
}

// Claim attribution id from ICEBATH_MACHINE_ID, else host and pid
func machineID() string {
	if explicit := pystr.Strip(os.Getenv("ICEBATH_MACHINE_ID")); explicit != "" {
		return explicit
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func ptrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Claims one external fire of a job, the winner getting a copy carrying its scheduled instant
func (s *Store) claimJobForFire(ctx context.Context, jobID string, o FireClaimOptions) (*Job, bool, error) {
	ttl := float64(FireClaimTTLSeconds)
	if o.TTLSeconds != nil {
		ttl = *o.TTLSeconds
	}
	var won *Job
	ok, err := s.underFireFence(ctx, jobID, func(ctx context.Context) (bool, error) {
		claimed, _, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
			if isTerminalJob(job) && !isRecoverableErrorJob(job) {
				return false, nil
			}
			if !o.Force && !isJobRunnable(job) {
				return false, nil
			}
			now := s.clk.now()
			if claimIsLive(s, job.FireClaim, now, ttl) {
				return false, nil
			}
			manualFire := o.Force || o.Manual || ptrEqual(job.ManualRunAt, job.NextRunAt)
			var instant *string
			if !manualFire {
				instant = scheduledInstant(job.NextRunAt)
			}
			if instant != nil {
				if at, ok := s.clk.parseAware(*instant, nil); ok && at.Sub(now) >= FireClaimSkewSeconds*time.Second {
					instant = nil
				}
			}
			nowISO := FormatISO(now)
			if deref(instant) != "" && completedOccurrence(s, job, *instant) {
				if isRecurring(job) {
					next, err := s.computeNextRun(job.Schedule, &nowISO, job.Timezone)
					if err != nil {
						return false, err
					}
					if next != nil && *next != "" {
						job.NextRunAt = next
						if err := s.saveJobsLocked(tx, jobs, nil, false); err != nil {
							return false, err
						}
					}
				}
				return false, nil
			}
			if o.Force {
				activateJobRecord(job)
			}
			job.FireClaim = &Claim{At: nowISO, By: machineID() + ":" + uuid4Hex()}
			job.PendingSlot = nil
			if slot, ok := extraFirePendingSlot(s, job, now, manualFire); ok && slot != "" {
				extraFireStampClaim(job, slot)
			} else if isRecurring(job) {
				next, err := s.computeNextRun(job.Schedule, &nowISO, job.Timezone)
				if err != nil {
					return false, err
				}
				if next != nil && *next != "" {
					job.NextRunAt = next
				}
			}
			if err := s.saveJobsLocked(tx, jobs, nil, false); err != nil {
				return false, err
			}
			won = job.Clone()
			won.ScheduledInstant = instant
			return true, nil
		})
		return claimed, err
	})
	if !ok {
		return nil, false, err
	}
	return won, true, err
}

// Refreshes an active fire claim for its owner without taking the fire fence
func (s *Store) heartbeatFireClaim(jobID, owner string) (bool, error) {
	ok, _, err := withJob(s, jobID, func(tx *jobsTx, jobs []*Job, i int, job *Job) (bool, error) {
		return s.refreshClaim(tx, jobs, job.FireClaim, owner)
	})
	return ok, err
}
