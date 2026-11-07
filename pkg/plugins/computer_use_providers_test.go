package plugins

import (
	"path/filepath"
	"strings"
	"testing"
)

// Hermes backend plugin fixture whose provider and backend live in the host child
const cuProviderFixture = `
from pathlib import Path
from tools.computer_use.backend import ActionResult, CaptureResult, ComputerUseBackend, ComputerUseProvider, UIElement

TAG = Path(__file__).parent.name
CALLS = []

class FakeBackend(ComputerUseBackend):
    def __init__(self, mode): self.mode = mode
    def start(self): CALLS.append(("start", self.mode))
    def stop(self): CALLS.append(("stop", None))
    def is_available(self): return True
    def capture(self, mode="som", app=None, pid=None, window_id=None):
        return CaptureResult(mode=mode, width=10, height=20, app=app or "",
                             elements=[UIElement(index=1, role="AXButton", label="OK", bounds=(1, 2, 3, 4))])
    def click(self, **kw):
        CALLS.append(("click", kw.get("element")))
        return ActionResult(ok=True, action="click")
    def drag(self, **kw): return ActionResult(ok=True, action="drag")
    def scroll(self, **kw): return ActionResult(ok=True, action="scroll")
    def type_text(self, text, **kw): return ActionResult(ok=True, action="type")
    def key(self, keys, **kw): return ActionResult(ok=True, action="key")
    def list_apps(self): return [{"backend": TAG, "file": __file__, "calls": [list(c) for c in CALLS]}]
    def focus_app(self, app, raise_window=False): return ActionResult(ok=True, action="focus_app")
    def set_value(self, value, element=None): return ActionResult(ok=True, action="set_value")

class FakeProvider(ComputerUseProvider):
    name = TAG
    def create_backend(self, *, permission_mode): return FakeBackend(permission_mode)

def register(ctx):
    ctx.register_tool("ignored", None, None, None)
    ctx.register_computer_use_provider(FakeProvider())
`

func TestComputerUseProviderInHostAnswersHostedBackend(t *testing.T) {
	ctx, h := engineHome(t)
	dir := filepath.Join(h, "plugins", "cu-beta")
	writeTree(t, dir, map[string]string{"__init__.py": cuProviderFixture, "plugin.yaml": "name: cu-beta\n"})
	provider, err := LoadComputerUseProviderInHost(ctx, dir, "_icebath_user_computer_use.cu-beta")
	if err != nil || provider == nil || provider.Base != computerUseProviderBase {
		t.Fatalf("provider %v err %v", provider, err)
	}
	if name, err := provider.Get(ctx, "name"); err != nil || name != "cu-beta" {
		t.Fatalf("name %v err %v", name, err)
	}
	raw, err := provider.Call(ctx, "create_backend", nil, map[string]any{"permission_mode": "standard"})
	backend, ok := raw.(*HostedObject)
	if err != nil || !ok || backend.Base != "tools.computer_use.backend:ComputerUseBackend" {
		t.Fatalf("create_backend %T %v err %v", raw, raw, err)
	}
	if _, err := backend.Call(ctx, "start", nil, nil); err != nil {
		t.Fatal(err)
	}
	clicked, err := backend.Call(ctx, "click", nil, map[string]any{"element": 3})
	rec, ok := clicked.(Record)
	if err != nil || !ok || rec["ok"] != true || rec["action"] != "click" {
		t.Fatalf("click %T %v err %v", clicked, clicked, err)
	}
	captured, err := backend.Call(ctx, "capture", nil, map[string]any{"mode": "ax", "app": "Safari"})
	cap, ok := captured.(Record)
	if err != nil || !ok || cap["mode"] != "ax" || cap["app"] != "Safari" {
		t.Fatalf("capture %T %v err %v", captured, captured, err)
	}
	elements, _ := cap["elements"].([]any)
	if len(elements) != 1 {
		t.Fatalf("elements %v", cap["elements"])
	}
	if el, ok := elements[0].(Record); !ok || el["role"] != "AXButton" || el["label"] != "OK" {
		t.Fatalf("element %T %v", elements[0], elements[0])
	}
	apps, err := backend.Call(ctx, "list_apps", nil, nil)
	list, _ := apps.([]any)
	if err != nil || len(list) != 1 {
		t.Fatalf("list_apps %v err %v", apps, err)
	}
	app, _ := list[0].(map[string]any)
	if app["backend"] != "cu-beta" || !strings.HasPrefix(app["file"].(string), dir) {
		t.Fatalf("app %v", app)
	}
	calls, _ := app["calls"].([]any)
	if len(calls) != 2 || calls[0].([]any)[0] != "start" || calls[0].([]any)[1] != "standard" || calls[1].([]any)[1] != int64(3) {
		t.Fatalf("calls %v", calls)
	}
	if waited, err := backend.Call(ctx, "wait", []any{0.0}, nil); err != nil || waited.(Record)["action"] != "wait" {
		t.Fatalf("wait %v err %v", waited, err)
	}
}

func TestComputerUseProviderInHostSubclassFallbackAndEmptyDir(t *testing.T) {
	ctx, h := engineHome(t)
	sub := filepath.Join(h, "plugins", "cu-sub")
	writeTree(t, sub, map[string]string{"__init__.py": "from tools.computer_use.backend import ComputerUseProvider\n" +
		"class Sub(ComputerUseProvider):\n    name = 'cu-sub'\n    def create_backend(self, *, permission_mode): return None\n" +
		"def register(ctx):\n    raise RuntimeError('boom')\n"})
	provider, err := LoadComputerUseProviderInHost(ctx, sub, "_icebath_user_computer_use.cu-sub")
	if err != nil || provider == nil {
		t.Fatalf("provider %v err %v", provider, err)
	}
	if name, _ := provider.Get(ctx, "name"); name != "cu-sub" {
		t.Fatalf("name %v", name)
	}
	empty := filepath.Join(h, "plugins", "cu-empty")
	writeTree(t, empty, map[string]string{"__init__.py": "VALUE = 1\n"})
	if got, err := LoadComputerUseProviderInHost(ctx, empty, "_icebath_user_computer_use.cu-empty"); err != nil || got != nil {
		t.Fatalf("empty dir %v err %v", got, err)
	}
}
