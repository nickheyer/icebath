package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
