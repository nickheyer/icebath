package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const jsonProviderSource = `import contextlib
import json
import threading
from pathlib import Path

EVENTS = []
_lock = threading.RLock()
_plain = threading.Lock()


def block(cfg, host):
    cfg.setdefault("seen", []).append(host)
    return (cfg.get("hosts") or {}).get(host) or {}


def swap(cfg, keep):
    old = cfg["hosts"]
    cfg["hosts"] = {"new": {}}
    if keep:
        cfg["old"] = old
        cfg["again"] = [old]
    return None


def where():
    return Path("/tmp") / "cfg.json"


def kind(value):
    return type(value).__name__


def strict(text):
    return json.loads(text)


@contextlib.contextmanager
def _guard(path, swallow):
    EVENTS.append(["enter", type(path).__name__, _plain.locked()])
    try:
        yield
    except ValueError as exc:
        EVENTS.append(["raised", str(exc)])
        if not swallow:
            raise
    finally:
        EVENTS.append(["exit"])


def state():
    return {"events": EVENTS, "plain": _plain.locked()}
`

func TestProviderModuleJSONCallKeepsIdentityPathsAndOrder(t *testing.T) {
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if host := GetPluginManager(ctx).pluginHostInstance; host != nil {
			host.Shutdown()
		}
	})
	dir := filepath.Join(h, "plugins", "jsonmem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "__init__.py"), "")
	writeFile(t, filepath.Join(dir, "client.py"), jsonProviderSource)
	module := UserMemoryNamespace + ".jsonmem__source_0123456789abcdef"
	out, loaded, err := ProviderModuleJSONCall(ctx, dir, module, "client", "block", `[{"z": 1, "hosts": {"b": {"k": 1.0}, "a": {}}}, "b"]`, nil, []string{"block", "where"}, [][]any{{0, "hosts"}})
	if err != nil || !loaded {
		t.Fatalf("block %v %v", loaded, err)
	}
	args, result, tracked, err := out.Graph()
	if err != nil || out.ResultIsPath {
		t.Fatalf("graph %v", err)
	}
	cfg, _ := args[0].(*pyjson.Object)
	hosts, _ := cfg.GetOr("hosts", nil).(*pyjson.Object)
	if !reflect.DeepEqual(cfg.Keys, []string{"z", "hosts", "seen"}) || args[1] != "b" || !reflect.DeepEqual(cfg.GetOr("seen", nil), []any{"b"}) {
		t.Fatalf("args after %v", *out.RootJSON)
	}
	block, _ := result.(*pyjson.Object)
	if !reflect.DeepEqual(out.ResultKeys, [][]string{{"b"}}) {
		t.Fatalf("result keys %v", out.ResultKeys)
	}
	if block == nil || block != hosts.GetOr("b", nil) || block.GetOr("k", nil) != float64(1) || tracked[0] != any(hosts) {
		t.Fatalf("identity lost in %v shared %v", *out.RootJSON, out.Shared)
	}
	out, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "block", `[{"hosts": {"a": {}}}, "a"]`, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, result, _, _ = out.Graph()
	fresh, _ := result.(*pyjson.Object)
	if fresh == nil || fresh.Len() != 0 || fresh == args[0].(*pyjson.Object).GetOr("hosts", nil).(*pyjson.Object).GetOr("a", nil) {
		t.Fatalf("fresh block %v", *out.RootJSON)
	}
	out, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "swap", `[{"hosts": {"a": {}}}, true]`, nil, nil, [][]any{{0, "hosts"}})
	if err != nil {
		t.Fatal(err)
	}
	args, _, tracked, _ = out.Graph()
	cfg = args[0].(*pyjson.Object)
	if tracked[0] != cfg.GetOr("old", nil) || tracked[0] != cfg.GetOr("again", nil).([]any)[0] || tracked[0] == cfg.GetOr("hosts", nil) {
		t.Fatalf("moved hosts %v shared %v", *out.RootJSON, out.Shared)
	}
	out, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "swap", `[{"hosts": {"a": {}}}, false]`, nil, nil, [][]any{{0, "hosts"}})
	if err != nil {
		t.Fatal(err)
	}
	args, _, tracked, _ = out.Graph()
	orphan, _ := tracked[0].(*pyjson.Object)
	if orphan == nil || !reflect.DeepEqual(orphan.Keys, []string{"a"}) || orphan == args[0].(*pyjson.Object).GetOr("hosts", nil) || len(out.Shared) != 0 {
		t.Fatalf("orphan hosts %v shared %v", *out.RootJSON, out.Shared)
	}
	out, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "where", `[]`, nil, nil, nil)
	if err != nil || !out.ResultIsPath || out.ResultPath != "/tmp/cfg.json" {
		t.Fatalf("where %v %v", out, err)
	}
	out, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "kind", `["/tmp/cfg.json"]`, []int{0}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, result, _, _ = out.Graph(); result != "PosixPath" {
		t.Fatalf("kind %v", result)
	}
	_, loaded, err = ProviderModuleJSONCall(ctx, dir, module, "client", "strict", `["{not json"]`, nil, nil, nil)
	if !loaded || !RemoteIsInstance(err, "ValueError") || RemoteIsInstance(err, "TypeError") {
		t.Fatalf("strict %v %v", loaded, err)
	}
	var remote *PluginHostError
	if !errors.As(err, &remote) || remote.TypeName != "JSONDecodeError" {
		t.Fatalf("strict type %v", err)
	}
	_, _, err = ProviderModuleJSONCall(ctx, dir, module, "client", "kind", `[1]`, nil, []string{"absent"}, nil)
	if !RemoteIsInstance(err, "AttributeError") {
		t.Fatalf("require %v", err)
	}
}

