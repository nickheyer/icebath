package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestLazyCronStoresImportAgainstPreUpgradeSqliteUtil(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		store string
		first func(s *Store) error
	}{
		{"notepad", func(s *Store) error { _, err := openNotepad(s).List("job"); return err }},
		{"incidents", func(s *Store) error { _, err := openIncidents(s).ListIncidents(nil); return err }},
		{"executions", func(s *Store) error { _, err := openExecutionsLedger(s).List(ctx, nil, 10, nil); return err }},
		{"delivery_queue", func(s *Store) error { _, err := openDeliveryQueue(s).GetStatus(ctx, "exec"); return err }},
	} {
		t.Run(tc.store, func(t *testing.T) {
			s := openCronStore(filepath.Join(t.TempDir(), "home"), nil)
			if err := tc.first(s); err != nil {
				t.Fatalf("first open of the %s store in a fresh process failed: %v", tc.store, err)
			}
		})
	}
}

func TestOccurrencesResolveFireClaimConstantsWithoutCachedJobsExports(t *testing.T) {
	s := openCronStore(filepath.Join(t.TempDir(), "home"), nil)
	if completedOccurrence(s, &Job{ID: "job"}, "2026-01-01T00:00:00+00:00") {
		t.Fatal("an empty ledger proved a completed occurrence")
	}
	now := time.Now().UTC()
	stale := FormatISO(now.Add(-time.Hour))
	job := &Job{ID: "job", Schedule: &Schedule{Kind: "interval"},
		PendingSlot: &PendingSlot{ScheduledAt: stale, At: stale, By: "other-machine"}}
	if got := unclaimedPendingSlot(s, job, now); deref(got) != stale {
		t.Fatalf("lapsed foreign pending slot = %v want %s", got, stale)
	}
}
