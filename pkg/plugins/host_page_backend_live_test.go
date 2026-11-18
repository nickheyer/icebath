package plugins_test

const pageBackendPlugin = `
import abc, sys, types

# Module double standing in for the browser page backend module
_module = types.ModuleType("agent.browser_page_backend")
class BrowserPageBackend(abc.ABC):
    @property
    def display_name(self):
        return self.name
    def get_setup_schema(self):
        return {"name": self.display_name, "env_vars": []}
_module.BrowserPageBackend = BrowserPageBackend
sys.modules["agent.browser_page_backend"] = _module


class Camo(BrowserPageBackend):
    name = "camo"
    def has_page(self, task_id):
        return task_id == "t1"
    def evaluate(self, task_id, expression, *, secret):
        return {"task": task_id, "expression": expression, "secret": secret}


class Lookalike:
    name = "lookalike"
    def has_page(self, task_id):
        return True
    def evaluate(self, task_id, expression, *, secret):
        return None


def register(ctx):
    ctx.register_browser_page_backend(Camo())
    ctx.register_browser_page_backend(Lookalike())
`
