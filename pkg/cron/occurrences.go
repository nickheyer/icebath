package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// executionStamp is one completed executions ledger row for a scheduled instant
type executionStamp struct {
	ID         string
	FinishedAt *string
	ClaimedAt  *string
}

// Canonical UTC isoformat of an aware timestamp, nil for naive or unparsable text
func scheduledInstant(value *string) *string {
	if value == nil {
		return nil
	}
	t, aware, err := pydatetime.ParseISOFormat(*value)
	if err != nil || !aware {
		return nil
	}
	out := pydatetime.ISOFormat(t.UTC())
	return &out
}

// Ledger already holds a completed run of this occurrence that finished no earlier than the slot minus skew
func completedOccurrence(s *Store, job *Job, instant string) bool {
	canonical := scheduledInstant(&instant)
	if canonical == nil {
		return false
	}
	slot, _, _ := pydatetime.ParseISOFormat(*canonical)
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
			at, _, _ := pydatetime.ParseISOFormat(*completedAt)
			if at.Before(earliestReal) {
				continue
			}
		}
		label := job.Name
		if !job.hasKey("name") {
			label = job.ID
		}
		shown := "None"
		if stamp != nil {
			shown = *stamp
		}
		s.log.Warn(fmt.Sprintf("Job '%s' (%s): scheduled occurrence %s was already completed by execution %s (finished %s); "+
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
	if _, _, err := pydatetime.ParseISOFormat(pending.ScheduledAt); err != nil {
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
	n, ok := pyint.Parse(pystr.Strip(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")))
	if !ok || n.Sign() < 0 || !n.IsInt64() {
		return 0
	}
	return int(n.Int64())
}

// Counts one catch up run in the store marker
func (s *Store) recordCatchUpOccurrence(ctx context.Context) {
	s.writeMarker(home.WithOverride(ctx, s.home), "catch_up_occurrences", fmt.Sprint(s.CatchUpOccurrenceCount()+1), ".count_")
}
