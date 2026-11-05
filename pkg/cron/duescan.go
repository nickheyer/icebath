package cron

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Mutable state of one due scan, the raw store records plus what to persist
type dueScan struct {
	s         *Store
	ctx       context.Context
	rawJobs   []*Job
	now       time.Time
	needsSave bool
	removed   []string
	onSaved   []func()
}

func (sc *dueScan) find(jobID string) *Job {
	for _, rj := range sc.rawJobs {
		if rj.ID == jobID {
			return rj
		}
	}
	return nil
}

// Applies fields to the raw record of the job and flags a save
func (sc *dueScan) persist(jobID string, apply func(rj *Job)) {
	if rj := sc.find(jobID); rj != nil {
		apply(rj)
		sc.needsSave = true
	}
}

// Drops the raw record of the job as an intentional removal
func (sc *dueScan) retire(jobID string) {
	for i, rj := range sc.rawJobs {
		if rj.ID == jobID {
			sc.rawJobs = append(sc.rawJobs[:i:i], sc.rawJobs[i+1:]...)
			sc.removed = append(sc.removed, jobID)
			sc.needsSave = true
			return
		}
	}
}

// One candidate under evaluation with its stored next run raw and normalized
type dueJob struct {
	job     *Job
	scan    *dueScan
	nextRun string
	rawNext ISOTime
	nextDT  time.Time
}

func (d *dueJob) schedule() *Schedule { return d.job.scheduleOrEmpty() }

func (d *dueJob) kind() string { return d.job.Kind() }

func (d *dueJob) label() string { return d.job.Label() }

func (d *dueJob) recomputeNext() (*string, error) {
	now := FormatISO(d.scan.now)
	return d.scan.s.computeNextRun(d.schedule(), &now, d.job.Timezone)
}

// Repairs records that would abort the scan, missing ids, non object schedules and non ISO stamps
func (s *Store) normalizeDueScanRecords(raw []*Job) bool {
	changed := false
	for _, rj := range raw {
		if rj.ID == "" {
			id := ""
			if legacy, ok := rj.Extra["job_id"]; ok {
				id, _ = decodeText(legacy)
				delete(rj.Extra, "job_id")
			}
			if id == "" {
				id = newJobID()
			}
			rj.ID = id
			changed = true
		}
		if rj.Schedule == nil || (rj.Schedule.Kind == "" && rj.Schedule.Raw != "") {
			rj.Schedule = &Schedule{}
			changed = true
		}
		for _, p := range []**string{&rj.NextRunAt, &rj.LastRunAt} {
			if *p != nil {
				if _, ok := s.clk.parseAware(**p, nil); !ok {
					*p = nil
					changed = true
				}
			}
		}
	}
	return changed
}

// Forces enabled false on a record that is enabled while carrying pause markers
func (sc *dueScan) selfDisableHalfPaused(job *Job) {
	sc.s.log.Error(fmt.Sprintf("Job '%s' (%s) has pause markers while enabled=true; self-disabling so it cannot "+
		"fire (pause must be authoritative).", job.Name, job.ID))
	rj := sc.find(job.ID)
	if rj == nil {
		return
	}
	rj.Enabled, rj.enabledAbsent, rj.State = false, false, "paused"
	if deref(rj.PausedAt) == "" {
		rj.PausedAt = strPtr(FormatISO(sc.now))
	}
	if deref(rj.PausedReason) == "" {
		rj.PausedReason = strPtr("auto-disabled: enabled+paused contradiction")
	}
	sc.needsSave = true
}

