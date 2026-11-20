package agent

import (
	"sync"
	"testing"
)

func TestLastCompressionAttemptRecordedReadsTheAttemptFlag(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.LastCompressionAttemptRecorded() {
		t.Fatal("a fresh agent reports a recorded attempt")
	}
	a.lastCompressionAttemptRecorded = true
	if !a.LastCompressionAttemptRecorded() {
		t.Fatal("recorded attempt not reported")
	}
}

func TestCompressionSkippedDueToLockKeepsBoolOrHolderText(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.CompressionSkippedDueToLock() != nil {
		t.Fatalf("fresh agent lock skip = %#v", a.CompressionSkippedDueToLock())
	}
	a.compressionSkippedDueToLock = true
	if a.CompressionSkippedDueToLock() != true {
		t.Fatalf("lock skip = %#v", a.CompressionSkippedDueToLock())
	}
	a.compressionSkippedDueToLock = "pid 42"
	if a.CompressionSkippedDueToLock() != "pid 42" {
		t.Fatalf("lock skip holder = %#v", a.CompressionSkippedDueToLock())
	}
}

func TestCompressionBlockedTransientReadsTheBlockReason(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.CompressionBlockedTransient() != nil {
		t.Fatalf("fresh agent block = %#v", a.CompressionBlockedTransient())
	}
	a.compressionBlockedTransient = "cooldown"
	if a.CompressionBlockedTransient() != "cooldown" {
		t.Fatalf("block = %#v", a.CompressionBlockedTransient())
	}
}

func TestLastCompressionAttemptInPlaceIsNoneUntilCommit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.LastCompressionAttemptInPlace() != nil {
		t.Fatal("fresh agent reports an in place outcome")
	}
	inPlace := false
	a.lastCompressionAttemptInPlace = &inPlace
	if got := a.LastCompressionAttemptInPlace(); got == nil || *got {
		t.Fatalf("in place = %#v", got)
	}
}

func TestLastCompressionTimedOutFollowsTheTimeoutOutcome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.LastCompressionTimedOut() {
		t.Fatal("fresh agent reports a timeout")
	}
	var wg sync.WaitGroup
	wg.Go(func() { markContextCompressionTimedOut(a) })
	wg.Wait()
	if !a.LastCompressionTimedOut() {
		t.Fatal("timeout marked on another goroutine is not mirrored")
	}
	resetContextCompressionTimeoutOutcome(a)
	if a.LastCompressionTimedOut() {
		t.Fatal("reset outcome still reports a timeout")
	}
}

func TestLastCompressionSummaryWarningReadsTheWarning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.LastCompressionSummaryWarning() != nil {
		t.Fatal("fresh agent reports a warning")
	}
	warning := "summary model unavailable"
	a.lastCompressionSummaryWarning = &warning
	if got := a.LastCompressionSummaryWarning(); got == nil || *got != warning {
		t.Fatalf("warning = %#v", got)
	}
}

func TestFallbackActivatedReadsTheActivationFlag(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{}
	if a.FallbackActivated() {
		t.Fatal("fresh agent reports an activated fallback")
	}
	a.fallbackActivated = true
	if !a.FallbackActivated() {
		t.Fatal("activated fallback not reported")
	}
}
