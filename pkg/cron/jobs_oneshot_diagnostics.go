package cron

import (
	"fmt"
	"strconv"
)

// Writes a diagnostic into the job output directory, false when the write failed
func writeOneshotDiagnostic(s *Store, job *Job, text, what string) bool {
	if _, err := s.SaveJobOutput(job.ID, text); err != nil {
		s.log.Debug(fmt.Sprintf("Failed to write %s diagnostic for job '%s'", what, job.ID), "error", err)
		return false
	}
	return true
}

// Name as str(job.get("name")) renders it, None when the record has no name key
func diagnosticName(job *Job) string {
	if !job.hasKey("name") {
		return "None"
	}
	return job.Name
}

// Leaves an output note for a claimed one shot removed before any run was recorded
func writeWedged(s *Store, job *Job) {
	if job.LastRunAt != nil {
		return
	}
	completed, times := "?", "?"
	if job.Repeat != nil {
		completed, times = strconv.Itoa(job.Repeat.Completed), "None"
		if job.Repeat.Times != nil {
			times = strconv.Itoa(*job.Repeat.Times)
		}
	}
	at, by := "unknown", "unknown"
	if job.RunClaim != nil {
		if job.RunClaim.At != "" {
			at = job.RunClaim.At
		}
		if job.RunClaim.By != "" {
			by = job.RunClaim.By
		}
	}
	text := "# Cron job removed without producing output\n\n" +
		fmt.Sprintf("- job id: %s\n", job.ID) +
		fmt.Sprintf("- name: %s\n", diagnosticName(job)) +
		fmt.Sprintf("- dispatch claimed: %s/%s\n", completed, times) +
		fmt.Sprintf("- run claimed at: %s by %s\n", at, by) +
		fmt.Sprintf("- removed at: %s\n\n", s.clk.nowISO()) +
		"This one-shot job's dispatch was claimed, but the run never " +
		"completed (`last_run_at` was never written) — the scheduler " +
		"process was most likely killed or restarted mid-execution. The " +
		"job has been removed to stop it re-firing; recreate it to run " +
		"again.\n"
	if writeOneshotDiagnostic(s, job, text, "wedged-oneshot") {
		s.log.Warn(fmt.Sprintf("Job '%s': removed without a completed run — diagnostic written to its output directory", oneshotLabel(job)))
	}
}

// Leaves an output note for a one shot retired because its run time fell outside the grace window
func writeMissed(s *Store, job *Job, nextRun string) {
	text := "# Cron job removed before firing (run time outside grace window)\n\n" +
		fmt.Sprintf("- job id: %s\n", job.ID) +
		fmt.Sprintf("- name: %s\n", diagnosticName(job)) +
		fmt.Sprintf("- scheduled run time: %s\n", nextRun) +
		fmt.Sprintf("- grace window: %ds\n", oneshotGraceSeconds) +
		fmt.Sprintf("- removed at: %s\n\n", s.clk.nowISO()) +
		"This one-shot's run time is more than the grace window in the " +
		"past (scheduler down past the window, host asleep, or jobs.json " +
		"edited), which is outside the 'will never fire' contract " +
		"enforced at create/update/resume time. The job was removed " +
		"without running; recreate it (or use the Run button) to " +
		"schedule it again.\n"
	writeOneshotDiagnostic(s, job, text, "missed-oneshot")
}

// Name when the record has one, else the id, else a question mark
func oneshotLabel(job *Job) string {
	switch {
	case job.hasKey("name"):
		return job.Name
	case job.ID != "":
		return job.ID
	}
	return "?"
}
