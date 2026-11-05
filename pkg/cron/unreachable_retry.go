package cron

import (
	"fmt"
	"time"
)

// UnreachableRetryDelaysSeconds is the bounded re run ladder for fires that never reached the model
var UnreachableRetryDelaysSeconds = []int{300, 900, 1800}

// Config cron.retry_unreachable is anything but false, true when config is unreadable
func unreachableRetryEnabledIn(cfg map[string]any) bool {
	cron, ok := cfg["cron"].(map[string]any)
	if !ok {
		return true
	}
	v, present := cron["retry_unreachable"]
	return !present || v != false
}

// Ladder enabled in the store config, the reliability default when config cannot load
func (s *Store) unreachableRetryEnabled() bool {
	cfg, err := s.loadConfig()
	if err != nil {
		return true
	}
	return unreachableRetryEnabledIn(cfg)
}

// Transient network failure from a run that completed zero model calls
func isModelUnreachableFailure(err error, sessionAPICalls int) bool {
	if sessionAPICalls > 0 {
		return false
	}
	return isTransientProviderResolveError(err)
}

// Recurring, not paused, and enabled in config
func (s *Store) unreachableLadderApplies(job *Job) bool {
	return isRecurring(job) && job.State != "paused" && s.unreachableRetryEnabled()
}

// Attempt count the job's ladder state records
func unreachableAttempt(job *Job) int {
	if job.UnreachableRetry == nil {
		return 0
	}
	return job.UnreachableRetry.Attempt
}

// Rung instant a retry parks, nil when exhausted, inapplicable, or the natural occurrence comes first
func (s *Store) unreachableLadderInstant(job *Job, naturalNext *time.Time, now time.Time) *time.Time {
	attempt := unreachableAttempt(job)
	if attempt >= len(UnreachableRetryDelaysSeconds) || !s.unreachableLadderApplies(job) {
		return nil
	}
	retryAt := secondsAfter(now, float64(UnreachableRetryDelaysSeconds[attempt]))
	if naturalNext != nil && instantAtOrBefore(*naturalNext, retryAt) {
		return nil
	}
	return &retryAt
}

// Plan retry will park a re run for this flagged failure so the interim notice may be held
func unreachableRetryWillRetry(s *Store, job *Job) (bool, error) {
	counted := 1
	if unreachableRetryIsRetryRun(s, job) {
		counted = 0
	}
	if job.Repeat != nil && job.Repeat.Times != nil && *job.Repeat.Times > 0 && job.Repeat.Completed+counted >= *job.Repeat.Times {
		return false, nil
	}
	now := s.clk.now()
	nowISO := FormatISO(now)
	sched := job.Schedule
	if sched == nil {
		sched = &Schedule{}
	}
	next, err := s.computeNextRun(sched, &nowISO, job.Timezone)
	if err != nil {
		return false, err
	}
	if next == nil {
		return false, nil
	}
	naturalNext, ok := s.clk.parseAware(*next, nil)
	if !ok {
		return false, nil
	}
	return s.unreachableLadderInstant(job, &naturalNext, now) != nil, nil
}

// A run reached the model so the ladder resets
func unreachableRetryClearState(job *Job) { job.UnreachableRetry = nil }

// Next run is the exact ladder instant parked for this unchanged expression
func unreachableRetryIsRetryFire(s *Store, job *Job, nextRun string) bool {
	state := job.UnreachableRetry
	if state == nil {
		return false
	}
	return state.At == nextRun && ptrEqual(state.Expr, scheduleExpr(job.Schedule))
}

// Dispatch fired the parked ladder instant, a re run that must not count toward repeat again
func unreachableRetryIsRetryRun(s *Store, job *Job) bool {
	if job.ScheduledInstant == nil {
		return false
	}
	var at *string
	if job.UnreachableRetry != nil {
		at = strPtr(job.UnreachableRetry.At)
	}
	parked := scheduledInstant(s, at)
	return parked != nil && *parked == *job.ScheduledInstant
}

// Pulls next run to the ladder instant when sooner than the natural occurrence, true when a retry is parked
func unreachableRetryPlanRetry(s *Store, job *Job) bool {
	var naturalNext *time.Time
	if job.NextRunAt != nil {
		if at, ok := s.clk.parseAware(*job.NextRunAt, nil); ok {
			naturalNext = &at
		}
	}
	retryAt := s.unreachableLadderInstant(job, naturalNext, s.clk.now())
	attempt := unreachableAttempt(job)
	if retryAt == nil {
		if attempt >= len(UnreachableRetryDelaysSeconds) && s.unreachableLadderApplies(job) {
			next := "None"
			if job.NextRunAt != nil {
				next = *job.NextRunAt
			}
			s.log.Warn(fmt.Sprintf("Job '%s': model unreachable after %d automatic re-runs — waiting for the "+
				"scheduled occurrence at %s", job.Label(), attempt, next))
		}
		unreachableRetryClearState(job)
		return false
	}
	delay := UnreachableRetryDelaysSeconds[attempt]
	at := FormatISO(*retryAt)
	job.UnreachableRetry = &RetryState{Attempt: attempt + 1, At: at, Expr: scheduleExpr(job.Schedule)}
	job.NextRunAt = strPtr(at)
	if job.State != "paused" {
		job.State = "scheduled"
	}
	s.log.Info(fmt.Sprintf("Job '%s': model unreachable with zero API calls — automatic re-run %d/%d in %ds "+
		"(at %s)", job.Label(), attempt+1, len(UnreachableRetryDelaysSeconds), delay, at))
	return true
}
