package main

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A plugin post_message resolves the session key the calling turn bound on its Env, and refuses outside a turn
func TestPluginPostMessageReadsTheBoundTurnSessionKey(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	previous := plugins.InstalledServices()
	t.Cleanup(func() { plugins.InstallServices(previous) })
	s := pluginServices()
	var resolved []string
	var mu sync.Mutex
	s.GatewayLoop = func(context.Context) (any, bool) { return "runner", false }
	s.ResolvePluginMessageTarget = func(_ context.Context, _ any, key string, _ any, _, _ string) (any, map[string]any, error) {
		mu.Lock()
		resolved = append(resolved, key)
		mu.Unlock()
		return nil, map[string]any{"ok": false, "error": "no_conversation", "detail": "unknown session"}, nil
	}
	plugins.InstallServices(s)
	ctx := context.Background()
	c := plugins.NewContext(&plugins.Manifest{Name: "progress"}, plugins.GetPluginManager(ctx))
	if got := c.PostMessage(ctx, "hi").Result(); got["error"] != "no_conversation" || len(resolved) != 0 {
		t.Fatalf("unbound post %v resolved %v", got, resolved)
	}
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	restore := tools.ScopedSessionKey(env, "agent:main:telegram:dm:42")
	defer restore()
	c.PostMessage(tools.WithToolEnv(ctx, env), "hi")
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(resolved, []string{"agent:main:telegram:dm:42"}) {
		t.Fatalf("bound post resolved %v", resolved)
	}
}
