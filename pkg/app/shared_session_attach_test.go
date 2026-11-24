package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Live lease of a session in the registry of a profile home, released when the case ends
func attachLease(t *testing.T, home, sessionID string, metadata map[string]any) *activesessions.Lease {
	t.Helper()
	lease, refusal, err := activesessions.TryAcquire(context.Background(), sessionID, "tui", map[string]any{}, metadata, home, false)
	if err != nil || refusal != nil {
		t.Fatalf("acquire %v %v", refusal, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

// Resolved path of a profile home, the text the handshake carries
func resolvedHome(t *testing.T, p string) string {
	t.Helper()
	resolved, err := pathutil.Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDiscoveryUsesExactProfileAndOwnerHandshake(t *testing.T) {
	ctx := context.Background()
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
	lease := attachLease(t, home, "same-id", map[string]any{"live_session_id": "live", "shared_runtime_url": origin})
	wsURL := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws?token=real-token"
	mu.Lock()
	reply["session_id"], reply["lease_id"], reply["profile_home"], reply["websocket_url"] = "same-id", lease.LeaseID, resolvedHome(t, home), wsURL
	mu.Unlock()
	served := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
	if url, found, err := discoverAttachURL(ctx, "same-id", other); err != nil || found || len(served()) != 0 {
		t.Fatalf("other profile discovered %q %v %v", url, found, err)
	}
	if url, found, err := discoverAttachURL(ctx, "same-id", home); err != nil || !found || url != wsURL || len(served()) != 1 {
		t.Fatalf("discovery %q %v %v %d", url, found, err, len(served()))
	}
	if first := served()[0]; !strings.HasPrefix(first, "/api/session-attach?session_id=same-id&lease_id="+lease.LeaseID+"&profile_home=") {
		t.Fatalf("handshake request %q", first)
	}
	env := map[string]string{"ICEBATH_TUI_GATEWAY_URL": "   "}
	if err := configureTUIAttachment(ctx, env, "same-id", home); err != nil || env["ICEBATH_TUI_GATEWAY_URL"] != wsURL {
		t.Fatalf("attachment %v %v", env, err)
	}
	mu.Lock()
	reply["profile_home"] = resolvedHome(t, other)
	mu.Unlock()
	if _, _, err := discoverAttachURL(ctx, "same-id", home); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("identity mismatch accepted: %v", err)
	}
	mu.Lock()
	reply["profile_home"], reply["websocket_url"] = resolvedHome(t, home), "ws://example.com/api/ws?token=secret"
	mu.Unlock()
	if _, _, err := discoverAttachURL(ctx, "same-id", home); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("foreign endpoint accepted: %v", err)
	}
}

func TestDiscoveryRefusesUnsupportedOwnerWithoutReleasingLease(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	lease := attachLease(t, home, "old", nil)
	_, _, err := discoverAttachURL(ctx, "old", home)
	if err == nil || !strings.Contains(err.Error(), "not available in this build") {
		t.Fatalf("error %v", err)
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "icebath --resume old") || !strings.HasPrefix(lines[1], "Details: ") {
		t.Fatalf("message %q", err.Error())
	}
	entries, err := activesessions.RegistrySnapshot(ctx, home, false)
	if err != nil || len(entries) != 1 || entries[0].Value("lease_id") != lease.LeaseID {
		t.Fatalf("lease not kept %v %v", entries, err)
	}
	registry := filepath.Join(home, "runtime", "active_sessions.json")
	raw, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["entries"].([]any)[0].(map[string]any)["surface"] = 7
	broken, _ := json.Marshal(doc)
	if err := os.WriteFile(registry, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discoverAttachURL(ctx, "old", home); err == nil || !strings.Contains(err.Error(), "invalid surface") {
		t.Fatalf("unprovable owner accepted: %v", err)
	}
	if after, _ := os.ReadFile(registry); !bytes.Equal(after, broken) {
		t.Fatal("a refused discovery rewrote the registry")
	}
}

func TestDiscoveryFailureMessageNamesStateAndResumePath(t *testing.T) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	attachLease(t, home, "held", map[string]any{"live_session_id": "live", "shared_runtime_url": server.URL})
	_, _, err := discoverAttachURL(context.Background(), "held", home)
	if err == nil || !strings.Contains(err.Error(), "just failed") {
		t.Fatalf("error %v", err)
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "icebath --resume held") || !strings.HasPrefix(lines[1], "Details: ") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestAttachmentYieldsToAnExplicitGatewayAndRefusesNonLoopbackOwners(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	attachLease(t, home, "same-id", map[string]any{"live_session_id": "live", "shared_runtime_url": "http://example.com:8080"})
	env := map[string]string{"ICEBATH_TUI_GATEWAY_URL": "ws://127.0.0.1:1/api/ws"}
	if err := configureTUIAttachment(ctx, env, "same-id", home); err != nil || env["ICEBATH_TUI_GATEWAY_URL"] != "ws://127.0.0.1:1/api/ws" {
		t.Fatalf("explicit gateway overridden %v %v", env, err)
	}
	if err := configureTUIAttachment(ctx, map[string]string{}, "", home); err != nil {
		t.Fatalf("fresh launch refused %v", err)
	}
	err := configureTUIAttachment(ctx, map[string]string{}, "same-id", home)
	if err == nil || err.Error() != "Shared runtime endpoint must be an explicit loopback address and port." {
		t.Fatalf("remote owner accepted %v", err)
	}
}
