package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hermetic home whose config.yaml holds body
func kanbanConfigHome(t *testing.T, body string) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

// Bare running runner with an optional Telegram adapter and no dispatcher lock
func makeGateRunner(withAdapter bool) *GatewayRunner {
	r := allocGatewayRunner()
	r.running.Store(true)
	if withAdapter {
		r.adapters = adapterDict(gateway.PlatformTelegram, newKanbanRecordingAdapter())
	} else {
		r.adapters = adapterDict()
	}
	r.kanbanSubFailCounts = map[kanbanSubKey]int{}
	return r
}

func TestNotifierWatcherSkipsWhenNotificationsDisabled(t *testing.T) {
	kanbanConfigHome(t, "kanban:\n  notify_in_gateway: false\n")
	r := makeGateRunner(true)
	listed, slept := 0, 0
	prevList, prevSleep := kanbanListAllBoards, asyncioSleep
	t.Cleanup(func() { kanbanListAllBoards, asyncioSleep = prevList, prevSleep })
	kanbanListAllBoards = func(bool) ([]*pyjson.Object, error) { listed++; return nil, nil }
	asyncioSleep = func(context.Context, float64) error { slept++; return nil }
	if err := r.kanbanNotifierWatcher(context.Background(), 5.0); err != nil {
		t.Fatal(err)
	}
	if listed != 0 || slept != 0 {
		t.Fatalf("listed %d slept %d", listed, slept)
	}
}

func TestNotifierWatcherPollsWithoutDispatchOwnership(t *testing.T) {
	kanbanConfigHome(t, "kanban:\n  dispatch_in_gateway: false\n  notify_in_gateway: true\n")
	r := makeGateRunner(true)
	var pastGate []bool
	var sleepCalls []float64
	prevList, prevSleep := kanbanListAllBoards, asyncioSleep
	t.Cleanup(func() { kanbanListAllBoards, asyncioSleep = prevList, prevSleep })
	kanbanListAllBoards = func(bool) ([]*pyjson.Object, error) { pastGate = append(pastGate, true); return nil, nil }
	asyncioSleep = func(_ context.Context, delay float64) error {
		sleepCalls = append(sleepCalls, delay)
		if len(sleepCalls) >= 2 {
			r.running.Store(false)
		}
		return nil
	}
	if err := r.kanbanNotifierWatcher(context.Background(), 5.0); err != nil {
		t.Fatal(err)
	}
	if len(pastGate) == 0 {
		t.Fatal("gateways without the dispatch lock must still poll owned subscriptions")
	}
}

func TestDisabledWhenFlagFalse(t *testing.T) {
	enabled, _, err := resolveAutoDecomposeSettings(func() (config.Tree, error) {
		return config.Tree{"kanban": map[string]any{"auto_decompose": false}}, nil
	})
	if err != nil || enabled {
		t.Fatalf("enabled %v err %v", enabled, err)
	}
}

func TestAutoDecomposeTickReadsLaunchProfileSecretsUnderMultiplex(t *testing.T) {
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("ANTHROPIC_API_KEY=launch-profile-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	prevSlugs, prevList, prevTask := dispatcherBoardSlugs, kanbanDecomposeListTriageIDs, kanbanDecomposeTask
	t.Cleanup(func() {
		dispatcherBoardSlugs, kanbanDecomposeListTriageIDs, kanbanDecomposeTask = prevSlugs, prevList, prevTask
	})
	dispatcherBoardSlugs = func() ([]string, error) { return []string{"default"}, nil }
	kanbanDecomposeListTriageIDs = func(context.Context, *string) ([]string, error) { return []string{"t1"}, nil }
	seen := ""
	kanbanDecomposeTask = func(ctx context.Context, taskID string, author *string, timeout *int64) (*kanban.DecomposeOutcome, error) {
		seen, _, _ = provider.GetSecret(ctx, "ANTHROPIC_API_KEY")
		return &kanban.DecomposeOutcome{OK: true}, nil
	}
	d := newKanbanDispatcher(dispatcherSettings{interval: 60.0, failureLimit: 2, reconcileOrphans: true})
	provider.SetMultiplexActive(true)
	decomposed, err := func() (int, error) {
		defer provider.SetMultiplexActive(false)
		return toThreadProcessService(context.Background(), func(fresh context.Context) (int, error) {
			return d.autoDecomposeTick(fresh, 5)
		})
	}()
	if err != nil || decomposed != 1 {
		t.Fatalf("decomposed %d err %v", decomposed, err)
	}
	if seen != "launch-profile-key" {
		t.Fatalf("secret %q", seen)
	}
	if scope := provider.CurrentSecretScope(context.Background()); scope != nil {
		t.Fatalf("scope leaked %v", scope)
	}
}

func TestZeroSubBoardIsNeverOpenedWritable(t *testing.T) {
	kanbanTestDB(t, "zero-subs.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "owner gate", "worker", nil)
	kanbanComplete(t, conn, tid, "done")

	adapter := newKanbanRecordingAdapter()
	r := makeKanbanRunner(t, adapter)
	r.kanbanDispatcherLockHandle = nil
	connects := 0
	prev := kanbanConnect
	t.Cleanup(func() { kanbanConnect = prev })
	kanbanConnect = func(ctx context.Context, dbPath string, board *string) (state.SQLiteConn, error) {
		connects++
		return prev(ctx, dbPath, board)
	}
	runOneNotifierTick(t, r)
	if connects != 0 || len(adapter.sent) != 0 {
		t.Fatalf("connects %d sent %d", connects, len(adapter.sent))
	}
}

func TestKanbanDispatchBlockedWhenEngaged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if !kanbanDispatchAllowed(ctx) {
		t.Fatal("dispatch blocked before engage")
	}
	tools.Engage(ctx, "test")
	if kanbanDispatchAllowed(ctx) {
		t.Fatal("dispatch allowed while engaged")
	}
	tools.Disengage(ctx)
	if !kanbanDispatchAllowed(ctx) {
		t.Fatal("dispatch blocked after disengage")
	}
}

func TestDispatcherToThreadDoesNotTripKanbanGuard(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := tools.WithDelegatedChildContext(context.Background(), true)
	if !tools.IsDelegatedChildContext(ctx) {
		t.Fatal("child marker not set")
	}
	err := kanban.AssertNotDelegatedChildMutation(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "delegate_task child") {
		t.Fatalf("guard %v", err)
	}
	if _, err := toThreadProcessService(ctx, func(fresh context.Context) (struct{}, error) {
		return struct{}{}, kanban.AssertNotDelegatedChildMutation(fresh, "")
	}); err != nil {
		t.Fatal(err)
	}
	if !tools.IsDelegatedChildContext(ctx) {
		t.Fatal("child marker lost")
	}
}