// Recomputes and persists a missing next run, empty when nothing can be recovered
func (sc *dueScan) recoverMissingNextRun(job *Job) (string, error) {
	s := sc.s
	sched := job.scheduleOrEmpty()
	zone := s.knobs.zoneForJob(job.Timezone)
	recovered := s.recoverableOneshotRunAt(sched, sc.now, job.LastRunAt, zone)
	recoveryKind := "one-shot"
	if recovered == nil && isRecurring(job) {
		now := FormatISO(sc.now)
		next, err := s.computeNextRun(sched, &now, job.Timezone)
		if err != nil {
			return "", err
		}
		recovered, recoveryKind = next, sched.Kind
	}
	if deref(recovered) == "" {
		return "", nil
	}
	s.log.Info(fmt.Sprintf("Job '%s' had no next_run_at; recovering %s run at %s", job.Label(), recoveryKind, *recovered))
	recoverable := isRecoverableErrorJob(job)
	apply := func(rj *Job) {
		rj.NextRunAt = strPtr(*recovered)
		if recoverable {
			rj.State = "scheduled"
		}
	}
	apply(job)
	sc.persist(job.ID, apply)
	return *recovered, nil
}

// Re-anchors a cron job whose stored offset moved while its stored wall clock is still ahead
func (d *dueJob) repairTimezoneShiftedCron() (bool, error) {
	now := d.scan.now
	if !(instantAtOrBefore(d.nextDT, now) && d.rawNext.Aware && offsetMismatch(d.rawNext.Time, now) &&
		storedWallClockIsFuture(d.rawNext.Time, now)) {
		return false, nil
	}
	next, err := d.recomputeNext()
	if err != nil || deref(next) == "" {
		return false, err
	}
	_, oldOff := d.rawNext.Time.Zone()
	_, newOff := now.Zone()
	d.scan.s.log.Info(fmt.Sprintf("Job '%s' next_run_at offset changed (%s -> %s). Recomputing cron run to "+
		"preserve local wall-clock intent: %s", d.label(), offsetRepr(oldOff), offsetRepr(newOff), *next))
	d.scan.persist(d.job.ID, func(rj *Job) { rj.NextRunAt = strPtr(*next) })
	return true, nil
}

