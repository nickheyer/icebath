package gatewayrun

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Agent whose last activity carries the description and provenance, aged by the given seconds
func provenanceAgent(idleSeconds float64, description, provenance string) *agent.AIAgent {
	a := &agent.AIAgent{}
	a.TouchActivityAs(description, provenance)
	a.SetLastActivityTS(float64(time.Now().UnixNano())/1e9 - idleSeconds)
	return a
}

// test_session_stall_watchdog.py test_session_stall_watcher_disabled_when_timeout_zero
func TestSessionStallWatcherDisabledWhenTimeoutZero(t *testing.T) {
	r, a := runnerForStall(t)
	t.Setenv("ICEBATH_SESSION_STALL_TIMEOUT", "0")
	key := "agent:main:telegram:dm:6"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(999)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.sessionStallWatcher(ctx, 1) }()
	time.Sleep(1300 * time.Millisecond)
	if got := r.sessionStallTimeoutSeconds(); got != 0 {
		t.Fatalf("timeout %v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sent) != 0 {
		t.Fatalf("disabled watcher sent %q", a.sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_logs_compression_provenance
func TestCheckSessionStallsLogsCompressionProvenance(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:compress"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = provenanceAgent(120, "compressing context", "agent.compression")
	logs := captureLogRecords(t, slog.LevelWarn)
	if sent, err := r.checkSessionStalls(context.Background(), 60); err != nil || sent != 1 {
		t.Fatalf("sent %d %v", sent, err)
	}
	messages := strings.Join(logs.messages(slog.LevelWarn), "\n")
	if !strings.Contains(messages, "agent.compression") || !strings.Contains(messages, "compressing context") {
		t.Fatalf("warnings %q", messages)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_skips_active_compression_heartbeat
func TestCheckSessionStallsSkipsActiveCompressionHeartbeat(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:compacting"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = provenanceAgent(5, "context compression in progress", "agent.compression")
	if sent, err := r.checkSessionStalls(context.Background(), 60); err != nil || sent != 0 {
		t.Fatalf("sent %d %v", sent, err)
	}
	if len(a.sent) != 0 || r.sessionStallNotified[key] {
		t.Fatalf("sent %q latched %v", a.sent, r.sessionStallNotified[key])
	}
}
