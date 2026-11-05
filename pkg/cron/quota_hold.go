package cron

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	quotaHoldStateKey        = "quota_hold_until"
	quotaHoldScheduleExprKey = "quota_hold_cron_expr"
	// QuotaHoldSlackSeconds pads the provider window so the parked fire lands after it reopens
	QuotaHoldSlackSeconds = 60
)

var quotaRetryAfterRe = regexp.MustCompile(`(?i)retry after (\d+)s`)

// Longest cause chain walked before giving up, the bound a cyclic Unwrap cannot exceed
const maxErrorChain = 1000

// Expression of a schedule, nil when it has none the way a missing expr key reads
func scheduleExpr(sched *Schedule) *string {
	if sched == nil || sched.Expr == "" {
		return nil
	}
	return strPtr(sched.Expr)
}

// Seconds a rate limited AuthError in the chain says the provider stays closed, false without a positive hint
func quotaHoldSecondsFromFailure(err error) (float64, bool) {
	for cur, depth := err, 0; cur != nil && depth < maxErrorChain; cur, depth = errors.Unwrap(cur), depth+1 {
		authErr, ok := cur.(*provider.AuthError)
		if !ok || authErr.ReloginRequired || authErr.Code != provider.CodexRateLimitedCode {
			continue
		}
		var hint float64
		switch {
		case authErr.HasRetryAfter:
			hint = authErr.RetryAfter.Seconds()
		default:
			m := quotaRetryAfterRe.FindStringSubmatch(authErr.Error())
			if m == nil {
				return 0, false
			}
			v, perr := strconv.ParseFloat(m[1], 64)
			if perr != nil {
				return 0, false
			}
			hint = v
		}
		return hint, hint > 0
	}
	return 0, false
}

// Job is parked inside a provider window whose end is still in the future
func quotaHoldActive(s *Store, job *Job, now time.Time) bool {
	if job.QuotaHoldUntil == nil || *job.QuotaHoldUntil == "" {
		return false
	}
	until, ok := s.clk.parseAware(*job.QuotaHoldUntil, nil)
	if now.IsZero() {
		now = s.clk.now()
	}
	return ok && instantAfter(until, now)
}

// Drops the hold marker and its expression fingerprint
func quotaHoldClearState(job *Job) {
	job.QuotaHoldUntil = nil
	job.QuotaHoldCronExpr = nil
}

// Next run is the exact off lattice cron fire the hold parked for this unchanged expression
func quotaHoldIsRecoveryFire(s *Store, job *Job, nextRun string) bool {
	sched := job.Schedule
	if sched == nil || sched.Kind != "cron" || job.QuotaHoldUntil == nil || *job.QuotaHoldUntil != nextRun {
		return false
	}
	return ptrEqual(job.QuotaHoldCronExpr, scheduleExpr(sched))
}

// Instant the provider window closes plus the slack
func (s *Store) quotaWindowEnd(holdSeconds float64) time.Time {
	return secondsAfter(s.clk.now(), holdSeconds+QuotaHoldSlackSeconds)
}

// One recovery fire for a sparse schedule whose natural occurrence is half a cadence past the window
func (s *Store) quotaRecoveryWorthwhile(job *Job, naturalNext, windowEnd time.Time) bool {
	if job.QuotaHoldUntil != nil && *job.QuotaHoldUntil != "" {
		return false
	}
	cadence, ok := s.scheduleCadenceSeconds(job.Schedule)
	return ok && cadence != 0 && elapsedSeconds(naturalNext, windowEnd) >= cadence/2
}

// Parks next run past a closed provider window after a failed run, true when parked
func quotaHoldPlanHold(s *Store, job *Job, holdSeconds float64, recoverConsumedFire bool) (bool, error) {
	sched := job.Schedule
	kind := ""
	if sched != nil {
		kind = sched.Kind
	}
	if (kind != "cron" && kind != "interval") || job.State == "paused" {
		quotaHoldClearState(job)
		return false, nil
	}
	windowEnd := s.quotaWindowEnd(holdSeconds)
	var naturalNext time.Time
	haveNext := false
	if job.NextRunAt != nil {
		naturalNext, haveNext = s.clk.parseAware(*job.NextRunAt, nil)
	}
	blocked := !haveNext || instantBefore(naturalNext, windowEnd)
	recover := kind == "cron" && !blocked && recoverConsumedFire && s.quotaRecoveryWorthwhile(job, naturalNext, windowEnd)
	if !blocked && !recover {
		quotaHoldClearState(job)
		return false, nil
	}
	parked := FormatISO(windowEnd)
	if kind == "cron" && blocked && !extraFireQueued(job) {
		next, err := s.computeNextRun(sched, &parked, job.Timezone)
		if err != nil {
			return false, err
		}
		if next != nil && *next != "" {
			parked = *next
		}
	}
	if recover {
		job.QuotaHoldCronExpr = scheduleExpr(sched)
	} else {
		job.QuotaHoldCronExpr = nil
	}
	extraFireReschedule(job, parked)
	job.QuotaHoldUntil = strPtr(parked)
	s.log.Warn(fmt.Sprintf("Job '%s': provider usage window closed for %.0fs — holding fires until %s instead of "+
		"failing on every cadence tick", job.Label(), holdSeconds, parked))
	return true, nil
}

// Line appended to the one failure alert sent on entering the hold, empty when no hold applies
func quotaHoldNotice(s *Store, job *Job, holdSeconds float64) string {
	if holdSeconds == 0 || job.Schedule == nil || (job.Schedule.Kind != "cron" && job.Schedule.Kind != "interval") {
		return ""
	}
	windowEnd := s.quotaWindowEnd(holdSeconds)
	hours := holdSeconds / 3600.0
	return fmt.Sprintf("\nThe provider's usage window is closed for about %.1fh. This job is held "+
		"through %s and resumes at the first safe "+
		"opportunity afterwards; no further alerts are sent while the provider is unavailable.",
		hours, pydatetime.Strftime(windowEnd, "%Y-%m-%d %H:%M %Z"))
}
