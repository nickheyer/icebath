package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// executionStamp is one completed executions ledger row for a scheduled instant
type executionStamp struct {
	ID         string
	FinishedAt *string
	ClaimedAt  *string
}

// Canonical UTC stamp text of a timestamp that carries an offset, nil for offset-less or unparsable text
func scheduledInstant(value *string) *string {
	if value == nil {
		return nil
	}
	t, aware, err := timeutil.ParseISOZone(*value, time.UTC)
	if err != nil || !aware {
		return nil
	}
	out := t.UTC().Format(timeutil.RFC3339Micro)
	return &out
}

// Ledger already holds a completed run of this occurrence that finished no earlier than the slot minus skew
func completedOccurrence(s *Store, job *Job, instant string) bool {
	canonical := scheduledInstant(&instant)
	if canonical == nil {
		return false
	}
	slot, _, _ := timeutil.ParseISOZone(*canonical, time.UTC)
	earliestReal := slot.Add(-FireClaimSkewSeconds * time.Second)
	rows, err := completedExecutions(s, job.ID, *canonical)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Cannot check completed occurrence for job %s", job.ID), "error", err)
		return false
	}
	for _, row := range rows {
		stamp := row.FinishedAt
		if deref(stamp) == "" {
			stamp = row.ClaimedAt
		}
		completedAt := scheduledInstant(stamp)
		if completedAt != nil {
			at, _, _ := timeutil.ParseISOZone(*completedAt, time.UTC)
			if at.Before(earliestReal) {
				continue
			}
		}
		label := job.Name
		if !job.hasKey("name") {
			label = job.ID
		}
		shown := "at an unrecorded time"
		if deref(stamp) != "" {
			shown = *stamp
		}
		s.log.Warn(fmt.Sprintf("Job %q (%s): scheduled occurrence %s was already completed by execution %s (finished %s); "+
			"skipping the due slot without a new run", label, job.ID, *canonical, row.ID, shown))
		return true
	}
	return false
}

// Pending marker for a recurring slot taken off the schedule but not yet claimed
func pendingSlotStamp(nextRun string, now time.Time) *PendingSlot {
	return &PendingSlot{ScheduledAt: nextRun, At: FormatISO(now), By: machineID()}
}

// Pending recurring slot no live claimant holds, nil when absent, malformed, running here or live elsewhere
func unclaimedPendingSlot(s *Store, job *Job, now time.Time) *string {
	pending := job.PendingSlot
	if pending == nil {
		return nil
	}
	if kind := job.Kind(); kind != "cron" && kind != "interval" {
		return nil
	}
	if _, _, err := timeutil.ParseISOZone(pending.ScheduledAt, time.UTC); err != nil {
		return nil
	}
	if s.jobRunningInThisProcess(job.ID) {
		return nil
	}
	if pending.By != machineID() && claimIsLive(s, &Claim{At: pending.At, By: pending.By}, now, FireClaimTTLSeconds) {
		return nil
	}
	slot := pending.ScheduledAt
	return &slot
}

// CatchUpOccurrenceCount is how many missed recurring runs collapsed into one catch up run
func (s *Store) CatchUpOccurrenceCount() int {
	raw, err := os.ReadFile(filepath.Join(s.cronDir, "catch_up_occurrences"))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return int(n)
}

// Counts one catch up run in the store marker
func (s *Store) recordCatchUpOccurrence(ctx context.Context) {
	s.writeMarker(home.WithOverride(ctx, s.home), "catch_up_occurrences", fmt.Sprint(s.CatchUpOccurrenceCount()+1), ".count_")
}
