package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const hookSourcePlugin = `
def _hook(**kwargs):
    return None


def register(ctx):
    ctx.register_hook("on_session_start", _hook)
`

// General discovery of a re-exporting package or a module function entry point owns the fallback hooks of its module file
func TestGeneralLoadOwnsFallbackHookSource(t *testing.T) {
	installLiveServices(t)
	installHostEnvHooks()
	python := liveInterpreters(t)["system"]
	root := t.TempDir()
	h := filepath.Join(root, "icebath-home")
	dir := filepath.Join(h, "plugins", "dual-source")
	for _, d := range []string{dir, filepath.Join(root, "bundled-plugins")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: dual-source\nversion: \"1.0\"\nprovides_hooks:\n  - on_session_start\n")
	writeFile(t, filepath.Join(dir, "implementation.py"), hookSourcePlugin)
	writeFile(t, filepath.Join(dir, "__init__.py"), "from .implementation import register\n")
	site := writeEntryPointSite(t, "epsrc", "1.0", "entry point hook source", "[icebath_agent.plugins]\nep-source = epsrc:register\n")
	writeFile(t, filepath.Join(site, "epsrc.py"), hookSourcePlugin)
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - dual-source\n  - ep-source\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(root, "bundled-plugins"))
	t.Setenv("PYTHONPATH", site)
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m := NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	sources := map[string]string{"dual-source": filepath.Join(dir, "__init__.py"), "ep-source": filepath.Join(site, "epsrc.py")}
	calls := 0
	fallback := func(name string) *Registration {
		c := newContext(&Manifest{Name: name, Key: name}, m)
		return m.registerFallbackHookWith(c, hookSourceOf(name, sources[name]), "on_session_start", func() *Registration {
			calls++
			return c.track("hook", "on_session_start", func() error { return nil }, false)
		})
	}
	early := map[string]*Registration{"dual-source": fallback("dual-source"), "ep-source": fallback("ep-source")}
	if calls != 2 {
		t.Fatalf("fallback registrations before discovery %d", calls)
	}
	m.DiscoverAndLoad(homeCtx(h), false)
	for name, file := range sources {
		m.mu.RLock()
		loaded := m.plugins[name]
		m.mu.RUnlock()
		if loaded == nil || !loaded.Enabled || loaded.Error != "" {
			t.Fatalf("%s loaded %+v", name, loaded)
		}
		if got, want := hookSourceOf(name, loaded.ModuleFile), hookSourceOf(name, file); got == nil || *got != *want {
			t.Fatalf("%s hook source %v want %v", name, got, want)
		}
		if early[name].Active() {
			t.Fatalf("%s fallback hook survived general discovery of its source", name)
		}
		fallback(name)
	}
	if calls != 2 {
		t.Fatalf("fallback hooks registered again for an owned source, %d registrations", calls)
	}
	if n := len(m.memoryHookRegistrations[*hookSourceOf("dual-source", sources["dual-source"])]); n != 1 {
		t.Fatalf("owned source ledger holds %d handles", n)
	}
}
