package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Writes one live lease of this process for a session into the registry of a profile home
func attachLease(t *testing.T, home, sessionID, leaseID string) {
	t.Helper()
	now := float64(time.Now().UnixNano()) / 1e9
	entry := map[string]any{"lease_id": leaseID, "session_id": sessionID, "surface": "tui", "pid": os.Getpid(),
		"process_start_time": nil, "started_at": now, "updated_at": now, "metadata": map[string]any{"live_session_id": "live"}}
	data, err := json.Marshal(map[string]any{"entries": []any{entry}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active_sessions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedResumeIsRefusedWithStateAndResumePath(t *testing.T) {
	home := t.TempDir()
	attachLease(t, home, "held", "lease-held")
	err := refuseOwnedTUIResume(map[string]string{}, "held", home)
	if err == nil {
		t.Fatal("owned resume was not refused")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "icebath --resume held") || !strings.HasPrefix(lines[1], "Details: ") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestOwnedResumeChecksOnlyTheGivenProfileAndYieldsToAnExplicitGateway(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	attachLease(t, home, "same-id", "lease-1")
	if err := refuseOwnedTUIResume(map[string]string{}, "same-id", other); err != nil {
		t.Fatalf("other profile refused %v", err)
	}
	if err := refuseOwnedTUIResume(map[string]string{"ICEBATH_TUI_GATEWAY_URL": "ws://127.0.0.1:1/api/ws"}, "same-id", home); err != nil {
		t.Fatalf("explicit gateway refused %v", err)
	}
}
