package plugins

import (
	"encoding/json"
	"fmt"
	"testing"
)

const (
	evictionModuleCount = 5000
	evictionRounds      = 300
)

const evictionProbePlugin = `
import json, sys, threading, types

_CHURN_BATCH = 50
_STATE = {"stop": threading.Event(), "rounds": 0, "interval": None, "thread": None, "handed_off": threading.Event()}
_SCHEMA = lambda name: {"name": name, "description": "d", "parameters": {"type": "object", "properties": {}}}

def _plant(args, **kw):
    for name in args["names"]:
        sys.modules[name] = types.ModuleType(name)
    return json.dumps(True)

def _names(args, **kw):
    prefixes = tuple(args["prefixes"])
    return json.dumps(sorted(str(n) for n in list(sys.modules) if str.startswith(n, prefixes)))

def _churn():
    while not _STATE["stop"].is_set():
        batch = [f"late_import_{_STATE['rounds']}_{j}" for j in range(_CHURN_BATCH)]
        for key in batch:
            sys.modules[key] = None
        for key in batch:
            sys.modules.pop(key, None)
        _STATE["rounds"] += 1

def _churn_start(args, **kw):
    for i in range(int(args["unrelated"])):
        sys.modules[f"unrelated_{i}"] = types.ModuleType(f"unrelated_{i}")
    _STATE["interval"] = sys.getswitchinterval()
    sys.setswitchinterval(1e-6)
    _STATE["thread"] = threading.Thread(target=_churn, daemon=True)
    _STATE["thread"].start()
    return json.dumps(True)

def _churn_stop(args, **kw):
    _STATE["stop"].set()
    _STATE["thread"].join(timeout=10)
    sys.setswitchinterval(_STATE["interval"])
    return json.dumps(_STATE["rounds"])

def _insert_elsewhere():
    sys.modules["race_probe_sdk.client"] = types.ModuleType("race_probe_sdk.client")

class _SwitchPoint(str):
    def startswith(self, *args):
        if not _STATE["handed_off"].is_set():
            _STATE["handed_off"].set()
            worker = threading.Thread(target=_insert_elsewhere)
            worker.start()
            worker.join()
        return str.startswith(self, *args)

def _race_setup(args, **kw):
    for name in ("icebath_plugins.discord_platform", "icebath_plugins.discord_platform.adapter"):
        sys.modules[name] = types.ModuleType(name)
    sys.modules[_SwitchPoint("unrelated")] = types.ModuleType("unrelated")
    sys.modules["icebath_plugins.discord_platform.tools"] = types.ModuleType("icebath_plugins.discord_platform.tools")
    return json.dumps(True)

def _race_handed_off(args, **kw):
    return json.dumps(_STATE["handed_off"].is_set())

def register(ctx):
    for name, handler in (("evict_plant", _plant), ("evict_names", _names), ("evict_churn_start", _churn_start),
                          ("evict_churn_stop", _churn_stop), ("evict_race_setup", _race_setup),
                          ("evict_race_handed_off", _race_handed_off)):
        ctx.register_tool(name=name, toolset="evictprobe", schema=_SCHEMA(name), handler=handler)
`

// Live host of every interpreter running the probe plugin whose tools plant and list interpreter modules
func evictionHosts(t *testing.T, run func(t *testing.T, h string, m *Manager)) {
	for label, python := range liveInterpreters(t) {
		t.Run(label, func(t *testing.T) {
			installLiveServices(t)
			h, m := liveHome(t, python, map[string]string{"evictprobe": evictionProbePlugin})
			if e := loadedError(m, "evictprobe"); e != "" {
				t.Fatalf("evictprobe failed to load: %s", e)
			}
			run(t, h, m)
		})
	}
}

// Module names of the host interpreter starting with any of the prefixes, sorted
func hostModuleNames(t *testing.T, h string, prefixes ...string) []string {
	t.Helper()
	var names []string
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "evict_names", map[string]any{"prefixes": prefixes})), &names); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestEvictModulesDropsModuleAndSubmodules(t *testing.T) {
	evictionHosts(t, func(t *testing.T, h string, m *Manager) {
		dispatchLive(t, h, "evict_plant", map[string]any{"names": []string{"icebath_plugins.aop", "icebath_plugins.aop.client", "icebath_plugins.aop_other"}})
		if err := evictModules(m, "icebath_plugins.aop"); err != nil {
			t.Fatal(err)
		}
		if got := hostModuleNames(t, h, "icebath_plugins.aop"); len(got) != 1 || got[0] != "icebath_plugins.aop_other" {
			t.Fatalf("modules left after eviction: %v", got)
		}
	})
}

func TestEvictModulesToleratesConcurrentImports(t *testing.T) {
	evictionHosts(t, func(t *testing.T, h string, m *Manager) {
		dispatchLive(t, h, "evict_churn_start", map[string]any{"unrelated": evictionModuleCount})
		var failure error
		for i := 0; i < evictionRounds && failure == nil; i++ {
			name := fmt.Sprintf("icebath_plugins.victim_%d", i)
			dispatchLive(t, h, "evict_plant", map[string]any{"names": []string{name, name + ".sub"}})
			failure = evictModules(m, name)
		}
		var rounds int
		if err := json.Unmarshal([]byte(dispatchLive(t, h, "evict_churn_stop", nil)), &rounds); err != nil {
			t.Fatal(err)
		}
		if failure != nil {
			t.Fatalf("eviction raised under concurrent import: %v", failure)
		}
		if rounds <= 0 {
			t.Fatal("churn thread never ran, the race window was never opened")
		}
		if leftover := hostModuleNames(t, h, "icebath_plugins.victim_"); len(leftover) != 0 {
			t.Fatalf("eviction left entries behind: %v", leftover)
		}
	})
}