// Python timedelta str of a UTC offset in seconds
func offsetRepr(seconds int) string {
	if seconds < 0 {
		days := int(math.Floor(float64(seconds) / 86400))
		rest := seconds - days*86400
		return fmt.Sprintf("%d day, %d:%02d:%02d", days, rest/3600, rest%3600/60, rest%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds%3600/60, seconds%60)
}

// Re-arms a recurring job wedged in a stale persisted error and returns the effective next run
func (d *dueJob) rearmStaleErrorRecurring() (time.Time, error) {
	s, now := d.scan.s, d.scan.now
	kind := d.kind()
	if !((kind == "cron" || kind == "interval") && instantAfter(d.nextDT, now) &&
		s.jobIsStaleErrorRecurring(d.job, d.schedule(), now)) {
		return d.nextDT, nil
	}
	var recovered *string
	var recoveredDT time.Time
	ok := false
	if kind == "interval" {
		recovered, recoveredDT, ok = strPtr(FormatISO(now)), now, true
	} else {
		next, err := d.recomputeNext()
		if err != nil {
			return d.nextDT, err
		}
		recovered = next
		if deref(next) != "" {
			recoveredDT, ok = s.clk.parseAware(*next, nil)
		}
	}
	if !(deref(recovered) != "" && ok && instantBefore(recoveredDT, d.nextDT)) {
		return d.nextDT, nil
	}
	s.log.Warn(fmt.Sprintf("cron.persisted_error.recovered job='%s' id=%s — recurring job wedged in stale "+
		"last_status=error without re-firing for a full cadence; re-arming next_run_at to %s so it re-dispatches "+
		"without force-run/resume", d.job.Name, d.job.ID, *recovered))
	s.recordPersistedErrorRecovery(d.job, d.nextRun)
	d.job.NextRunAt = strPtr(*recovered)
	d.scan.persist(d.job.ID, func(rj *Job) { rj.NextRunAt = strPtr(*recovered) })
	return recoveredDT, nil
}

// Re-anchors a due cron instant left off the current lattice by an edit, firing once for migrations and planned instants
func (d *dueJob) reanchorStaleCron() (bool, error) {
	s := d.scan.s
	switch classifyStaleCronNextRun(d.schedule(), d.rawNext.Time, d.nextDT) {
	case StaleCronExprEdit:
		if quotaHoldIsRecoveryFire(s, d.job, d.nextRun) || unreachableRetryIsRetryFire(s, d.job, d.nextRun) {
			s.log.Info(fmt.Sprintf("cron.off_lattice_fire job='%s' id=%s expr=%s at=%s",
				d.label(), d.job.ID, pyrepr.Repr(d.schedule().Expr), d.nextRun))
			return false, nil
		}
		next, err := d.recomputeNext()
		if err != nil {
			return false, err
		}
		s.log.Info(fmt.Sprintf("Job '%s' next_run_at %s does not match its current cron expression %s (direct "+
			"jobs.json edit?); re-anchoring to %s without firing.", d.label(), d.nextRun,
			pyrepr.Repr(d.schedule().Expr), pyrepr.Str(skillAny(next))))
		if deref(next) != "" {
			d.scan.persist(d.job.ID, func(rj *Job) { rj.NextRunAt = strPtr(*next) })
		}
		return true, nil
	case StaleCronTimezoneMigration:
		_, rawOff := d.rawNext.Time.Zone()
		_, nowOff := d.scan.now.Zone()
		s.log.Warn(fmt.Sprintf("cron.timezone_migration.catch_up job='%s' id=%s expr=%s stored=%s normalized=%s — "+
			"stored next_run_at carries a pre-migration UTC offset (%s, now %s) and is a legal occurrence at its "+
			"own wall clock; firing the due run instead of re-anchoring past it.", d.label(), d.job.ID,
			pyrepr.Repr(d.schedule().Expr), d.nextRun, FormatISO(d.nextDT), offsetRepr(rawOff), offsetRepr(nowOff)))
		s.recordTimezoneMigrationCatchup(d.job, d.rawNext.Time, d.nextDT)
	}
	return false, nil
}

// Retires a finite one shot whose budget is spent unless this process is still running it
func (sc *dueScan) oneshotDispatchLimitReached(job *Job) bool {
	s := sc.s
	if job.Repeat == nil || job.Repeat.Times == nil || *job.Repeat.Times <= 0 || job.Repeat.Completed < *job.Repeat.Times {
		return false
	}
	times, completed, name := *job.Repeat.Times, job.Repeat.Completed, job.Label()
	if s.jobRunningInThisProcess(job.ID) {
		s.log.Info(fmt.Sprintf("Job '%s': dispatch limit reached (%d/%d) but its run is still in flight in this "+
			"process — keeping entry", name, completed, times))
		return true
	}
	if job.LastRunAt != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': one-shot dispatch limit reached (%d/%d) on a record that already "+
			"completed a run (last_run_at=%s) — removing it WITHOUT firing. This record was re-armed without a "+
			"budget reset (pre-#93615 store or hand edit); re-run it with 'icebath cron resume <job> --run-now' "+
			"(#93524).", name, completed, times, *job.LastRunAt))
	} else {
		s.log.Info(fmt.Sprintf("Job '%s': one-shot dispatch limit reached (%d/%d) — removing stale due entry",
			name, completed, times))
	}
	sc.retire(job.ID)
	writeWedged(s, job)
	return true
}

// Restores an occurrence the dispatcher advanced past but never claimed, once
func (sc *dueScan) restoreUnclaimedSlot(job *Job) string {
	slot := unclaimedPendingSlot(sc.s, job, sc.now)
	if slot == nil {
		return ""
	}
	sc.s.log.Warn(fmt.Sprintf("Job '%s' (%s): occurrence %s was taken off the schedule but never claimed "+
		"(scheduler stopped before dispatch); restoring it as the due instant (was %s).",
		nameOrID(job), job.ID, *slot, pyrepr.Str(skillAny(job.NextRunAt))))
	job.NextRunAt = strPtr(*slot)
	job.PendingSlot = nil
	if rj := sc.find(job.ID); rj != nil {
		rj.PendingSlot = nil
	}
	sc.persist(job.ID, func(rj *Job) { rj.NextRunAt = strPtr(*slot) })
	return *slot
}

