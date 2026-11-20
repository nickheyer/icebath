//go:build !windows

package tuiserver

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestExitFlushIsBounded(t *testing.T) {
	a := newFlushAgent("unflushed turn")
	a.persist = func([]*state.Message) { time.Sleep(5 * time.Second) }
	registerFlushSession(t, "sess-hang", a, nil)
	budget := 0.3
	start := time.Now()
	flushSessionsBeforeExit(&budget)
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("exit flush blocked %.1fs past its budget", elapsed.Seconds())
	}
}

func TestExitFlushNeverWaitsOnAnExternalSecretSource(t *testing.T) {
	launch, served := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	a := newFlushAgent("hi")
	registerFlushSession(t, "sid", a, map[string]any{"profile_home": served})
	budget := 1.0
	start := time.Now()
	if got := flushSessionsBeforeExit(&budget); got != 1 {
		t.Fatalf("flushed %d transcripts, want 1", got)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("the flush waited %.1fs on something other than the transcript", elapsed.Seconds())
	}
}
