package gatewayrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Adapter recording every sent notice
type stallAdapter struct {
	*gateway.BasePlatformAdapter
	mu        sync.Mutex
	sent      []string
	calls     int
	failFirst string
}

func (a *stallAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *stallAdapter) Disconnect(context.Context) error            { return nil }
func (a *stallAdapter) Send(_ context.Context, _ string, content string, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 && a.failFirst == "raise" {
		return gateway.SendResult{}, errors.New("boom")
	}
	if a.calls == 1 && a.failFirst == "soft" {
		return gateway.SendResult{Success: false, Error: "chat not found"}, nil
	}
	a.sent = append(a.sent, content)
	return gateway.SendResult{Success: true}, nil
}
func (a *stallAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Runner holding one stall adapter as its only live adapter
func runnerForStall(t *testing.T) (*GatewayRunner, *stallAdapter) {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	a := &stallAdapter{}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, a)
	r := allocGatewayRunner()
	r.running.Store(true)
	r.adapters = adapterDict(gateway.PlatformTelegram, gateway.PlatformAdapter(a))
	r.sessionStallNotified = map[string]bool{}
	return r, a
}

// Pending follow up event of a Telegram chat
func pendingStallEvent() *gateway.MessageEvent {
	event := gateway.NewMessageEvent("follow-up")
	event.Source = &gateway.SessionSource{ChatID: "chat-1", Platform: gateway.PlatformTelegram}
	return event
}

// test_session_stall_watchdog.py test_check_session_stalls_notifies_once
func TestCheckSessionStallsNotifiesOnce(t *testing.T) {
	r, a := runnerForStall(t)
	t.Setenv("ICEBATH_SESSION_STALL_TIMEOUT", "60")
	key := "agent:main:telegram:dm:1"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(120)
	ctx := context.Background()
	if sent, err := r.checkSessionStalls(ctx, 60); err != nil || sent != 1 {
		t.Fatalf("sent = %d, %v", sent, err)
	}
	if len(a.sent) != 1 || !strings.Contains(a.sent[0], "/new") || !r.sessionStallNotified[key] {
		t.Fatalf("sent %q latched %v", a.sent, r.sessionStallNotified[key])
	}
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 0 || len(a.sent) != 1 {
		t.Fatalf("second pass sent %d", sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_skips_fresh_activity
func TestCheckSessionStallsSkipsFreshActivity(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:2"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(5)
	if sent, _ := r.checkSessionStalls(context.Background(), 60); sent != 0 || len(a.sent) != 0 {
		t.Fatalf("sent %d %q", sent, a.sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_skips_without_pending
func TestCheckSessionStallsSkipsWithoutPending(t *testing.T) {
	r, a := runnerForStall(t)
	r.sessionState("agent:main:telegram:dm:3").turn.agent = idleAgent(999)
	if sent, _ := r.checkSessionStalls(context.Background(), 60); sent != 0 || len(a.sent) != 0 {
		t.Fatalf("sent %d %q", sent, a.sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_clears_latch_when_pending_drains
func TestCheckSessionStallsClearsLatchWhenPendingDrains(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:4"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(200)
	ctx := context.Background()
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 1 || !r.sessionStallNotified[key] {
		t.Fatalf("first pass sent %d", sent)
	}
	a.PopPendingMessage(key)
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 0 {
		t.Fatalf("drained pass sent %d", sent)
	}
	if _, latched := r.sessionStallNotified[key]; latched {
		t.Fatal("latch must clear once pending drained")
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_skips_pending_sentinel_without_activity
func TestCheckSessionStallsSkipsPendingSentinelWithoutActivity(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:5"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = agentPendingSentinel
	r.runningAgentsTS().Set(key, float64(time.Now().Unix()-90))
	if sent, _ := r.checkSessionStalls(context.Background(), 60); sent != 0 {
		t.Fatalf("sent %d", sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_queued_events_overflow_notifies
func TestCheckSessionStallsQueuedEventsOverflowNotifies(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:overflow"
	r.queuedEvents().Set(key, []*gateway.MessageEvent{pendingStallEvent()})
	r.sessionState(key).turn.agent = idleAgent(120)
	if sent, err := r.checkSessionStalls(context.Background(), 60); err != nil || sent != 1 {
		t.Fatalf("sent = %d, %v", sent, err)
	}
	if len(a.sent) == 0 || !strings.Contains(a.sent[0], "/new") {
		t.Fatalf("sent %q", a.sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_scans_profile_adapters
func TestCheckSessionStallsScansProfileAdapters(t *testing.T) {
	r, a := runnerForStall(t)
	r.adapters = adapterDict(gateway.PlatformTelegram, nil)
	r.adapters.Delete(gateway.PlatformTelegram)
	r.profileAdapters = profileAdapterDict("coder", adapterDict(gateway.PlatformTelegram, gateway.PlatformAdapter(a)))
	key := "agent:coder:telegram:dm:1"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(120)
	if sent, err := r.checkSessionStalls(context.Background(), 60); err != nil || sent != 1 || len(a.sent) != 1 {
		t.Fatalf("sent = %d, %v, %q", sent, err, a.sent)
	}
}

// Runs the retry after a failed first send for a failure mode
func checkStallRetryAfterFailure(t *testing.T, mode, key string) {
	r, a := runnerForStall(t)
	a.failFirst = mode
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(120)
	ctx := context.Background()
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 0 {
		t.Fatalf("failed send counted %d", sent)
	}
	if _, latched := r.sessionStallNotified[key]; latched {
		t.Fatal("a failed send must not latch")
	}
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 1 || !r.sessionStallNotified[key] || len(a.sent) != 1 {
		t.Fatalf("retry sent %d latched %v %q", sent, r.sessionStallNotified[key], a.sent)
	}
}

// test_session_stall_watchdog.py test_check_session_stalls_retries_after_send_failure
func TestCheckSessionStallsRetriesAfterSendFailure(t *testing.T) {
	checkStallRetryAfterFailure(t, "raise", "agent:main:telegram:dm:retry")
}

// test_session_stall_watchdog.py test_check_session_stalls_retries_after_soft_send_failure
func TestCheckSessionStallsRetriesAfterSoftSendFailure(t *testing.T) {
	checkStallRetryAfterFailure(t, "soft", "agent:main:telegram:dm:soft")
}

// test_session_stall_watchdog.py test_check_session_stalls_renotifies_after_resume_then_restall
func TestCheckSessionStallsRenotifiesAfterResumeThenRestall(t *testing.T) {
	r, a := runnerForStall(t)
	key := "agent:main:telegram:dm:episode"
	a.MergePendingMessage(key, pendingStallEvent(), false)
	r.sessionState(key).turn.agent = idleAgent(120)
	ctx := context.Background()
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 1 {
		t.Fatalf("first episode sent %d", sent)
	}
	r.sessionState(key).turn.agent = idleAgent(5)
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 0 {
		t.Fatalf("resumed pass sent %d", sent)
	}
	if _, latched := r.sessionStallNotified[key]; latched {
		t.Fatal("resumed activity must clear the latch")
	}
	r.sessionState(key).turn.agent = idleAgent(120)
	if sent, _ := r.checkSessionStalls(ctx, 60); sent != 1 || len(a.sent) != 2 {
		t.Fatalf("second episode sent %d %q", sent, a.sent)
	}
}
