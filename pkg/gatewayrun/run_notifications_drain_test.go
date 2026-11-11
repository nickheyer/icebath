package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Watch match event of a process on the Telegram DM topic 42
func watchEvent(sessionID string) map[string]any {
	return map[string]any{"type": "watch_match", "session_id": sessionID, "session_key": "agent:main:telegram:dm:123:42",
		"pattern": "READY", "command": "build", "output": "READY\n"}
}

func TestPostTurnWatchDrainOffConsumesWithoutInjecting(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	if err := os.WriteFile(filepath.Join(homeDir, "config.yaml"), []byte("display:\n  background_process_notifications: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, adapter := makeRestartRunner(t)
	r.sessionStore = gateway.NewSessionStore(context.Background(), filepath.Join(homeDir, "sessions"), r.config, nil)
	queue := tools.NewEventQueue()
	queue.Put(watchEvent("proc_one"))
	queue.Put(watchEvent("proc_two"))
	asyncEvent := map[string]any{"type": "async_delegation", "session_id": "delegate_one"}
	queue.Put(asyncEvent)

	if err := r.drainWatchNotifications(context.Background(), queue); err != nil {
		t.Fatal(err)
	}
	if calls := adapter.calls(); len(calls) != 0 {
		t.Fatalf("injected %v", calls)
	}
	if queue.Len() != 1 {
		t.Fatalf("queue size %d", queue.Len())
	}
	left, _ := queue.GetNowait()
	if fmt.Sprintf("%p", left) != fmt.Sprintf("%p", asyncEvent) {
		t.Fatalf("left %v", left)
	}
}
