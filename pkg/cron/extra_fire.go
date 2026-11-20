package cron

import "time"

// Trigger marker the scheduled slot rides on until the claim restores it
func extraFireQueued(job *Job) bool {
	return job.ManualRunAt != nil && job.NextRunAt != nil && *job.ManualRunAt == *job.NextRunAt && job.ManualRunRestoreAt != nil
}

// Scheduled slot the trigger overwrote while one is queued, the stored next run otherwise
func extraFireRestoreTarget(job *Job) *string {
	if extraFireQueued(job) {
		if !job.ManualRunRestoreAt.Valid {
			return nil
		}
		return strPtr(job.ManualRunRestoreAt.S)
	}
	return job.NextRunAt
}

// Slot text when it parses to an instant after now
func (s *Store) futureSlot(value *string, now time.Time) (string, bool) {
	if value == nil {
		return "", false
	}
	if at, ok := s.clk.parseAware(*value, nil); ok && at.After(now) {
		return *value, true
	}
	return "", false
}

func restoreValue(job *Job) *string {
	if job.ManualRunRestoreAt == nil || !job.ManualRunRestoreAt.Valid {
		return nil
	}
	return strPtr(job.ManualRunRestoreAt.S)
}

// Future scheduled slot an off schedule fire must leave pending, the restored slot after a trigger
func extraFirePendingSlot(s *Store, job *Job, now time.Time, manualFire bool) (string, bool) {
	if deref(job.ManualRunAt) == deref(job.NextRunAt) && (job.ManualRunAt == nil) == (job.NextRunAt == nil) {
		return s.futureSlot(restoreValue(job), now)
	}
	if manualFire {
		return s.futureSlot(job.NextRunAt, now)
	}
	return "", false
}

// Marks the fire claim as an extra fire and puts the pending slot back on the schedule
func extraFireStampClaim(job *Job, slot string) {
	job.FireClaim.ExtraSlot = slot
	job.NextRunAt = strPtr(slot)
	job.ManualRunRestoreAt = nil
}

// Claim belongs to an off schedule fire that leaves the scheduled occurrence pending
func extraFireIsExtra(job *Job) bool { return job.FireClaim != nil && job.FireClaim.ExtraSlot != "" }

// Trigger markers a finished extra fire must keep because another trigger is still queued
func extraFireHeldMarkers(job *Job) map[string]any {
	if !extraFireQueued(job) {
		return map[string]any{}
	}
	held := map[string]any{"manual_run_at": *job.ManualRunAt}
	if v := restoreValue(job); v != nil {
		held[restoreKey] = *v
	} else {
		held[restoreKey] = nil
	}
	if job.ManualRunPrompt != nil {
		held["manual_run_prompt"] = *job.ManualRunPrompt
	} else if job.hasKey("manual_run_prompt") {
		held["manual_run_prompt"] = nil
	}
	return held
}

// Moves the next fire to at, carrying a queued trigger marker along with it
func extraFireReschedule(job *Job, at string) {
	if extraFireQueued(job) {
		job.ManualRunAt = strPtr(at)
	}
	job.NextRunAt = strPtr(at)
}

// Leaves a pending slot or queued trigger alone, otherwise schedules the next fire after an extra run
func extraFireSettle(s *Store, job *Job, now string) error {
	nowAt, ok := s.clk.parseAware(now, nil)
	if !ok {
		parsed, err := ParseISO(now)
		if err != nil {
			return err
		}
		nowAt = parsed.Time
	}
	if _, future := s.futureSlot(job.NextRunAt, nowAt); extraFireQueued(job) || future {
		return nil
	}
	if job.Kind() == "once" {
		job.NextRunAt = strPtr(now)
		return nil
	}
	next, err := s.computeNextRun(job.Schedule, &now, job.Timezone)
	if err != nil {
		return err
	}
	if next != nil {
		job.NextRunAt = next
	}
	return nil
}