// Grace window the due scan applies per job, swapped by tests that poison one record
var dueScanGraceSeconds = (*Store).jobGraceSeconds

// Evaluates one job turning any panic into an error so one malformed record never aborts the scan
func (sc *dueScan) evaluateContained(job *Job, runClaimTTL float64) (fires bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			fires, err = false, fmt.Errorf("%v", r)
		}
	}()
	return sc.evaluateDueJob(job, runClaimTTL)
}

// Decides whether one enabled non terminal job fires this tick, persisting any repairs
func (sc *dueScan) evaluateDueJob(job *Job, runClaimTTL float64) (bool, error) {
	s, now := sc.s, sc.now
	if job.Kind() == "once" && claimIsLive(s, job.RunClaim, now, runClaimTTL) {
		return false, nil
	}
	nextRun := sc.restoreUnclaimedSlot(job)
	if nextRun == "" {
		nextRun = deref(job.NextRunAt)
	}
	if nextRun == "" {
		recovered, err := sc.recoverMissingNextRun(job)
		if err != nil {
			return false, err
		}
		nextRun = recovered
	}
	if nextRun == "" {
		return false, nil
	}
	zone := s.knobs.zoneForJob(job.Timezone)
	raw, err := ParseISO(nextRun)
	if err != nil {
		return false, err
	}
	d := &dueJob{job: job, scan: sc, nextRun: nextRun, rawNext: raw, nextDT: s.clk.ensureAware(raw, zone)}
	kind := d.kind()
	recurring := kind == "cron" || kind == "interval"
	manualRun := job.ManualRunAt != nil && *job.ManualRunAt == nextRun
	if !manualRun && completedOccurrence(s, job, nextRun) {
		if recurring {
			next, err := d.recomputeNext()
			if err != nil {
				return false, err
			}
			if deref(next) != "" {
				sc.persist(job.ID, func(rj *Job) { rj.NextRunAt = strPtr(*next) })
			}
		}
		return false, nil
	}
	if kind == "cron" && !manualRun && zone == nil {
		repaired, err := d.repairTimezoneShiftedCron()
		if err != nil || repaired {
			return false, err
		}
	}
	if d.nextDT, err = d.rearmStaleErrorRecurring(); err != nil {
		return false, err
	}
	if instantAfter(d.nextDT, now) {
		return false, nil
	}
	job.ScheduledInstant = nil
	if !manualRun {
		job.ScheduledInstant = scheduledInstant(s, job.NextRunAt)
	}
	if !manualRun && kind == "cron" {
		reanchored, err := d.reanchorStaleCron()
		if err != nil || reanchored {
			return false, err
		}
	}
	grace := dueScanGraceSeconds(s, job)
	if !manualRun && recurring {
		skipped, err := fastForwardMissedRecurring(d, grace)
		if err != nil || skipped {
			return false, err
		}
	}
	if kind == "once" {
		if retireExpiredOneshot(d) || sc.oneshotDispatchLimitReached(job) {
			return false, nil
		}
		claim := &Claim{At: FormatISO(now), By: machineID()}
		if elapsedSeconds(now, d.nextDT) > oneshotGraceSeconds {
			claim.Outage = true
		}
		job.RunClaim = claim
		sc.persist(job.ID, func(rj *Job) { c := *claim; rj.RunClaim = &c })
	}
	if !manualRun && recurring {
		lateness := math.Max(0, elapsedSeconds(now, d.nextDT))
		stamp := &Dispatch{ScheduledAt: nextRun, DispatchedAt: FormatISO(now),
			LatenessSeconds: roundTenth(lateness), Kind: classifyDispatchLateness(lateness, grace)}
		job.LastDispatch = stamp
		slot := pendingSlotStamp(nextRun, now)
		sc.persist(job.ID, func(rj *Job) {
			c := *stamp
			rj.LastDispatch = &c
			rj.PendingSlot = slot
		})
	}
	return true, nil
}

