package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const asyncPlugin = `
import asyncio, json
S = {"type": "object", "properties": {}}

def register(ctx):
    async def whoami(args, **kw):  # async plugin code calling back into Icebath
        return json.dumps({"inner": json.loads(ctx.dispatch_tool("session_probe", {})),
                           "acomplete_awaitable": asyncio.iscoroutinefunction(ctx.llm.acomplete)})
    ctx.register_tool(name="async_whoami", toolset="asyncprobe", schema={"name": "async_whoami",
        "description": "d", "parameters": S}, handler=whoami, is_async=True)
`

type liveSessionKey struct{}

// Under host isolation user plugin code is refused in process and the audit reads the host boundary table
func TestIsolationHostKeepsEveryUserImportPathOutOfProcess(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	for name, body := range map[string]string{"platformprobe": platformPlugin, "fine": probePlugin} {
		dir := filepath.Join(h, "plugins", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\n")
		writeFile(t, filepath.Join(dir, "__init__.py"), body)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [platformprobe, fine]\n  isolation: host\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	refusal := InProcessImportRefusal(homeCtx(h), "plugin 'fine' (loaded as _icebath_user_x.fine)", "user", nil)
	if !strings.Contains(refusal, "plugin 'fine' (loaded as _icebath_user_x.fine) runs only in-process") {
		t.Fatalf("user code was not refused in process: %q", refusal)
	}
	fine, err := auditPluginDir(filepath.Join(h, "plugins", "fine"), nil)
	if err != nil || fine.Verdict != VerdictHost {
		t.Fatalf("fine audit %+v %v", fine, err)
	}
	blocked, err := auditPluginDir(filepath.Join(h, "plugins", "platformprobe"), nil)
	if err != nil || blocked.Verdict != VerdictInProcess || len(blocked.Reasons) == 0 || !strings.Contains(blocked.Reasons[0], "register_platform") {
		t.Fatalf("platformprobe audit %+v %v", blocked, err)
	}
}

// A hosted async tool calling back into the parent runs in the session of whoever dispatched it
func TestAsyncPluginCodeCallsBackInTheCallersSession(t *testing.T) {
	for label, python := range liveInterpreters(t) {
		t.Run(label, func(t *testing.T) {
			installLiveServices(t)
			tools.Default.Register(tools.Def{Name: "session_probe", Toolset: "asyncprobe", Schema: ledgerSchema("session_probe"),
				Handler: func(ctx context.Context, _ *tools.Call) string {
					session, ok := ctx.Value(liveSessionKey{}).(string)
					if !ok {
						session = "<unset>"
					}
					out, _ := json.Marshal(map[string]any{"session": session})
					return string(out)
				}})
			t.Cleanup(func() { _ = tools.Default.Deregister("session_probe", "", "") })
			creator := func(ctx context.Context) context.Context {
				return context.WithValue(ctx, liveSessionKey{}, "host-creator")
			}
			h, m := liveHomeLoadedIn(t, python, map[string]string{"asyncprobe": asyncPlugin}, IsolationHost, creator)
			if e := loadedError(m, "asyncprobe"); e != "" {
				t.Fatalf("asyncprobe failed to load: %s", e)
			}
			callAs := func(name string) map[string]any {
				def := tools.Default.Lookup(homeCtx(h), h, "async_whoami")
				if def == nil {
					t.Fatal("async_whoami is not registered")
				}
				raw := def.Handler(context.WithValue(homeCtx(h), liveSessionKey{}, name), &tools.Call{Name: "async_whoami", Args: map[string]any{}})
				var got map[string]any
				if err := json.Unmarshal([]byte(raw), &got); err != nil {
					t.Fatalf("async_whoami = %s", raw)
				}
				return got
			}
			b := callAs("session-B")
			if inner, _ := b["inner"].(map[string]any); len(b) != 2 || len(inner) != 1 || inner["session"] != "session-B" || b["acomplete_awaitable"] != true {
				t.Fatalf("session-B call = %v", b)
			}
			if inner, _ := callAs("session-C")["inner"].(map[string]any); len(inner) != 1 || inner["session"] != "session-C" {
				t.Fatalf("session-C inner = %v", inner)
			}
		})
	}
}