func TestProviderModuleHoldEntersAndLeavesOnOneThread(t *testing.T) {
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if host := GetPluginManager(ctx).pluginHostInstance; host != nil {
			host.Shutdown()
		}
	})
	dir := filepath.Join(h, "plugins", "jsonmem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "__init__.py"), "")
	writeFile(t, filepath.Join(dir, "client.py"), jsonProviderSource)
	module := UserMemoryNamespace + ".jsonmem__source_0123456789abcdef"
	state := func() string {
		out, _, err := ProviderModuleJSONCall(ctx, dir, module, "client", "state", `[]`, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, result, _, err := out.Graph()
		if err != nil {
			t.Fatal(err)
		}
		text, _ := pyjson.Dumps(result)
		return text
	}
	managers := func(swallow string) []ProviderModuleManager {
		return []ProviderModuleManager{{Name: "_lock"}, {Name: "_plain"}, {Name: "_guard", Call: true, ArgsJSON: `["/tmp/cfg.json", ` + swallow + `]`, PathArgs: []int{0}}}
	}
	hold, loaded, err := HoldProviderModuleManagers(ctx, dir, module, "client", managers("false"))
	if err != nil || !loaded {
		t.Fatalf("hold %v %v", loaded, err)
	}
	if got := state(); got != `{"events": [["enter", "PosixPath", true]], "plain": true}` {
		t.Fatalf("held state %s", got)
	}
	suppressed, err := hold.Release(ctx, "", "")
	if err != nil || suppressed {
		t.Fatalf("release %v %v", suppressed, err)
	}
	if got := state(); got != `{"events": [["enter", "PosixPath", true], ["exit"]], "plain": false}` {
		t.Fatalf("released state %s", got)
	}
	if _, err := hold.Release(ctx, "", ""); !RemoteIsInstance(err, "LookupError") {
		t.Fatalf("second release %v", err)
	}
	hold, _, err = HoldProviderModuleManagers(ctx, dir, module, "client", managers("false"))
	if err != nil {
		t.Fatal(err)
	}
	if suppressed, err = hold.Release(ctx, "ValueError", "bad field"); err != nil || suppressed {
		t.Fatalf("raising release %v %v", suppressed, err)
	}
	hold, _, err = HoldProviderModuleManagers(ctx, dir, module, "client", managers("true"))
	if err != nil {
		t.Fatal(err)
	}
	if suppressed, err = hold.Release(ctx, "ValueError", "eaten"); err != nil || !suppressed {
		t.Fatalf("swallowing release %v %v", suppressed, err)
	}
	want := `{"events": [["enter", "PosixPath", true], ["exit"], ["enter", "PosixPath", true], ["raised", "bad field"], ["exit"], ` +
		`["enter", "PosixPath", true], ["raised", "eaten"], ["exit"]], "plain": false}`
	if got := state(); got != want {
		t.Fatalf("final state %s", got)
	}
	_, loaded, err = HoldProviderModuleManagers(ctx, dir, module, "client", []ProviderModuleManager{{Name: "_plain"}, {Name: "_absent"}})
	if !loaded || !RemoteIsInstance(err, "AttributeError") {
		t.Fatalf("absent manager %v %v", loaded, err)
	}
	if got := state(); got != want {
		t.Fatalf("state after a failed enter %s", got)
	}
}
