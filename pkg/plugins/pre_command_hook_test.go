package plugins

import (
	"context"
	"strings"
	"testing"
)

func TestFireHelperIsObserverOnlyAndNeverRaises(t *testing.T) {
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	ctx := homeCtx(t.TempDir())
	m := GetPluginManager(ctx)
	m.discovered = true
	calls := map[string]any{}
	m.TrackHook(newContext(&Manifest{Name: "observer", Key: "observer"}, m), "pre_command", func(_ context.Context, kwargs map[string]any) (any, error) {
		calls["name"], calls["kwargs"] = "pre_command", kwargs
		return map[string]any{"action": "block", "message": "nope"}, nil
	}, false)
	logs := captureLogs(t)
	FirePreCommandHook(ctx, "cli", "model", "model", "gpt-x", nil, nil)
	kwargs, _ := calls["kwargs"].(map[string]any)
	if calls["name"] != "pre_command" || kwargs["surface"] != "cli" || kwargs["command"] != "model" {
		t.Fatalf("hook calls %v", calls)
	}
	if !strings.Contains(logs.String(), "pre_command is observer-only in v1: ignoring directive") {
		t.Fatalf("the directive shaped return was not reported as ignored: %s", logs.String())
	}
}

// Context whose home lookup panics, so resolving the plugin manager of the call fails
type explodingLookupContext struct{ context.Context }

func (explodingLookupContext) Value(any) any { panic("plugin discovery exploded") }

func TestFireHelperSwallowsManagerErrors(t *testing.T) {
	logs := captureLogs(t)
	FirePreCommandHook(explodingLookupContext{context.Background()}, "gateway", "new", "reset", "", nil, nil)
	if !strings.Contains(logs.String(), "pre_command hook dispatch failed (non-fatal): plugin discovery exploded") {
		t.Fatalf("the manager lookup failure was not logged: %s", logs.String())
	}
}
