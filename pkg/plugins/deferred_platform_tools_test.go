package plugins

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Python preamble counting one import kind into a probe file the Go test reads back
func deferredProbePreamble(probe string) string {
	return "PROBE = " + strconv.Quote(probe) + "\n" +
		"def _bump(kind):\n" +
		"    with open(PROBE + '/' + kind, 'a') as f:\n" +
		"        f.write('x\\n')\n" +
		"def _count(kind):\n" +
		"    try:\n" +
		"        with open(PROBE + '/' + kind) as f:\n" +
		"            return len(f.read().splitlines())\n" +
		"    except FileNotFoundError:\n" +
		"        return 0\n"
}

// Probe of one synthetic platform plugin, its directory, manifest and import counters
type deferredProbe struct {
	dir, probe string
	mf         *Manifest
}

func (p *deferredProbe) count(kind string) int {
	raw, err := os.ReadFile(filepath.Join(p.probe, kind))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (p *deferredProbe) write(t *testing.T, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(p.dir, name), deferredProbePreamble(p.probe)+body)
}

// Bundled style platform plugin whose adapter import is deferred into register, the hermes _write_platform_plugin twin
func writeDeferredPlatform(t *testing.T, platform string, withTools bool, declares bool) *deferredProbe {
	t.Helper()
	root := t.TempDir()
	p := &deferredProbe{dir: filepath.Join(root, platform), probe: filepath.Join(root, "probe")}
	for _, d := range []string{p.dir, p.probe} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var provides []string
	manifest := "name: " + platform + "-platform\nkind: platform\nversion: 1.0.0\n"
	if declares {
		provides = []string{platform + "_call"}
		manifest += "provides_tools:\n- " + platform + "_call\n"
	}
	writeFile(t, filepath.Join(p.dir, "plugin.yaml"), manifest)
	p.write(t, "adapter.py", "_bump('adapter_imports')\n")
	init := "_bump('package_execs')\n\ndef register(ctx):\n    from . import adapter  # noqa: F401\n"
	if withTools {
		p.write(t, "tools.py", "_bump('tools_execs')\n\n"+
			"def _handler(**kwargs):\n    return 'ok'\n\n"+
			"def register_tools(ctx):\n"+
			"    ctx.register_tool(name='"+platform+"_call', toolset='"+platform+"', "+
			"schema={'type': 'function', 'function': {'name': '"+platform+"_call', 'description': 'call a peer', "+
			"'parameters': {'type': 'object', 'properties': {}}}}, handler=_handler, description='call a peer')\n")
		init += "    from .tools import register_tools\n    register_tools(ctx)\n"
	}
	p.write(t, "__init__.py", init)
	p.mf = &Manifest{Name: platform + "-platform", Kind: "platform", Source: "bundled", Path: p.dir, Key: platform + "-platform", ProvidesTools: provides}
	return p
}

// Copy of the manager row of a plugin key read under the manager lock
func deferredRow(t *testing.T, m *Manager, key string) LoadedPlugin {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.plugins[key]
	if p == nil {
		t.Fatalf("plugin %s has no manager row", key)
	}
	return *p
}

func deferredPredeclared(m *Manager, key string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	got, ok := m.predeclaredTools[key]
	return got, ok
}

// Whether a WARN log line names every given text
func warnedWith(logs string, texts ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "level=WARN") {
			continue
		}
		hit := true
		for _, text := range texts {
			hit = hit && strings.Contains(line, text)
		}
		if hit {
			return true
		}
	}
	return false
}

func resolveProbe(h string) []string {
	return tools.Default.ResolveToolset(homeCtx(h), h, "probeplat")
}

// Rewrites tools.py so register_tools raises after registering its one tool
func breakAfterFirstTool(t *testing.T, p *deferredProbe) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dir, "tools.py"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.dir, "tools.py"), string(raw)+"    raise RuntimeError('boom after the first tool')\n")
}
