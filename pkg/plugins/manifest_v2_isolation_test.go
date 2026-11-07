package plugins

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Loads user plugins from init sources under the given isolation, returning the manager and any escaped panic
func isoLoad(t *testing.T, isolation string, inits map[string]string, pluginsYAML string) (m *Manager, escaped any) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	names := []string{}
	for name, init := range inits {
		mv2Write(t, filepath.Join(h, "plugins", name, "plugin.yaml"), "name: "+name+"\nversion: 0.1.0\ndescription: test "+name+"\n")
		mv2Write(t, filepath.Join(h, "plugins", name, "__init__.py"), init)
		names = append(names, name)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: "+isolation+"\n  enabled: ["+strings.Join(names, ", ")+"]\n"+pluginsYAML)
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "0")
	python := liveInterpreters(t)["system"]
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m = NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	func() {
		defer func() { escaped = recover() }()
		m.DiscoverAndLoad(homeCtx(h), false)
	}()
	return m, escaped
}

func isoHooks(m *Manager, name string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks[name])
}

func TestSysExitInPluginIsIsolatedAndNamed(t *testing.T) {
	inits := map[string]string{"b_exit": "import sys\nsys.exit(0)\n", "c_after": "def register(ctx):\n    pass\n"}
	t.Run("in_process", func(t *testing.T) {
		m, escaped := isoLoad(t, IsolationInProcess, inits, "")
		if escaped != nil || !m.discovered {
			t.Fatalf("discovery raised %v discovered %v", escaped, m.discovered)
		}
		if !m.plugins["c_after"].Enabled || m.plugins["b_exit"].Enabled || m.plugins["b_exit"].Error != "SystemExit(0) raised during import/register()" {
			t.Fatalf("c_after %+v b_exit %+v", m.plugins["c_after"], m.plugins["b_exit"])
		}
	})
	t.Run("string_code", func(t *testing.T) {
		m, _ := isoLoad(t, IsolationInProcess, map[string]string{"b_exit": "import sys\nsys.exit('bye')\n"}, "")
		if got := m.plugins["b_exit"].Error; got != "SystemExit('bye') raised during import/register()" {
			t.Fatalf("string code %q", got)
		}
	})
	t.Run("hosted", func(t *testing.T) {
		m, _ := isoLoad(t, IsolationHost, inits, "")
		if got := m.plugins["b_exit"].Error; got != "SystemExit: 0" || !m.plugins["c_after"].Enabled {
			t.Fatalf("hosted %q", got)
		}
	})
}

func TestKeyboardInterruptStillPropagates(t *testing.T) {
	t.Run("in_process", func(t *testing.T) {
		_, escaped := isoLoad(t, IsolationInProcess, map[string]string{"ctrlc": "raise KeyboardInterrupt\n"}, "")
		if _, ok := escaped.(*UserInterrupt); !ok {
			t.Fatalf("escaped %#v", escaped)
		}
	})
	t.Run("hosted", func(t *testing.T) {
		m, escaped := isoLoad(t, IsolationHost, map[string]string{"ctrlc": "raise KeyboardInterrupt\n"}, "")
		if escaped != nil || m.plugins["ctrlc"].Enabled || m.plugins["ctrlc"].Error != "KeyboardInterrupt: " {
			t.Fatalf("hosted escaped %v plugin %+v", escaped, m.plugins["ctrlc"])
		}
	})
}

func TestRegisterOverrunningLoadTimeoutSkipsOnlyThatPlugin(t *testing.T) {
	gates := t.TempDir()
	gate, done := filepath.Join(gates, "gate"), filepath.Join(gates, "done")
	slow := "import os, time\ndef register(ctx):\n    ctx.register_hook('pre_tool_call', lambda **kw: None)\n" +
		"    deadline = time.time() + 5\n    while not os.path.exists(" + pyQuote(gate) + ") and time.time() < deadline:\n        time.sleep(0.02)\n" +
		"    ctx.register_hook('post_tool_call', lambda **kw: None)\n    open(" + pyQuote(done) + ", 'w').close()\n"
	m, escaped := isoLoad(t, IsolationInProcess, map[string]string{"b_slow": slow, "c_after": "def register(ctx):\n    pass\n"}, "  load_timeout_seconds: 0.3\n")
	if escaped != nil || !m.plugins["c_after"].Enabled || m.plugins["b_slow"].Enabled {
		t.Fatalf("escaped %v c_after %+v b_slow %+v", escaped, m.plugins["c_after"], m.plugins["b_slow"])
	}
	if got := m.plugins["b_slow"].Error; got != "load timed out after 0.3s (import + register() never returned)" {
		t.Fatalf("error %q", got)
	}
	if n := isoHooks(m, "pre_tool_call"); n != 0 {
		t.Fatalf("pre-hang hook kept %d", n)
	}
	writeFile(t, gate, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("abandoned register never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := isoHooks(m, "post_tool_call"); n != 0 {
		t.Fatalf("late hook landed %d", n)
	}
}

type isoInline struct{ ids *[]uint64 }

func (p isoInline) Manifest() Manifest { return Manifest{Name: "inline"} }
func (p isoInline) Register(*Context) error {
	*p.ids = append(*p.ids, isoGoroutineID())
	return nil
}

func isoGoroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	id, _ := strconv.ParseUint(strings.Fields(strings.TrimPrefix(string(buf), "goroutine "))[0], 10, 64)
	return id
}

func TestLoadTimeoutZeroRunsRegisterInline(t *testing.T) {
	bundledMu.Lock()
	savedByKey, savedOrder := bundledByKey, bundledOrder
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
	bundledMu.Unlock()
	t.Cleanup(func() {
		bundledMu.Lock()
		bundledByKey, bundledOrder = savedByKey, savedOrder
		bundledMu.Unlock()
	})
	var ids []uint64
	RegisterBundled(isoInline{ids: &ids})
	for _, c := range []struct {
		timeout string
		inline  bool
	}{{"0", true}, {"5", false}} {
		h := t.TempDir()
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [inline]\n  load_timeout_seconds: "+c.timeout+"\n")
		t.Setenv("ICEBATH_HOME", h)
		ids = nil
		m := NewManager(h)
		m.DiscoverAndLoad(homeCtx(h), false)
		if !m.plugins["inline"].Enabled || len(ids) != 1 || (ids[0] == isoGoroutineID()) != c.inline {
			t.Fatalf("timeout %s plugin %+v ids %v caller %d", c.timeout, m.plugins["inline"], ids, isoGoroutineID())
		}
	}
}
