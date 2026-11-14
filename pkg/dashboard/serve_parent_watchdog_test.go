package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysignal"
)

func markerProbe(marker string) func(int) (string, error) {
	return func(int) (string, error) { return marker, nil }
}

func strPtr(s string) *string { return &s }

func TestParentWatchdogTracksRecordedDesktopPidNotImmediatePpid(t *testing.T) {
	if isServeOrphaned(4242, nil, func(pid int) bool { return pid == 4242 }, nil) {
		t.Fatal("a live recorded desktop pid must not be orphaned")
	}
	if !isServeOrphaned(4242, nil, func(int) bool { return false }, nil) {
		t.Fatal("a dead recorded desktop pid must be orphaned")
	}
}

func TestParentWatchdogFailsSafeWhenLivenessProbeErrors(t *testing.T) {
	broken := func(int) bool { panic(errors.New("process table temporarily unavailable")) }
	if isServeOrphaned(4242, nil, broken, nil) {
		t.Fatal("a failing liveness probe must keep serving")
	}
}

func TestParentWatchdogAcceptsElectronWindowsCreationTimeMarker(t *testing.T) {
	unixMS := int64(1_723_456_789_123)
	ticks := int64(621_355_968_000_000_000) + unixMS*10_000 + 9_999
	if !validParentStartMarker(fmt.Sprintf("winms:%d", unixMS)) {
		t.Fatal("winms marker must be valid")
	}
	if isServeOrphaned(4242, strPtr(fmt.Sprintf("winms:%d", unixMS)), nil, markerProbe(fmt.Sprintf("win:%d", ticks))) {
		t.Fatal("matching creation time must not be orphaned")
	}
}

func TestParentWatchdogRejectsReusedPidWithDifferentWindowsCreationTime(t *testing.T) {
	unixMS := int64(1_723_456_789_123)
	next := int64(621_355_968_000_000_000) + (unixMS+1)*10_000
	if !isServeOrphaned(4242, strPtr(fmt.Sprintf("winms:%d", unixMS)), nil, markerProbe(fmt.Sprintf("win:%d", next))) {
		t.Fatal("a reused pid must be orphaned")
	}
}

func TestParentWatchdogPreservesLegacyExactWindowsMarker(t *testing.T) {
	marker := "win:638908765432109876"
	if isServeOrphaned(4242, &marker, nil, markerProbe(marker)) {
		t.Fatal("an exact legacy marker must not be orphaned")
	}
}

func TestParentWatchdogDoesNotKillALiveParentOnMacosTimezoneDrift(t *testing.T) {
	expected, actual := "ps:Thu Aug 20 22:33:11 2026", "ps:Fri Aug 21 04:33:11 2026"
	if isServeOrphaned(4242, &expected, func(int) bool { return true }, markerProbe(actual)) {
		t.Fatal("a ps marker drift must degrade to pid liveness")
	}
}

func TestParentWatchdogStillDetectsAGenuinelyDeadParentDespitePsMarkerMismatch(t *testing.T) {
	expected, actual := "ps:Thu Aug 20 22:33:11 2026", "ps:Fri Aug 21 04:33:11 2026"
	if !isServeOrphaned(4242, &expected, func(int) bool { return false }, markerProbe(actual)) {
		t.Fatal("a dead parent must be orphaned")
	}
}

func TestParentWatchdogExactPsMarkerMatchStillShortCircuits(t *testing.T) {
	marker := "ps:Thu Aug 20 22:33:11 2026"
	if isServeOrphaned(4242, &marker, func(int) bool { return false }, markerProbe(marker)) {
		t.Fatal("an exact ps match must short circuit")
	}
}

func TestParentWatchdogStillRejectsRecycledPidViaStableLinuxMarker(t *testing.T) {
	if !isServeOrphaned(4242, strPtr("linux:12345"), func(int) bool { return true }, markerProbe("linux:99999")) {
		t.Fatal("a linux marker mismatch must be conclusive")
	}
	if !parentStartMarkerMismatchIsConclusive("linux:123", "linux:456") || !parentStartMarkerMismatchIsConclusive("win:1", "winms:2") {
		t.Fatal("machine marker mismatches must be conclusive")
	}
}

