package plugins

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const postPlugin = `
import asyncio, json

def _probe(args, **kw):
    h = ctx_ref[0].post_message("first")
    out = {"repr": repr(h), "type": type(h).__name__, "ok": h.ok, "id": h.message_id, "error": h.error, "pending": h.pending}
    out["edit"] = h.edit("second")
    out["after"] = h.result
    async def later():
        h2 = await ctx_ref[0].apost_message("third")
        return {"repr": repr(h2), "aedit": await h2.aedit("fourth")}
    out["async"] = asyncio.run(later())
    return json.dumps(out)

ctx_ref = []

def register(ctx):
    ctx_ref.append(ctx)
    ctx.register_tool(name="postprobe", toolset="postprobe", schema={"name": "postprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=_probe)
`

func TestLiveInProcessPluginPostsAndEditsMessagesThroughItsHandle(t *testing.T) {
	installLiveServices(t)
	var mu sync.Mutex
	var ops []string
	withGateway := *installedServices()
	withGateway.BoundSessionKey = func(context.Context) string { return "sess-1" }
	withGateway.GatewayLoop = func(context.Context) (any, bool) { return "runner", false }
	withGateway.ResolvePluginMessageTarget = func(_ context.Context, _ any, key string, _ any, _, _ string) (any, map[string]any, error) {
		return "target:" + key, nil, nil
	}
	withGateway.PostPluginMessage = func(_ context.Context, target any, text, _ string) map[string]any {
		mu.Lock()
		ops = append(ops, "post "+target.(string)+" "+text)
		mu.Unlock()
		return map[string]any{"ok": true, "message_id": "m-" + text}
	}
	withGateway.EditPluginMessage = func(_ context.Context, target, messageID any, text, _ string) map[string]any {
		mu.Lock()
		ops = append(ops, "edit "+messageID.(string)+" "+text)
		mu.Unlock()
		return map[string]any{"ok": true, "message_id": messageID, "edited": true}
	}
	InstallServices(&withGateway)
	h, m := liveHomeIsolated(t, liveInterpreters(t)["system"], map[string]string{"postprobe": postPlugin}, IsolationInProcess)
	if e := loadedError(m, "postprobe"); e != "" {
		t.Fatalf("postprobe failed to load: %s", e)
	}
	var got map[string]any
	out := dispatchLive(t, h, "postprobe", nil)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got["repr"] != "<PluginMessage ok=True message_id='m-first' error=None>" || got["type"] != "PluginMessage" || got["ok"] != true ||
		got["id"] != "m-first" || got["error"] != nil || got["pending"] != false {
		t.Fatalf("handle = %v", got)
	}
	if edit := got["edit"].(map[string]any); edit["ok"] != true || edit["edited"] != true || edit["message_id"] != "m-first" {
		t.Fatalf("edit = %v", edit)
	}
	async := got["async"].(map[string]any)
	if async["repr"] != "<PluginMessage ok=True message_id='m-third' error=None>" || async["aedit"].(map[string]any)["edited"] != true {
		t.Fatalf("async = %v", async)
	}
	want := []string{"post target:sess-1 first", "edit m-first second", "post target:sess-1 third", "edit m-third fourth"}
	mu.Lock()
	defer mu.Unlock()
	if len(ops) != len(want) {
		t.Fatalf("gateway ops = %v", ops)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("gateway ops = %v", ops)
		}
	}
}

const spawnPlugin = `
import json

CTX = []

async def _later():
    return CTX[0].post_message("from task").ok

def _probe(args, **kw):
    return json.dumps({"ok": CTX[0].spawn_task(_later()).result(10)})

def register(ctx):
    CTX.append(ctx)
    ctx.register_tool(name="spawnprobe", toolset="spawnprobe", schema={"name": "spawnprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=_probe)
`

type spawnSessionKey struct{}

// Port of the spawn_task contract of plugin_host_child, a task's ctx calls carrying the request that spawned it
func TestLiveSpawnedTaskCallsBackForTheRequestThatSpawnedIt(t *testing.T) {
	installLiveServices(t)
	var mu sync.Mutex
	var posted []string
	withGateway := *installedServices()
	withGateway.BoundSessionKey = func(ctx context.Context) string {
		key, _ := ctx.Value(spawnSessionKey{}).(string)
		return key
	}
	withGateway.GatewayLoop = func(context.Context) (any, bool) { return "runner", false }
	withGateway.ResolvePluginMessageTarget = func(_ context.Context, _ any, key string, _ any, _, _ string) (any, map[string]any, error) {
		return "target:" + key, nil, nil
	}
	withGateway.PostPluginMessage = func(_ context.Context, target any, text, _ string) map[string]any {
		mu.Lock()
		posted = append(posted, target.(string)+" "+text)
		mu.Unlock()
		return map[string]any{"ok": true, "message_id": "m1"}
	}
	InstallServices(&withGateway)
	h, m := liveHomeIsolated(t, liveInterpreters(t)["system"], map[string]string{"spawnprobe": spawnPlugin}, IsolationInProcess)
	if e := loadedError(m, "spawnprobe"); e != "" {
		t.Fatalf("spawnprobe failed to load: %s", e)
	}
	def := tools.Default.Lookup(homeCtx(h), h, "spawnprobe")
	out := def.Handler(context.WithValue(homeCtx(h), spawnSessionKey{}, "sess-7"), &tools.Call{Name: "spawnprobe", Args: map[string]any{}})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["ok"] != true {
		t.Fatalf("spawned post = %s %v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0] != "target:sess-7 from task" {
		t.Fatalf("posted = %v", posted)
	}
}
