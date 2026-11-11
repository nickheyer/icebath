package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Writes one live lease of this process for a session into the registry of a profile home
func attachLease(t *testing.T, home, sessionID, leaseID string, metadata map[string]any) {
	t.Helper()
	now := float64(time.Now().UnixNano()) / 1e9
	entry := map[string]any{"lease_id": leaseID, "session_id": sessionID, "surface": "desktop", "pid": os.Getpid(),
		"process_start_time": nil, "started_at": now, "updated_at": now}
	if metadata != nil {
		entry["metadata"] = metadata
	}
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

func TestDiscoveryUsesExactProfileAndOwnerHandshake(t *testing.T) {
	base := t.TempDir()
	home, other := filepath.Join(base, "profile"), filepath.Join(base, "other")
	var mu sync.Mutex
	reply := map[string]any{}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.URL.RequestURI())
		payload, _ := json.Marshal(reply)
		w.WriteHeader(200)
		w.Write(payload)
	}))
	defer server.Close()
	origin := server.URL
	attachLease(t, home, "same-id", "lease-1", map[string]any{"live_session_id": "live", "shared_runtime_url": origin})
	resolvedHome, err := pypath.Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOther, err := pypath.Resolve(other)
	if err != nil {
		t.Fatal(err)
	}
	wsURL := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws?token=real-token"
	mu.Lock()
	reply["session_id"], reply["lease_id"], reply["profile_home"], reply["websocket_url"] = "same-id", "lease-1", resolvedHome, wsURL
	mu.Unlock()
	if url, found, err := discoverAttachURL("same-id", other); err != nil || found || len(requests) != 0 {
		t.Fatalf("other profile discovered %q %v %v", url, found, err)
	}
	if url, found, err := discoverAttachURL("same-id", home); err != nil || !found || url != wsURL || len(requests) != 1 {
		t.Fatalf("discovery %q %v %v %d", url, found, err, len(requests))
	}
	env := map[string]string{"ICEBATH_TUI_GATEWAY_URL": "   "}
	if err := configureTUIAttachment(env, "same-id", home); err != nil || env["ICEBATH_TUI_GATEWAY_URL"] != wsURL {
		t.Fatalf("attachment %v %v", env, err)
	}
	mu.Lock()
	reply["profile_home"] = resolvedOther
	mu.Unlock()
	if _, _, err := discoverAttachURL("same-id", home); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("identity mismatch accepted: %v", err)
	}
	mu.Lock()
	reply["profile_home"], reply["websocket_url"] = resolvedHome, "ws://example.com/api/ws?token=secret"
	mu.Unlock()
	if _, _, err := discoverAttachURL("same-id", home); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("foreign endpoint accepted: %v", err)
	}
}

func TestDiscoveryFailureMessageNamesStateAndResumePath(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	attachLease(t, home, "held", "lease-held", map[string]any{"live_session_id": "live", "shared_runtime_url": server.URL})
	_, _, err := discoverAttachURL("held", home)
	if err == nil || !strings.Contains(err.Error(), "just failed") {
		t.Fatalf("error %v", err)
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "icebath --resume held") || !strings.HasPrefix(lines[1], "Details: ") {
		t.Fatalf("message %q", err.Error())
	}
}
