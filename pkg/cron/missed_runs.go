package cron

import (
	"fmt"
	"time"
)

const maxCountedSlots = 1000

// Explicit catch_up wins, one shots never catch up, an explicit grace skips, else cron.catch_up_missed
func (s *Store) jobCatchesUp(job *Job) bool {
	if job.CatchUp != nil {
		return *job.CatchUp
	}
	if job.Kind() == "once" {
		return false
	}
	if s.knobs.graceSecondsOverride(job.GraceMinutes, rawGraceMinutes(job)) != nil {
		return false
	}
	b, isBool := s.cronConfigValue("catch_up_missed", true).(bool)
	return !isBool || b
}

// Scheduled slots from first up to now, stopping at the counting cap
func (s *Store) countMissedSlots(sched *Schedule, first string, now time.Time, timezone *string) (int, error) {
	count, cursor := 0, &first
	for cursor != nil && *cursor != "" && count < maxCountedSlots {
		at, ok := s.clk.parseAware(*cursor, nil)
		if !ok || instantAfter(at, now) {
			break
		}
		count++
		following, err := s.computeNextRun(sched, cursor, timezone)
		if err != nil {
			return count, err
		}
		if following != nil && *following == *cursor {
			following = nil
		}
		cursor = following
	}
	return count, nil
}

// Books skipped occurrences in the executions ledger, a ledger failure only logged
func (s *Store) bookMissed(job *Job, count int, first *string, detail string, capped bool) {
	if capped {
		detail = fmt.Sprintf("%s; counting stopped at %d", detail, maxCountedSlots)
	}
	if err := recordMissedOccurrences(s, job.ID, count, first, detail); err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': could not book its missed occurrences", job.ID), "error", err)
	}
}

func epochSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Fast forwards a recurring job past its grace window, true when the miss is skipped rather than run once
func fastForwardMissedRecurring(d *dueJob, grace int) (bool, error) {
	sc, s, job := d.scan, d.scan.s, d.job
	if elapsedSeconds(sc.now, d.nextDT) <= float64(grace) {
		return false, nil
	}
	newNext, err := d.recomputeNext()
	if err != nil || deref(newNext) == "" {
		return false, err
	}
	next := *newNext
	sc.persist(job.ID, func(rj *Job) { rj.NextRunAt = strPtr(next) })
	missed, err := s.countMissedSlots(d.schedule(), d.nextRun, sc.now, job.Timezone)
	if err != nil {
		return false, err
	}
	capped := missed >= maxCountedSlots
	first := d.nextRun
	nextAt, _ := s.clk.parseAware(next, nil)
	if instantAfter(nextAt, sc.now) && !s.jobCatchesUp(job) {
		s.log.Info(fmt.Sprintf("Job '%s' missed its scheduled time (%s, grace=%ds). Skipping %d missed occurrence(s) "+
			"because catch-up is off for this job; next run: %s", d.label(), d.nextRun, grace, missed, next))
		sc.onSaved = append(sc.onSaved, func() { recordCronMissed(job) },
			func() { s.bookMissed(job, missed, &first, "skipped, catch-up off; next run "+next, capped) })
		return true, nil
	}
	s.log.Info(fmt.Sprintf("Job '%s' missed its scheduled time (%s, grace=%ds). Running now; next run provisionally "+
		"set to: %s (re-anchored on completion)", d.label(), d.nextRun, grace, next))
	sc.onSaved = append(sc.onSaved, s.recordCatchUpOccurrence)
	if missed > 1 {
		sc.onSaved = append(sc.onSaved, func() { s.bookMissed(job, missed-1, &first, "collapsed into one catch-up run", capped) })
	}
	return false, nil
}

// Retires a one shot past its grace window, true when it must not fire this tick
func retireExpiredOneshot(d *dueJob) bool {
	sc, s, job := d.scan, d.scan.s, d.job
	if elapsedSeconds(sc.now, d.nextDT) <= oneshotGraceSeconds {
		return false
	}
	claimed := job.RunClaim != nil || job.FireClaim != nil
	dueAt := epochSeconds(d.nextDT)
	graceStart := dueAt
	if !claimed {
		graceStart = dispatchHoldGraceStartsAt(s.cronDir, dueAt, oneshotGraceSeconds)
	}
	if epochSeconds(sc.now)-graceStart <= oneshotGraceSeconds {
		return false
	}
	if s.jobCatchesUp(job) {
		s.log.Info(fmt.Sprintf("Job '%s' missed its one-shot time %s; firing it once now (catch_up on).", d.label(), d.nextRun))
		return false
	}
	if OutageCovers(s.cronDir, dueAt, oneshotGraceSeconds) {
		return false
	}
	if job.RunClaim != nil && job.RunClaim.Outage && job.FireClaim == nil {
		return false
	}
	if job.RunClaim == nil && job.FireClaim == nil {
		first := d.nextRun
		writeMissed(s, job, first)
		sc.retire(job.ID)
		sc.onSaved = append(sc.onSaved, func() { recordCronMissed(job) },
			func() { s.bookMissed(job, 1, &first, "one-shot past its grace window; catch-up off", false) })
	}
	return true
}
