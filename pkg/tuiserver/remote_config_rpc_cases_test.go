package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Remote plane fixture with the raw config cache of the server cleared
func tuiRemotePlane(t *testing.T) *configplane.Plane {
	t.Helper()
	p := remoteplane.Setup(t)
	clear := func() {
		rawCfgCache.Lock()
		rawCfgCache.cfg, rawCfgCache.sig, rawCfgCache.path = nil, nil, ""
		rawCfgCache.Unlock()
		config.ResetCache()
	}
	clear()
	t.Cleanup(clear)
	return p
}

func rawTuiTheme(t *testing.T) any {
	t.Helper()
	raw, err := loadCfgRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	display, _ := raw["display"].(map[string]any)
	return display["tui_theme"]
}

// A keyed edit under a lock answers with the lock, sends nothing and keeps the accepted value cached
func TestTuiConfigSetOfALockedKeyIsRefusedNotReportedSaved(t *testing.T) {
	p := tuiRemotePlane(t)
	p.Upper = map[string]any{"display": map[string]any{"tui_theme": "dark"}}
	p.UpperLocks = []map[string]any{{"path": "display.tui_theme", "level": "tenant"}}
	answer := callMethod(t, "config.set", map[string]any{"key": "theme", "value": "light"})
	rpcError, _ := answer["error"].(map[string]any)
	if rpcError == nil || rpcError["code"] != 4002 || !strings.Contains(rpcError["message"].(string), "locked") {
		t.Fatalf("answer %v", answer)
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches", n)
	}
	if got := rawTuiTheme(t); got != "dark" {
		t.Fatalf("cached theme %v", got)
	}
	p.Mu.Lock()
	p.UpperLocks = nil
	p.Mu.Unlock()
	remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
	second := callMethod(t, "config.set", map[string]any{"key": "theme", "value": "light"})
	if result, _ := second["result"].(map[string]any); result == nil || result["value"] != "light" {
		t.Fatalf("second answer %v", second)
	}
	if got := rawTuiTheme(t); got != "light" {
		t.Fatalf("cached theme after save %v", got)
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if !reflect.DeepEqual(p.Profile("default").Values, map[string]any{"display": map[string]any{"tui_theme": "light"}}) {
		t.Fatalf("plane values %v", p.Profile("default").Values)
	}
}

// A request refused by the config backend answers once with its own id, inline or pooled
func TestRpcRefusedByTheConfigBackendStillGetsItsOneAnswer(t *testing.T) {
	tuiRemotePlane(t)
	wsTestMethod(t, "cc.refused", func(context.Context, any, map[string]any) map[string]any {
		panic(&config.ConfigBackendUnavailable{Message: "Remote Config: cannot load the config for profile 'lazy'"})
	})
	inline := handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": "inline-1", "method": "cc.refused", "params": map[string]any{}})
	if rpcError, _ := inline["error"].(map[string]any); inline["id"] != "inline-1" || rpcError == nil || !strings.Contains(rpcError["message"].(string), "cannot load the config") {
		t.Fatalf("inline %v", inline)
	}
	addLongHandlers("cc.refused")
	t.Cleanup(func() {
		longHandlersMu.Lock()
		defer longHandlersMu.Unlock()
		next := make(map[string]bool, len(longHandlers))
		for name := range longHandlers {
			if name != "cc.refused" {
				next[name] = true
			}
		}
		longHandlers = next
	})
	transport := &frameRecorder{}
	if resp := Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": "pooled-1", "method": "cc.refused", "params": map[string]any{}}, transport); resp != nil {
		t.Fatalf("pooled request answered inline %v", resp)
	}
	deadline := time.Now().Add(10 * time.Second)
	for transport.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	transport.mu.Lock()
	frames := append([]*pyjson.Object(nil), transport.frames...)
	transport.mu.Unlock()
	if len(frames) != 1 {
		t.Fatalf("frames %v", frames)
	}
	frame := pyjson.Plain(frames[0]).(map[string]any)
	rpcError, _ := frame["error"].(map[string]any)
	if frame["id"] != "pooled-1" || rpcError == nil || !strings.Contains(rpcError["message"].(string), "cannot load the config") {
		t.Fatalf("frame %v", frame)
	}
}