// Prunes completed one shot records whose last run is older than the retention window
func (s *Store) sweepCompletedOneshots(raw []*Job, now time.Time, removed *[]string) ([]*Job, bool, error) {
	days := s.cronConfigFloat("completed_retention_days", completedOneshotRetentionDays)
	if days <= 0 {
		return raw, false, nil
	}
	if days > 999999999 {
		return raw, false, fmt.Errorf("days=%v; must have magnitude <= 999999999", days)
	}
	whole := math.Floor(days)
	cutoff := now.AddDate(0, 0, -int(whole)).Add(-time.Duration((days - whole) * 86400 * float64(time.Second)))
	if cutoff.Year() < 1 {
		return raw, false, fmt.Errorf("date value out of range")
	}
	kept := raw[:0:0]
	pruned := false
	for _, rj := range raw {
		lastRun, ok := time.Time{}, false
		if rj.State == "completed" && rj.Kind() == "once" && rj.LastRunAt != nil {
			lastRun, ok = s.clk.parseAware(*rj.LastRunAt, nil)
		}
		if !ok || !lastRun.Before(cutoff) {
			kept = append(kept, rj)
			continue
		}
		pruned = true
		if removed != nil && rj.ID != "" {
			*removed = append(*removed, rj.ID)
		}
		s.log.Info(fmt.Sprintf("Job '%s': pruning completed one-shot record (finished %s, retention %.1f days)",
			rj.Label(), *rj.LastRunAt, days))
	}
	return kept, pruned, nil
}

// Jobs due now, collapsing a stale recurring backlog into one fire and persisting every repair
func (s *Store) getDueJobs(ctx context.Context) ([]*Job, error) {
	var due []*Job
	err := s.withJobsLock(func(tx *jobsTx) error {
		var err error
		due, err = s.getDueJobsLocked(ctx, tx)
		return err
	})
	return due, err
}

func (s *Store) getDueJobsLocked(ctx context.Context, tx *jobsTx) ([]*Job, error) {
	raw, err := s.loadJobs(tx)
	if err != nil {
		return nil, err
	}
	sc := &dueScan{s: s, ctx: ctx, rawJobs: raw, now: s.clk.now()}
	sc.needsSave = s.normalizeDueScanRecords(raw)
	jobs := make([]*Job, 0, len(raw))
	for _, rj := range raw {
		jobs = append(jobs, applySkillFields(rj.Clone()))
	}
	runClaimTTL := s.oneshotRunClaimTTL(ctx)
	kept, pruned, err := s.sweepCompletedOneshots(sc.rawJobs, sc.now, &sc.removed)
	if err != nil {
		return nil, err
	}
	if pruned {
		sc.rawJobs, sc.needsSave = kept, true
		live := jobs[:0:0]
		for _, j := range jobs {
			if sc.find(j.ID) != nil {
				live = append(live, j)
			}
		}
		jobs = live
	}
	var due []*Job
	for _, job := range jobs {
		if isTerminalJob(job) && !isRecoverableErrorJob(job) || !job.Enabled {
			continue
		}
		if hasPauseMarker(job) {
			sc.selfDisableHalfPaused(job)
			continue
		}
		fires, err := sc.evaluateContained(job, runClaimTTL)
		if err != nil {
			label := job.Name
			if label == "" {
				label = job.ID
			}
			if label == "" {
				label = "?"
			}
			s.log.Error(fmt.Sprintf("Skipping malformed cron job %s during due scan", pyrepr.Repr(label)), "error", err)
			continue
		}
		if fires {
			due = append(due, job)
		}
	}
	if sc.needsSave {
		var removed []string
		if len(sc.removed) > 0 {
			removed = sc.removed
		}
		if err := s.saveJobsLocked(tx, sc.rawJobs, removed, false); err != nil {
			if isOSError(err) {
				storeHealthNoteUnwritable(s, err, "due-scan repairs not persisted", "scan")
				return due, nil
			}
			return nil, err
		}
	}
	storeHealthEndRecoveryWindow(s, s.cronDir)
	for _, emit := range sc.onSaved {
		emit()
	}
	return due, nil
}
