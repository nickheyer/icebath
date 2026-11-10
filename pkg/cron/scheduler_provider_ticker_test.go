package cron

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs the real ticker loop on a virtual clock, returning its waits and the virtual seconds elapsed
func driveTicker(t *testing.T, loop string, tickCost float64, cycles int) ([]float64, float64) {
	t.Helper()
	s, _ := newTestStore(t)
	now := 100.0
	var waits []float64
	patchChronos(t, &tickerMonotonic, func() float64 { return now })
	patchChronos(t, &tickerWait, func(_ <-chan struct{}, d float64) bool {
		waits = append(waits, d)
		now += d
		return len(waits) >= cycles
	})
	patchChronos(t, &recoverInterruptedExecutions, func(context.Context, *Store) (int, error) { return 0, nil })
	patchChronos(t, &tickHook, func(context.Context, *Store, tickOptions) (int, error) {
		now += tickCost
		return 0, nil
	})
	patchChronos(t, &recordTickerHeartbeat, func(*Store, context.Context, bool) {})
	interval := 60.0
	o := StartOptions{Interval: &interval}
	if loop == "multiplex" {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "cron"), 0o700); err != nil {
			t.Fatal(err)
		}
		o.ProfileHomes = []ProfileHome{{Name: strPtr("p1"), Home: root}}
	}
	(&InProcessCronScheduler{}).Start(withCronStore(context.Background(), s), make(chan struct{}), o)
	return waits, now - 100.0
}

func TestTickerPeriodDoesNotAccumulateTickCost(t *testing.T) {
	for _, loop := range []string{"single", "multiplex"} {
		t.Run(loop, func(t *testing.T) {
			cycles := 20
			waits, elapsed := driveTicker(t, loop, 0.25, cycles)
			if len(waits) != cycles {
				t.Fatalf("waits %v", waits)
			}
			for _, w := range waits {
				if w <= 0 || w >= 60 {
					t.Fatalf("waits %v", waits)
				}
			}
			if math.Abs(elapsed-float64(cycles)*60) >= 0.1 {
				t.Fatalf("elapsed %v", elapsed)
			}
		})
	}
}

func TestTickerOverrunRebaselinesInsteadOfBursting(t *testing.T) {
	for _, loop := range []string{"single", "multiplex"} {
		t.Run(loop, func(t *testing.T) {
			waits, _ := driveTicker(t, loop, 1.7*60, 3)
			for _, w := range waits {
				if math.Abs(w-60) > 60*1e-6 {
					t.Fatalf("waits %v", waits)
				}
			}
		})
	}
}

// Whether the ticker goroutine is still running
func (r *tickerRun) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

func TestTickerSurvivesACorruptLedgerAtStartup(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "executions.db"), []byte(strings.Repeat("not a sqlite database", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &tickRecorder{}
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		return 0, nil
	})
	zero := 0.0
	run := newTickerRun().start(t, context.Background(), StartOptions{Interval: &zero})
	waitUntil(func() bool { return rec.count() >= 2 })
	alive := run.alive()
	run.stop()
	run.joined(5 * time.Second)
	if !alive || rec.count() < 2 {
		t.Fatal("ticker died on the pre-loop recovery scan")
	}
	if _, err := os.Stat(filepath.Join(h, "cron", "ticker_heartbeat")); err != nil {
		t.Fatal(err)
	}
}

func TestMultiplexTickerSurvivesARaisingGateWithoutTickingUngated(t *testing.T) {
	s, _ := newTestStore(t)
	logs := &lockedLog{}
	s.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	homes := profileHomes(t, t.TempDir(), "default")
	rec := &tickRecorder{}
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		return 0, nil
	})
	gateErrors := func() int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "level=ERROR") && strings.Contains(line, "gate boom") {
				n++
			}
		}
		return n
	}
	zero := 0.0
	run := newTickerRun().start(t, withCronStore(context.Background(), s), StartOptions{
		Interval: &zero, ProfileHomes: homes, ProfileGate: func(*string, string) bool { panic(errors.New("gate boom")) },
	})
	waitUntil(func() bool { return gateErrors() >= 2 })
	alive := run.alive()
	run.stop()
	run.joined(5 * time.Second)
	if !alive {
		t.Fatal("ticker thread died when profile_gate raised")
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("a raising gate must not tick the ungated home list, ticked %d", n)
	}
}

func TestMultiplexTickerProfileGateSkipsRejectedProfile(t *testing.T) {
	s, _ := newTestStore(t)
	homes := profileHomes(t, t.TempDir(), "own-gateway", "orphan")
	ownGateway, orphan := homes[0].Home, homes[1].Home
	rec := &tickRecorder{}
	run := newTickerRun()
	patchChronos(t, &tickHook, func(ctx context.Context, ps *Store, o tickOptions) (int, error) {
		rec.record(ctx, ps, o)
		if rec.count() >= 3 {
			run.stop()
		}
		return 0, nil
	})
	zero := 0.0
	run.start(t, withCronStore(context.Background(), s), StartOptions{
		Interval: &zero, ProfileHomes: homes, ProfileGate: func(name *string, _ string) bool { return *name != "own-gateway" },
	})
	if !run.joined(5 * time.Second) {
		run.stop()
		if !run.joined(5 * time.Second) {
			t.Fatal("ticker did not exit")
		}
	}
	_, ticked := rec.snapshot()
	set := map[string]bool{}
	for _, h := range ticked {
		set[h] = true
	}
	if len(set) != 1 || !set[orphan] {
		t.Fatalf("ticked %v", ticked)
	}
	if _, err := os.Stat(filepath.Join(ownGateway, "cron", "ticker_last_success")); err == nil {
		t.Fatal("the gated profile got a success marker")
	}
	if _, err := os.Stat(filepath.Join(orphan, "cron", "ticker_last_success")); err != nil {
		t.Fatal("the ticked profile missing its success marker")
	}
}
