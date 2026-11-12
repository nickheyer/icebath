package plugins_test

import (
	"maps"
	"testing"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const pageBackendPlugin = `
import abc, sys, types

# Stands in for the upstream agent.browser_page_backend module the way a hermes test installs a module double
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

func TestLiveInProcessPluginBrowserPageBackendEvaluatesInTheHost(t *testing.T) {
	world := plugins.NewLoginWorld(t, "camo-page", pageBackendPlugin)
	world.SetTier("verified")
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.ProviderRegistries = maps.Clone(next.ProviderRegistries)
	if next.ProviderRegistries == nil {
		next.ProviderRegistries = map[string]plugins.ScopedRegistry{}
	}
	next.ProviderRegistries["browser_page_backend"] = browser.BrowserPageBackendScopedRegistry()
	plugins.InstallServices(&next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
	if err := world.InstallCatalogEntry("", false); err != nil {
		t.Fatal(err)
	}
	world.Load(t, "plugins:\n  enabled: [camo-page]\n")
	backend := browser.BrowserPageBackendRegistry.GetProvider(world.Ctx(), "camo", world.Home())
	if backend == nil {
		t.Fatal("the hosted camo backend did not register")
	}
	if backend.Name() != "camo" || backend.DisplayName() != "camo" || backend.GetSetupSchema()["name"] != "camo" {
		t.Fatalf("identity = %s %s %v", backend.Name(), backend.DisplayName(), backend.GetSetupSchema())
	}
	if !backend.HasPage("t1") || backend.HasPage("t2") {
		t.Fatal("has_page did not answer from the host")
	}
	got, err := backend.Evaluate("t1", "document.title", true)
	value, _ := got.(map[string]any)
	if err != nil || value["task"] != "t1" || value["expression"] != "document.title" || value["secret"] != true {
		t.Fatalf("evaluate = %v %v", got, err)
	}
	if browser.BrowserPageBackendRegistry.GetProvider(world.Ctx(), "lookalike", world.Home()) != nil {
		t.Fatal("an object that is not a BrowserPageBackend registered")
	}
}
