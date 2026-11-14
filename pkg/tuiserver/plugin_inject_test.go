package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Home whose config lets notify-plugin inject, with a fresh manager and the plugin's context on it
func injectHostCase(t *testing.T) (*plugins.Context, *plugins.Manager, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "plugins:\n  entries:\n    notify-plugin:\n      allow_gateway_injection: true\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	manager := plugins.NewManager(h)
	manifest := &plugins.Manifest{Name: "notify-plugin", Key: "notify-plugin", Source: "user"}
	return plugins.NewContext(manifest, manager), manager, h
}

// Live session record shaped like the hermes case fixture, registered under the UI sid until cleanup
func injectHostSession(t *testing.T, sid, sessionKey string, extra map[string]any) *tuigateway.Session {
	t.Helper()
	fields := map[string]any{"agent": struct{}{}, "session_key": sessionKey, "history": []any{}, "history_lock": &sync.Mutex{},
		"history_version": 0, "running": false, "transport": nil, "attached_images": []any{}, "last_active": 1.0}
	for k, v := range extra {
		fields[k] = v
	}
	session := tuigateway.NewSession(fields)
	tuigateway.SetSessionRecord(sid, session)
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return session
}

// Gateway slot recording every call and answering true
type recordingInjector struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *recordingInjector) inject(_ context.Context, kwargs map[string]any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, kwargs)
	return true
}

// CLI host whose pending input and interrupt queues are recorded
type recordingCLI struct{ pending, interrupts []string }

func (c *recordingCLI) AgentRunning() bool        { return false }
func (c *recordingCLI) QueueInterrupt(msg string) { c.interrupts = append(c.interrupts, msg) }
func (c *recordingCLI) QueuePending(msg string)   { c.pending = append(c.pending, msg) }
func (c *recordingCLI) Agent() any                { return nil }
func queuedText(session *tuigateway.Session) any {
	q, _ := session.Get("queued_prompt").(map[string]any)
	return q["text"]
}
func injectWithHost(t *testing.T, manager *plugins.Manager) {
	t.Helper()
	InstallTUIMessageInjector(context.Background(), manager)
	t.Cleanup(func() { ClearTUIMessageInjector(context.Background(), manager) })
}

// test_plugin_inject_host.py L78 test_reported_session_key_is_queued_not_the_ui_session_id
func TestReportedSessionKeyIsQueuedNotTheUISessionID(t *testing.T) {
	pctx, manager, _ := injectHostCase(t)
	gw := &recordingInjector{}
	manager.SetGatewayMessageInjector(&struct{ int }{}, gw.inject)
	origin := injectHostSession(t, "ui-origin", "ses_origin", map[string]any{"running": true, "last_active": 10.0})
	chatty := injectHostSession(t, "ui-chatty", "ses_chatty", map[string]any{"running": true, "last_active": 200.0})
	injectWithHost(t, manager)
	if !pctx.InjectMessage(context.Background(), "turn complete", "user", "ses_origin", nil) {
		t.Fatal("inject_message returned false")
	}
	if got := queuedText(origin); got != "turn complete" {
		t.Fatalf("origin queued %v", got)
	}
	if chatty.Get("queued_prompt") != nil {
		t.Fatalf("chatty queued %v", chatty.Get("queued_prompt"))
	}
	if len(gw.calls) != 0 {
		t.Fatalf("gateway called %v", gw.calls)
	}
}

// test_plugin_inject_host.py L100 test_unknown_session_key_falls_through_to_the_gateway_slot
func TestUnknownSessionKeyFallsThroughToTheGatewaySlot(t *testing.T) {
	pctx, manager, h := injectHostCase(t)
	gw := &recordingInjector{}
	manager.SetGatewayMessageInjector(&struct{ int }{}, gw.inject)
	live := injectHostSession(t, "ui-live", "ses_live", map[string]any{"running": true})
	injectWithHost(t, manager)
	if !pctx.InjectMessage(context.Background(), "wake telegram", "user", "agent:main:telegram:dm:42", nil) {
		t.Fatal("inject_message returned false")
	}
	want := []map[string]any{{"session_key": "agent:main:telegram:dm:42", "plugin_home": h, "content": "wake telegram",
		"plugin_id": "notify-plugin"}}
	if !reflect.DeepEqual(gw.calls, want) {
		t.Fatalf("gateway calls %v", gw.calls)
	}
	if live.Get("queued_prompt") != nil {
		t.Fatalf("live queued %v", live.Get("queued_prompt"))
	}
}

// test_plugin_inject_host.py L124 test_missing_session_is_not_rerouted_to_another_tui_session
func TestMissingSessionIsNotReroutedToAnotherTUISession(t *testing.T) {
	pctx, manager, _ := injectHostCase(t)
	other := injectHostSession(t, "ui-other", "ses_other", map[string]any{"running": true, "last_active": 200.0})
	injectWithHost(t, manager)
	if pctx.InjectMessage(context.Background(), "report", "user", "ses_gone", nil) {
		t.Fatal("inject_message returned true")
	}
	if other.Get("queued_prompt") != nil {
		t.Fatalf("other queued %v", other.Get("queued_prompt"))
	}
}

// test_plugin_inject_host.py L161 test_cli_injection_still_bypasses_the_tui_host
func TestCLIInjectionStillBypassesTheTUIHost(t *testing.T) {
	pctx, manager, _ := injectHostCase(t)
	cli := &recordingCLI{}
	manager.SetCLIRef(cli)
	origin := injectHostSession(t, "ui-origin", "ses_origin", map[string]any{"running": true})
	injectWithHost(t, manager)
	if !pctx.InjectMessage(context.Background(), "typed", "user", "ses_origin", nil) {
		t.Fatal("inject_message returned false")
	}
	if !reflect.DeepEqual(cli.pending, []string{"typed"}) {
		t.Fatalf("pending %v", cli.pending)
	}
	if origin.Get("queued_prompt") != nil {
		t.Fatalf("origin queued %v", origin.Get("queued_prompt"))
	}
}
