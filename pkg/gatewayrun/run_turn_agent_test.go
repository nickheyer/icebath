package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

var cleanupCaptureNextMid atomic.Int64

// Adapter recording sends, edits and deletions with minted message ids
type cleanupCaptureAdapter struct {
	*gateway.BasePlatformAdapter
	mu      sync.Mutex
	sent    []map[string]any
	edits   []map[string]any
	deleted []map[string]any
}

func (a *cleanupCaptureAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *cleanupCaptureAdapter) Disconnect(context.Context) error            { return nil }
func (a *cleanupCaptureAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *cleanupCaptureAdapter) Send(_ context.Context, chatID, content, _ string, metadata map[string]any) (gateway.SendResult, error) {
	mid := strconv.FormatInt(100+cleanupCaptureNextMid.Add(1), 10)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, map[string]any{"chat_id": chatID, "content": content, "message_id": mid, "metadata": metadata})
	return gateway.SendResult{Success: true, MessageID: mid}, nil
}
func (a *cleanupCaptureAdapter) EditMessage(_ context.Context, chatID, messageID, content string, _ bool) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.edits = append(a.edits, map[string]any{"chat_id": chatID, "message_id": messageID, "content": content})
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}
func (a *cleanupCaptureAdapter) DeleteMessage(_ context.Context, chatID, messageID string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleted = append(a.deleted, map[string]any{"chat_id": chatID, "message_id": messageID})
	return true, nil
}
func (a *cleanupCaptureAdapter) SendTyping(context.Context, string, map[string]any) error { return nil }
func (a *cleanupCaptureAdapter) StopTyping(context.Context, string, map[string]any) error {
	return nil
}

func (a *cleanupCaptureAdapter) deletedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.deleted)
}

func newCleanupCaptureAdapter() *cleanupCaptureAdapter {
	a := &cleanupCaptureAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformTelegram, a)
	return a
}

// Bare runner serving the adapter on a running gateway loop, the _make_runner of the hermes cases
func makeTurnRunner(t *testing.T, adapter gateway.PlatformAdapter) *GatewayRunner {
	r := allocGatewayRunner()
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	r.adapters.Set(adapter.Base().Platform, adapter)
	loop := gateway.NewCallbackLoop()
	loopCtx, stop := context.WithCancel(context.Background())
	go func() { _ = loop.Run(loopCtx) }()
	t.Cleanup(func() {
		stop()
		loop.Close()
	})
	r.gatewayLoop = loop
	r.hooks = NewProfileHookRegistries()
	return r
}

// Home whose config points at the scripted provider with the extra config appended
func installTurnFakes(t *testing.T, server *fakellm.Server, extraConfig string) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TOOL_PROGRESS_MODE", "all")
	if err := fakellm.WriteHome(home, server.BaseURL(), "", extraConfig, 0); err != nil {
		t.Fatal(err)
	}
}

// test_run_cleanup_progress.py test_messaging_agent_forwards_checkpoint_config
func TestMessagingAgentForwardsCheckpointConfig(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "done"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	installTurnFakes(t, server, "checkpoints:\n  enabled: true\n  max_snapshots: 9\n  max_total_size_mb: 444\n  max_file_size_mb: 6\n")
	adapter := newCleanupCaptureAdapter()
	r := makeTurnRunner(t, adapter)
	r.agentCache, r.agentCacheLock = pydict.New[string, agentCacheEntry](), &sync.Mutex{}
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001"}
	sessionKey := "agent:main:telegram:group:-1001"
	result, err := r.runAgent(context.Background(), runAgentParams{message: "hello", source: source, sessionID: "sess-checkpoints", sessionKey: sessionKey})
	if err != nil {
		t.Fatal(err)
	}
	if result["final_response"] != "done" {
		t.Fatalf("final_response = %#v", result["final_response"])
	}
	a := r.cachedAgentFor(sessionKey, true)
	if a == nil {
		t.Fatal("no agent was built for the session")
	}
	t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
	mgr := a.CheckpointManager()
	if mgr == nil || !mgr.Enabled || mgr.MaxSnapshots != 9 || mgr.MaxTotalSizeMB != 444 || mgr.MaxFileSizeMB != 6 {
		t.Fatalf("checkpoint manager = %#v", mgr)
	}
}

// test_run_cleanup_progress.py test_cleanup_chains_with_existing_callback
func TestCleanupChainsWithExistingCallback(t *testing.T) {
	server, err := fakellm.New([]any{
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "pwd"}},
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "ls"}},
		fakellm.Text{Text: "done"},
	}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	installTurnFakes(t, server, "display:\n  platforms:\n    telegram:\n      cleanup_progress: true\n")
	adapter := newCleanupCaptureAdapter()
	r := makeTurnRunner(t, adapter)
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-1001"}
	sessionKey := "agent:main:telegram:group:-1001"
	var preExistingFired atomic.Int32
	adapter.RegisterPostDeliveryCallback(sessionKey, func(context.Context) error {
		preExistingFired.Add(1)
		return nil
	}, nil)
	result, err := r.runAgent(context.Background(), runAgentParams{message: "hello", source: source, sessionID: "sess-1", sessionKey: sessionKey})
	if err != nil {
		t.Fatal(err)
	}
	if result["final_response"] != "done" {
		t.Fatalf("final_response = %#v", result["final_response"])
	}
	cb := adapter.PopPostDeliveryCallback(sessionKey, nil)
	if cb == nil {
		t.Fatal("no post delivery callback registered")
	}
	if err := cb(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		time.Sleep(10 * time.Millisecond)
		if adapter.deletedCount() > 0 {
			break
		}
	}
	if preExistingFired.Load() != 1 {
		t.Fatalf("pre existing callback fired %d times", preExistingFired.Load())
	}
	if adapter.deletedCount() < 1 {
		t.Fatalf("deleted = %#v sent = %#v", adapter.deleted, adapter.sent)
	}
}