func TestMacosPsMarkerRequiresFullLstartNotATruncatedWeekday(t *testing.T) {
	if !validParentStartMarker("ps:Sat Aug 29 15:04:31 2026") {
		t.Fatal("a full lstart must be valid")
	}
	if validParentStartMarker("ps:Sat") || validParentStartMarker("ps:Sat Aug 29") {
		t.Fatal("a truncated lstart must be invalid")
	}
}

func TestParentWatchdogTreatsEmptyMarkerEnvAsAbsent(t *testing.T) {
	t.Setenv("ICEBATH_PARENT_PID", "4242")
	t.Setenv("ICEBATH_PARENT_START_MARKER", "")
	t.Setenv("ICEBATH_PARENT_NONCE", "")
	pid, marker, _, armed := parentDeathWatchdogArgs()
	if !armed || pid != 4242 || marker != nil {
		t.Fatalf("args = %d %v %v, want 4242 nil armed", pid, marker, armed)
	}
}

func TestParentWatchdogWarnsWhenDisarmedByUnusableMarker(t *testing.T) {
	t.Setenv("ICEBATH_PARENT_PID", "4242")
	t.Setenv("ICEBATH_PARENT_START_MARKER", "ps:Sat")
	t.Setenv("ICEBATH_PARENT_NONCE", "n")
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, _, _, armed := parentDeathWatchdogArgs(); armed {
		t.Fatal("an unusable marker must disarm the watchdog")
	}
	if !strings.Contains(buf.String(), "watchdog disabled") {
		t.Fatalf("missing disarm warning in %q", buf.String())
	}
}

func TestParentWatchdogDegradesToPidLivenessWhenMarkerProbeRaisesOserror(t *testing.T) {
	broken := func(pid int) (string, error) {
		return "", fmt.Errorf("ps could not inspect PID %d: process table temporarily unavailable", pid)
	}
	marker := "ps:Thu Aug 20 22:33:11 2026"
	if !isServeOrphaned(4242, &marker, func(int) bool { return false }, broken) {
		t.Fatal("a failed probe must fall through to a dead pid")
	}
	if isServeOrphaned(4242, &marker, func(int) bool { return true }, broken) {
		t.Fatal("a failed probe must fall through to a live pid")
	}
	lookup := func(pid int) (string, error) { return "", &ProcessLookupError{PID: pid} }
	if !isServeOrphaned(4242, &marker, func(int) bool { return true }, lookup) {
		t.Fatal("ProcessLookupError must be conclusive")
	}
}

// Catches SIGTERM raised inside the test process for the duration of the test
func catchSIGTERM(t *testing.T) chan os.Signal {
	ch := make(chan os.Signal, 4)
	prev := pysignal.Signal(syscall.SIGTERM, pysignal.Func(func(sig os.Signal) error { ch <- sig; return nil }))
	t.Cleanup(func() { pysignal.Signal(syscall.SIGTERM, prev) })
	return ch
}

func TestParentWatchdogShutsDownThroughTheSignalPathNotOsExit(t *testing.T) {
	ch := catchSIGTERM(t)
	ceiling := parentDeathLoop(4242, nil, 2.0, func(int, *string) bool { return true })
	stopped := ceiling.Stop()
	select {
	case sig := <-ch:
		if sig != syscall.SIGTERM {
			t.Fatalf("got %v, want SIGTERM", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent loss must run this process's shutdown handlers")
	}
	if !stopped {
		t.Fatal("the watchdog must not bypass those handlers with a hard exit")
	}
}

func TestParentWatchdogArmsADaemonHardExitCeiling(t *testing.T) {
	ch := catchSIGTERM(t)
	ceiling := requestOrphanShutdown()
	defer ceiling.Stop()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM must be raised")
	}
	if orphanExitCeiling <= 0 || orphanExitCeiling > 30*time.Second {
		t.Fatalf("ceiling %v out of range", orphanExitCeiling)
	}
	if !ceiling.Stop() {
		t.Fatal("the ceiling timer must be armed")
	}
}
