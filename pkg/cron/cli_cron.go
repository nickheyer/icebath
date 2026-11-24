package cron

import (
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const overdueGraceSeconds = 15 * 60

// Compact lateness like 31m, 2h 30m or 45s
func formatLateness(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "?"
	}
	n := int64(math.Max(0, math.Trunc(seconds)))
	if n < 60 {
		return fmt.Sprintf("%ds", n)
	}
	minutes := n / 60
	hours, minutes := minutes/60, minutes%60
	days, hours := hours/24, hours%24
	if days != 0 {
		minutes = 0
	}
	var parts []string
	for _, p := range []struct {
		n    int64
		unit string
	}{{days, "d"}, {hours, "h"}, {minutes, "m"}} {
		if p.n != 0 {
			parts = append(parts, fmt.Sprintf("%d%s", p.n, p.unit))
		}
	}
	if len(parts) == 0 {
		return "0m"
	}
	return strings.Join(parts, " ")
}

// Human label of a dispatch kind, empty for kinds without one
func dispatchKindLabel(kind any) string {
	switch kind {
	case "catch_up":
		return "catch-up after missed fire"
	case "late":
		return "late"
	}
	return ""
}

// Seconds the stored next run is already past, false when it is not an ISO stamp
func (s *Store) nextRunOverdueSeconds(nextRunAt any) (float64, bool) {
	text, ok := nextRunAt.(string)
	if !ok {
		return 0, false
	}
	dt, ok := s.clk.parseAware(text, nil)
	if !ok {
		return 0, false
	}
	return s.clk.now().UTC().Sub(dt.UTC()).Seconds(), true
}

// Next run label and value, Overdue with a scheduler hint when an active job is past the grace
func (s *Store) nextRunRow(job *jsonutil.Object) (string, string) {
	stamp := field(job, "next_run_at", "?")
	overdue, ok := s.nextRunOverdueSeconds(stamp)
	state := field(job, "state", nil)
	if !ok || overdue <= overdueGraceSeconds || !anyutil.IsTrue(field(job, "enabled", true)) || state == "paused" || state == "completed" {
		return "Next run", anyutil.String(stamp)
	}
	return "Overdue", cliui.Color(fmt.Sprintf("%s  (%s ago — the job has not fired; is the scheduler running?)", anyutil.String(stamp), formatLateness(overdue)), cliui.Yellow)
}

// One line verdict of a manual run, the dispatch for a background run
func runOutcome(job *jsonutil.Object) string {
	if id := field(job, "delegation_id", nil); anyutil.IsTrue(id) {
		return fmt.Sprintf("Running in background (delegation %s).", anyutil.String(id))
	}
	if field(job, "execution_mode", nil) == "background" {
		return "Running in background."
	}
	if anyutil.IsTrue(field(job, "executed", nil)) {
		success := anyutil.IsTrue(field(job, "execution_success", nil))
		if !success && field(job, "last_status", nil) == "cancelled" {
			return "Ran now: cancelled."
		}
		if success {
			return "Ran now: succeeded."
		}
		return "Ran now: failed."
	}
	if skipped := field(job, "execution_skipped", nil); anyutil.IsTrue(skipped) {
		return anyutil.String(skipped)
	}
	return "It will run on the next scheduler tick."
}
