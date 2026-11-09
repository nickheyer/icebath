package tools

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const registeringSource = "from tools.registry import registry\n" +
	"registry.register(name='conn', toolset='x', schema={}, handler=lambda *_a, **_k: '{}')\n"

func writeDiscoveryFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeDiscoveryPkg(t *testing.T, init bool) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	toolsDir := filepath.Join(t.TempDir(), "tools")
	pkg := filepath.Join(toolsDir, "connectors")
	writeDiscoveryFile(t, filepath.Join(toolsDir, "__init__.py"), "")
	if init {
		writeDiscoveryFile(t, filepath.Join(pkg, "__init__.py"), "")
	}
	writeDiscoveryFile(t, filepath.Join(pkg, "tool.py"), registeringSource)
	return toolsDir, pkg
}

func mustDiscover(t *testing.T, toolsDir string) []string {
	t.Helper()
	names, err := discoverToolModules(toolsDir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestSkipsMcpToolEvenIfItRegisters(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	toolsDir := filepath.Join(t.TempDir(), "tools")
	writeDiscoveryFile(t, filepath.Join(toolsDir, "__init__.py"), "")
	writeDiscoveryFile(t, filepath.Join(toolsDir, "mcp_tool.py"), "from tools.registry import registry\nregistry.register(name='mcp_alpha', toolset='mcp-test', schema={}, handler=lambda *_a, **_k: '{}')\n")
	writeDiscoveryFile(t, filepath.Join(toolsDir, "alpha.py"), "from tools.registry import registry\nregistry.register(name='alpha', toolset='x', schema={}, handler=lambda *_a, **_k: '{}')\n")
	if got := mustDiscover(t, toolsDir); !reflect.DeepEqual(got, []string{"tools.alpha"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPackageToolPyIsImportedUnderItsDottedName(t *testing.T) {
	toolsDir, _ := makeDiscoveryPkg(t, true)
	if got := mustDiscover(t, toolsDir); !reflect.DeepEqual(got, []string{"tools.connectors.tool"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPackageSiblingsAreLibrariesNotScanned(t *testing.T) {
	toolsDir, pkg := makeDiscoveryPkg(t, true)
	writeDiscoveryFile(t, filepath.Join(pkg, "operation.py"), registeringSource)
	if got := mustDiscover(t, toolsDir); !reflect.DeepEqual(got, []string{"tools.connectors.tool"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPackageWithoutInitIsSkippedLoudly(t *testing.T) {
	toolsDir, _ := makeDiscoveryPkg(t, false)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	if got := mustDiscover(t, toolsDir); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(buf.String(), "__init__.py") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestCacheRoundTripsTheNestedVerdict(t *testing.T) {
	toolsDir, pkg := makeDiscoveryPkg(t, true)
	mustDiscover(t, toolsDir)
	tool := filepath.Join(pkg, "tool.py")
	if err := os.Chmod(tool, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(tool, 0o644)
	if registers, _ := moduleRegistersTools(tool); registers {
		t.Fatal("an unreadable file must not register, so a rescan would drop the module")
	}
	if got := mustDiscover(t, toolsDir); !reflect.DeepEqual(got, []string{"tools.connectors.tool"}) {
		t.Fatalf("got %v", got)
	}
}

// Module body and module level for loop registrations count, function bodies and unparsable files do not
func TestModuleRegistersTools(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		src     string
		want    bool
		wantErr string
	}{
		{registeringSource, true, ""},
		{"\ufeff" + registeringSource, true, ""},
		{"from tools.registry import registry\nfor n in ('a', 'b'):\n    registry.register(name=n)\n", true, ""},
		{"from tools.registry import registry\ndef setup():\n    registry.register(name='a')\n", false, ""},
		{"from tools.registry import registry\nif True:\n    registry.register(name='a')\n", false, ""},
		{"from tools.registry import registry\nx = registry.register(name='a')\n", false, ""},
		{"from tools.registry import registry\nother.registry.register(name='a')\n", false, ""},
		{"registry.register(\n", false, ""},
		{"registry.register(name='a')\x00\n", false, ""},
		{"r.register(name='a')\n", false, ""},
		{"registry.register(name='\xff')\n", false, "'utf-8' codec can't decode byte 0xff in position 24: invalid start byte"},
	}
	for i, c := range cases {
		p := filepath.Join(dir, "m"+string(rune('a'+i))+".py")
		writeDiscoveryFile(t, p, c.src)
		got, err := moduleRegistersTools(p)
		if got != c.want || (err == nil) != (c.wantErr == "") || (err != nil && err.Error() != c.wantErr) {
			t.Errorf("%q: got %v %v", c.src, got, err)
		}
	}
	if got, err := moduleRegistersTools(filepath.Join(dir, "gone.py")); got || err != nil {
		t.Fatalf("missing %v %v", got, err)
	}
}