// Prompt, reasoning and details mode setters under an upper lock answer with the lock and change nothing, unlocked they write
func TestTuiSectionAndPromptSettersRefuseLockedEdits(t *testing.T) {
	p := tuiRemotePlane(t)
	p.Upper = map[string]any{"custom_prompt": "tenant-prompt",
		"display": map[string]any{"show_reasoning": false, "sections": map[string]any{"thinking": "hidden"}, "details_mode": "collapsed"}}
	p.UpperLocks = []map[string]any{{"path": "custom_prompt", "level": "tenant"}, {"path": "display", "level": "group"}}
	session := liveRecord(t, "s1")
	session.Set("show_reasoning", false)
	session.Set("session_key", "k")
	calls := [][2]string{{"prompt", "synthetic-new-prompt"}, {"prompt", "clear"}, {"reasoning", "show"},
		{"details_mode", "expanded"}, {"details_mode.thinking", "expanded"}, {"details_mode.thinking", ""}}
	for _, c := range calls {
		answer := callMethod(t, "config.set", map[string]any{"key": c[0], "value": c[1], "session_id": "s1"})
		rpcError, _ := answer["error"].(map[string]any)
		if rpcError == nil || rpcError["code"] != 4002 || !strings.Contains(rpcError["message"].(string), "locked") {
			t.Fatalf("%v answered %v", c, answer)
		}
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches", n)
	}
	if session.Get("show_reasoning") != false {
		t.Fatalf("session show_reasoning %v", session.Get("show_reasoning"))
	}
	raw, err := loadCfgRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw["custom_prompt"] != "tenant-prompt" || raw["display"].(map[string]any)["show_reasoning"] != false {
		t.Fatalf("raw %v", raw)
	}
	p.Mu.Lock()
	p.UpperLocks = nil
	p.Mu.Unlock()
	remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
	for _, c := range append(calls[:1:1], calls[2:5]...) {
		if answer := callMethod(t, "config.set", map[string]any{"key": c[0], "value": c[1], "session_id": "s1"}); answer["result"] == nil {
			t.Fatalf("%v answered %v", c, answer)
		}
	}
	p.Mu.Lock()
	stored := config.Clone(p.Profile("default").Values).(map[string]any)
	p.Mu.Unlock()
	display, _ := stored["display"].(map[string]any)
	sections, _ := display["sections"].(map[string]any)
	if stored["custom_prompt"] != "synthetic-new-prompt" || display["show_reasoning"] != true || display["details_mode"] != "expanded" || sections["thinking"] != "expanded" {
		t.Fatalf("stored %v", stored)
	}
	if session.Get("show_reasoning") != true {
		t.Fatalf("session show_reasoning %v", session.Get("show_reasoning"))
	}
	for _, patch := range p.Patches() {
		if _, has := patch.Body["unset"]; has {
			t.Fatalf("patch unsets %v", patch.Body)
		}
	}
}

// The consent setter refuses a locked answer and records no consent change
func TestTuiSharedMetricsSetRefusesALockedConsent(t *testing.T) {
	p := tuiRemotePlane(t)
	p.Upper = map[string]any{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": false, "send": false}}}
	p.UpperLocks = []map[string]any{{"path": "telemetry.shared_metrics", "level": "tenant"}}
	answer := callMethod(t, "shared_metrics.set", map[string]any{"enabled": true, "send": true})
	rpcError, _ := answer["error"].(map[string]any)
	if rpcError == nil || rpcError["code"] != 4002 || !strings.Contains(rpcError["message"].(string), "locked") {
		t.Fatalf("answer %v", answer)
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches", n)
	}
	if _, err := os.Stat(filepath.Join(p.Home, "telemetry", "shared_metrics", "metrics.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("consent change recorded %v", err)
	}
}
