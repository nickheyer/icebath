package plugins

import ()

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
