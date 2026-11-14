package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/kanban"
)

// Base adapter whose send lands on a wire, everything else the real base lifecycle
type wireAdapter struct {
	*gateway.BasePlatformAdapter
	mu   sync.Mutex
	wire []string
}

func (a *wireAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *wireAdapter) Disconnect(context.Context) error            { return nil }
func (a *wireAdapter) Send(_ context.Context, _, content, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.wire = append(a.wire, content)
	return gateway.SendResult{Success: true, MessageID: len(a.wire)}, nil
}
func (a *wireAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

// Sent wire lines
func (a *wireAdapter) lines() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.wire...)
}

// Wire route of one Telegram dm, its runner, adapter and session key
func setupWireRoute(t *testing.T) (*GatewayRunner, *wireAdapter, string) {
	t.Helper()
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	a := &wireAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, a)
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformTelegram, a)
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r.kanbanDispatcherLockHandle = f
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "42", UserID: kstr("42"), ChatType: "dm"}
	key, err := gateway.BuildSessionKey(context.Background(), source, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return r, a, key
}

// One collect and deliver pass sharing the failure counts
func acceptanceTick(t *testing.T, r *GatewayRunner, failures map[kanbanSubKey]int) {
	t.Helper()
	ctx := context.Background()
	rows, err := notifierCollect(ctx, r, "", false, 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := newKanbanNotification(ctx, r, row, failures).deliver(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// Waits until cond holds or fails the test after five seconds
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNotifierRetriesUnacceptedWakeWithoutRepeatingPings(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(t.TempDir(), "board.db"))
	r, adapter, key := setupWireRoute(t)
	conn := kanbanTestConn(t)
	tids := map[string]string{}
	for _, mode := range []string{"notify+wake", "wake", "notify"} {
		tid := kanbanCreate(t, conn, mode, "worker", kstr(key))
		kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "42", UserID: kstr("42"),
			ChatType: kstr("dm"), DeliveryMode: kstr(mode)})
		kanbanComplete(t, conn, tid, "handoff")
		tids[mode] = tid
	}
	unseen := func(mode string) bool {
		_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tids[mode], "telegram", "42", nil, []string{"completed"})
		if err != nil {
			t.Fatal(err)
		}
		return len(events) > 0
	}
	failures := map[kanbanSubKey]int{}
	acceptanceTick(t, r, failures)
	if len(adapter.lines()) != 2 || !unseen("notify+wake") || !unseen("wake") || unseen("notify") {
		t.Fatalf("wire %v", adapter.lines())
	}
	for range 13 {
		acceptanceTick(t, r, failures)
	}
	if len(adapter.lines()) != 2 || !unseen("notify+wake") || !unseen("wake") || len(failures) != 0 {
		t.Fatalf("wire %v failures %v", adapter.lines(), failures)
	}
	var mu sync.Mutex
	var received []string
	adapter.SetMessageHandler(func(_ context.Context, event *gateway.MessageEvent) (any, error) {
		mu.Lock()
		received = append(received, event.Text)
		mu.Unlock()
		return nil, nil
	})
	acceptanceTick(t, r, failures)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(received) >= 1 })
	waitFor(t, func() bool { return !adapter.HasActiveSession(key) })
	acceptanceTick(t, r, failures)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(received) >= 2 })
	waitFor(t, func() bool { return !adapter.HasActiveSession(key) })
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("received %v", received)
	}
	for _, text := range received {
		if !strings.Contains(text, "handoff") {
			t.Fatalf("wake %q", text)
		}
	}
	if len(adapter.lines()) != 2 || unseen("notify+wake") || unseen("wake") || unseen("notify") {
		t.Fatalf("wire %v", adapter.lines())
	}
}

func TestSuppressedPingHasNoSentReceiptButWakeExecutes(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(h, "board.db"))
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display: {suppress_warning_notifications: true}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, adapter, key := setupWireRoute(t)
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "failure", "worker", kstr(key))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "42", UserID: kstr("42"),
		ChatType: kstr("dm"), DeliveryMode: kstr("notify+wake")})
	if _, err := kanban.BlockTaskWithKind(context.Background(), conn, tid, kstr("worker failure"), kstr("transient"), nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var work []*gateway.MessageEvent
	adapter.SetMessageHandler(func(_ context.Context, event *gateway.MessageEvent) (any, error) {
		mu.Lock()
		work = append(work, event)
		mu.Unlock()
		return "technical diagnostic echo", nil
	})
	acceptanceTick(t, r, map[kanbanSubKey]int{})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(work) >= 1 })
	mu.Lock()
	if len(work) != 1 || work[0].Metadata["notification_category"] != "diagnostic" {
		mu.Unlock()
		t.Fatalf("work %v", work)
	}
	mu.Unlock()
	waitFor(t, func() bool { return !adapter.HasActiveSession(key) })
	if lines := adapter.lines(); len(lines) != 0 {
		t.Fatalf("wire %v", lines)
	}
	var lastPing, lastEvent int64
	if err := conn.QueryRowContext(context.Background(), "SELECT last_ping_event_id, last_event_id FROM kanban_notify_subs WHERE task_id=?", tid).Scan(&lastPing, &lastEvent); err != nil {
		t.Fatal(err)
	}
	if lastPing != 0 || lastEvent <= 0 {
		t.Fatalf("ping %d event %d", lastPing, lastEvent)
	}
}
