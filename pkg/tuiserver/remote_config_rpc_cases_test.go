package tuiserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
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
