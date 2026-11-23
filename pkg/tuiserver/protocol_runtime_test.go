package tuiserver

import (
	"context"
	"maps"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Already set agent ready event of a built session
func readyEvent() *pythread.Event {
	ready := &pythread.Event{}
	ready.Set()
	return ready
}

// Transport whose socket already closed
type closedPeer struct{}

func (*closedPeer) Write(*pyjson.Object) bool { return false }
func (*closedPeer) Close()                    {}
func (*closedPeer) Closed() bool              { return true }

func TestSyncSessionKeyAfterCompressReanchorsActiveSessionLease(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rotated := testAgent(t, agent.AIAgentOptions{})
	rotated.SetSessionID("session-new")
	lease, refusal, err := activesessions.TryAcquire(ctx, "session-old", "tui", map[string]any{"max_concurrent_sessions": 1},
		map[string]any{"live_session_id": "ui-1"}, "", false)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("lease %v %v %v", lease, refusal, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	session := tuihost.NewSession(map[string]any{"active_session_lease": lease,
		"agent": rotated, "session_key": "session-old"})
	syncSessionKeyAfterCompress(ctx, "ui-1", session, true)
	if session.Get("session_key") != "session-new" || lease.SessionID() != "session-new" {
		t.Fatalf("session key %v lease %v", session.Get("session_key"), lease.SessionID())
	}
	entries := registryEntries(t, "")
	if len(entries) != 1 || entries[0]["session_id"] != "session-new" {
		t.Fatalf("registry %v", entries)
	}
}

func TestMakeAgentAcceptsListSystemPrompt(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeFile(t, filepath.Join(h, "config.yaml"), "model:\n  default: test/model\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n  api_key: test\n"+
		"agent:\n  system_prompt:\n    - one\n    - two\n")
	useSharedDB(t, ledgerHandle(t, filepath.Join(h, "state.db")))
	built, err := makeAgent(context.Background(), "sid", "session-key", makeAgentOptions{sessionID: "session-key"})
	if err != nil {
		t.Fatal(err)
	}
	a, isAgent := built.(*agent.AIAgent)
	if !isAgent {
		t.Fatalf("built %T %v", built, built)
	}
	if a.EphemeralSystemPrompt() != "one\ntwo" {
		t.Fatalf("prompt %q", a.EphemeralSystemPrompt())
	}
}

func TestConfigRoundtrip(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if err := saveCfg(ctx, map[string]any{"model": "test/model"}); err != nil {
		t.Fatal(err)
	}
	if got := loadCfg(ctx)["model"]; got != "test/model" {
		t.Fatalf("model %v", got)
	}
}

// session.create through the real dispatcher under a fresh home with synthetic agent builds
func createCall(t *testing.T, rid string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"id": rid, "method": "session.create", "params": maps.Clone(params)})
}

// Created session id of a successful create
func createdSID(t *testing.T, resp map[string]any) string {
	t.Helper()
	if _, failed := resp["error"]; failed {
		t.Fatalf("create failed %v", resp)
	}
	sid, _ := resultOf(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session id in %v", resp)
	}
	return sid
}
