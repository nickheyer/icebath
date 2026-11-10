package cron

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Ledger at a fixed clock whose every owner probe answers alive, so the stale bound alone decides
func liveOwnerLedger(t *testing.T) (*Ledger, *Store) {
	t.Helper()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) { return true, nil })
	return openExecutionsLedger(s), s
}

// Inserts a running row of a foreign process claimed ageSeconds ago
func seedRunning(t *testing.T, l *Ledger, jobID string, ageSeconds float64, pid int) string {
	t.Helper()
	claimedAt := FormatISO(l.s.clk.now().Add(-time.Duration(ageSeconds * float64(time.Second))))
	eid := newExecutionID()
	ctx := context.Background()
	if err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO executions
               (id, job_id, status, source, process_id, pid, process_started_at, claimed_at)
               VALUES (?, ?, 'running', 'cron', ?, ?, ?, ?)`, eid, jobID, "ext-proc", pid, time.Now().Unix(), claimedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return eid
}

func executionStatus(t *testing.T, l *Ledger, eid string) any {
	t.Helper()
	rec, err := l.Get(context.Background(), eid)
	if err != nil || rec == nil {
		t.Fatalf("row %s: %v %v", eid, rec, err)
	}
	return rec["status"]
}

func recoverCount(t *testing.T, l *Ledger) int {
	t.Helper()
	n, err := l.RecoverInterrupted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLiveOwnerStaleClaimGetsRecovered(t *testing.T) {
	l, s := liveOwnerLedger(t)
	ctx := context.Background()
	t.Setenv("ICEBATH_CRON_TIMEOUT", "600")
	bound, ok := s.liveOwnerStaleAfterSeconds(ctx)
	if !ok || math.Abs(bound-7200.0) > 1e-6 {
		t.Fatalf("bound %v %v", bound, ok)
	}
	eid := seedRunning(t, l, "job-stale", bound+60, 99999)
	if n := recoverCount(t, l); n != 1 || executionStatus(t, l, eid) != "unknown" {
		t.Fatalf("stale claim recovered %d status %v", n, executionStatus(t, l, eid))
	}
	busy := seedRunning(t, l, "job-busy", bound+60, 66666)
	editExecution(t, l, busy, "progress_at", FormatISO(s.clk.now().Add(-120*time.Second)))
	if n := recoverCount(t, l); n != 0 || executionStatus(t, l, busy) != "running" {
		t.Fatalf("busy run recovered %d status %v", n, executionStatus(t, l, busy))
	}
	editExecution(t, l, busy, "progress_at", FormatISO(s.clk.now().Add(-time.Duration((bound+1)*float64(time.Second)))))
	if n := recoverCount(t, l); n != 1 || executionStatus(t, l, busy) != "unknown" {
		t.Fatalf("silent run recovered %d status %v", n, executionStatus(t, l, busy))
	}
	t.Setenv("ICEBATH_CRON_TIMEOUT", "3600")
	if wide, ok := s.liveOwnerStaleAfterSeconds(ctx); !ok || math.Abs(wide-10800.0) > 1e-6 {
		t.Fatalf("wide bound %v %v", wide, ok)
	}
	eid2 := seedRunning(t, l, "job-long", bound+60, 99999)
	if n := recoverCount(t, l); n != 0 || executionStatus(t, l, eid2) != "running" {
		t.Fatalf("long run recovered %d status %v", n, executionStatus(t, l, eid2))
	}
}

func TestLiveOwnerRecentClaimNotRecovered(t *testing.T) {
	for _, timeout := range []string{"600", "0", "nan", "inf"} {
		t.Run(timeout, func(t *testing.T) {
			l, s := liveOwnerLedger(t)
			t.Setenv("ICEBATH_CRON_TIMEOUT", timeout)
			recent := seedRunning(t, l, "job-recent", 60, 88888)
			ancient := seedRunning(t, l, "job-ancient", 30*24*3600, 77777)
			recovered := recoverCount(t, l)
			if executionStatus(t, l, recent) != "running" {
				t.Fatalf("recent claim reclaimed %v", executionStatus(t, l, recent))
			}
			if timeout == "600" {
				if recovered != 1 || executionStatus(t, l, ancient) != "unknown" {
					t.Fatalf("ancient claim recovered %d status %v", recovered, executionStatus(t, l, ancient))
				}
				return
			}
			if _, ok := s.liveOwnerStaleAfterSeconds(context.Background()); ok {
				t.Fatal("a bound derived from an unlimited or non finite inactivity timeout")
			}
			if recovered != 0 || executionStatus(t, l, ancient) != "running" {
				t.Fatalf("fail closed recovered %d status %v", recovered, executionStatus(t, l, ancient))
			}
		})
	}
}

func TestProgressStampedDuringTheSweepKeepsTheRun(t *testing.T) {
	l, s := liveOwnerLedger(t)
	t.Setenv("ICEBATH_CRON_TIMEOUT", "600")
	bound, ok := s.liveOwnerStaleAfterSeconds(context.Background())
	if !ok {
		t.Fatal("no bound")
	}
	eid := seedRunning(t, l, "job-resumed", bound+3600, 99999)
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) {
		editExecution(t, l, eid, "progress_at", FormatISO(s.clk.now()))
		return true, nil
	})
	if n := recoverCount(t, l); n != 0 || executionStatus(t, l, eid) != "running" {
		t.Fatalf("resumed run recovered %d status %v", n, executionStatus(t, l, eid))
	}
}
