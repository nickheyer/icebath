package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

const hostStatePlugin = `
import json, os
def register(ctx):
    def run(args, **kw):
        st = ctx.state
        st.set("count", {"n": 3})
        out = {"pid": os.getpid(), "got": st.get("count"), "missing": st.get("nope", default="dflt"),
               "dir": st.data_dir, "path": st.path, "quota": st.quota_bytes}
        for name, probe in (("bad_key", lambda: st.set("a..b", 1)), ("attr", lambda: st.nope)):
            try:
                probe()
            except Exception as exc:
                out[name] = type(exc).__name__ + ": " + str(exc)
        return json.dumps(out)
    ctx.register_tool(name="hoststate", toolset="hoststate",
        schema={"name": "hoststate", "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=run)
`

// ctx.state of a hosted plugin reads and writes the parent store through the state facade
func TestHostedPluginStateFacade(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{"hoststate": hostStatePlugin})
	var out map[string]any
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "hoststate", map[string]any{})), &out); err != nil {
		t.Fatal(err)
	}
	dir, _ := out["dir"].(string)
	if out["pid"] == float64(os.Getpid()) || filepath.Dir(dir) != home.PluginDataDir(h) || out["path"] != filepath.Join(dir, "state.json") || out["quota"] != float64(pluginStateQuotaBytes) {
		t.Fatalf("state facade answered %v", out)
	}
	if got, _ := out["got"].(map[string]any); got["n"] != float64(3) || out["missing"] != "dflt" {
		t.Fatalf("get answered %v / %v", out["got"], out["missing"])
	}
	if out["bad_key"] != "ValueError: Plugin state keys must be 1-128 characters using letters, numbers, '_', '-', '.', or ':' (without '..')" {
		t.Fatalf("bad key = %v", out["bad_key"])
	}
	if out["attr"] != "PluginHostError: AttributeError: 'PluginState' object has no attribute 'nope'" {
		t.Fatalf("unknown attribute = %v", out["attr"])
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	var stored map[string]any
	if err != nil || json.Unmarshal(data, &stored) != nil || stored["count"].(map[string]any)["n"] != float64(3) {
		t.Fatalf("state file %q %v", data, err)
	}
}
