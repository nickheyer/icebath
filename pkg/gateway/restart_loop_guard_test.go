package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home for the restart loop guard with the boot log cleared
func isolateRestartLoopState(t *testing.T) context.Context {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, dir)
	ctx := context.Background()
	ClearRestartLoopGuard(ctx)
	return ctx
}

// Restart loop check at a fixed time with the default gap
func rlgCheck(t *testing.T, ctx context.Context, maxRestarts, window int, now float64, maxGap int) bool {
	t.Helper()
	tripped, err := RestartLoopCheckAndRecord(ctx, maxRestarts, window, &now, maxGap)
	if err != nil {
		t.Fatal(err)
	}
	return tripped
}

func TestRestartLoopGuardClearResets(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	rlgCheck(t, ctx, 3, 60, 1000.0, RestartLoopDefaultMaxGapSeconds)
	rlgCheck(t, ctx, 3, 60, 1001.0, RestartLoopDefaultMaxGapSeconds)
	ClearRestartLoopGuard(ctx)
	if rlgCheck(t, ctx, 3, 60, 1002.0, RestartLoopDefaultMaxGapSeconds) {
		t.Fatal("a cleared log must not trip")
	}
}

func TestRestartLoopGuardTripsOnSlowCrashCycleWiderThanWindow(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	for i, c := range []struct {
		now  float64
		want bool
	}{{1000.0, false}, {1150.0, false}, {1300.0, true}} {
		if got := rlgCheck(t, ctx, 3, 60, c.now, RestartLoopDefaultMaxGapSeconds); got != c.want {
			t.Fatalf("boot %d tripped=%v", i, got)
		}
	}
}

func TestRestartLoopGuardSlowCycleChainIsPersistedNotTruncated(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	var boots []float64
	for _, now := range []float64{1000.0, 1150.0, 1300.0} {
		var err error
		if boots, err = RecordRestartInterruptedBoot(ctx, 60, &now, RestartLoopDefaultMaxGapSeconds); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(boots, []float64{1000.0, 1150.0, 1300.0}) {
		t.Fatalf("boots = %v", boots)
	}
}

func TestRestartLoopGuardQuietPeriodBreaksTheChain(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	rlgCheck(t, ctx, 3, 60, 1000.0, RestartLoopDefaultMaxGapSeconds)
	rlgCheck(t, ctx, 3, 60, 1150.0, RestartLoopDefaultMaxGapSeconds)
	if rlgCheck(t, ctx, 3, 60, 4800.0, RestartLoopDefaultMaxGapSeconds) {
		t.Fatal("a boot after real quiet must start a fresh chain")
	}
}

func TestRestartLoopGuardFastRespawnLoopStillTrips(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	for i, c := range []struct {
		now  float64
		want bool
	}{{1000.0, false}, {1010.0, false}, {1020.0, true}} {
		if got := rlgCheck(t, ctx, 3, 60, c.now, RestartLoopDefaultMaxGapSeconds); got != c.want {
			t.Fatalf("boot %d tripped=%v", i, got)
		}
	}
}

func TestRestartLoopGuardMaxGapSecondsIsConfigurable(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	for _, now := range []float64{1000.0, 1150.0, 1300.0} {
		if rlgCheck(t, ctx, 3, 60, now, 100) {
			t.Fatalf("tripped at %v with a 100s gap", now)
		}
	}
}

func TestRestartLoopGuardWindowSecondsFloorsTheGap(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	for i, c := range []struct {
		now  float64
		want bool
	}{{1000.0, false}, {1400.0, false}, {1800.0, true}} {
		if got := rlgCheck(t, ctx, 3, 900, c.now, 100); got != c.want {
			t.Fatalf("boot %d tripped=%v", i, got)
		}
	}
}

func TestRestartLoopGuardDisabledBreakerNeverTrips(t *testing.T) {
	ctx := isolateRestartLoopState(t)
	for _, now := range []float64{1000.0, 1150.0, 1300.0, 1450.0} {
		if rlgCheck(t, ctx, 0, 60, now, RestartLoopDefaultMaxGapSeconds) {
			t.Fatal("a disabled breaker tripped")
		}
	}
}
