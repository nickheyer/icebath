package plugins

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type mv2Plugin struct{ dir, manifest, init string }

// Plugin whose manifest carries the name version and description lines plus extra and whose register runs body
func mv2Dir(name, extra, body string) mv2Plugin {
	return mv2Plugin{dir: name, manifest: "name: " + name + "\nversion: 0.1.0\ndescription: test " + name + "\n" + extra,
		init: "def register(ctx):\n    " + body + "\n"}
}

// Log capture of the plugins logger at the given level, the caplog at_level twin
func mv2Logs(t *testing.T, level slog.Level) *bytes.Buffer {
	var buf bytes.Buffer
	saved := Logger
	Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: level}))
	t.Cleanup(func() { Logger = saved })
	return &buf
}

// Python statement appending the repr of expr to a probe file
func mv2Probe(probe, expr string) string {
	return "open(" + pyQuote(probe) + ", 'a').write(repr(" + expr + ") + '\\n')"
}

// File write that creates the parent directories first
func mv2Write(t *testing.T, path, body string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body)
}

func readProbe(t *testing.T, probe string) []string {
	raw, err := os.ReadFile(probe)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// Fake pip pip3 and uv executables ahead of PATH that record every invocation
func mv2SpyInstallers(t *testing.T) string {
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls.txt")
	for _, name := range []string{"pip", "pip3", "uv"} {
		writeFile(t, filepath.Join(bin, name), "#!/bin/sh\necho \"$0 $*\" >> "+calls+"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestListManifestIsRejectedWithAClearReasonAndHooksAlias(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	base := filepath.Join(t.TempDir(), "plugins")
	mv2Write(t, filepath.Join(base, "listy", "plugin.yaml"), "- name: listy\n")
	good := mv2Dir("hooky", "hooks: [pre_tool_call]\n", "pass")
	mv2Write(t, filepath.Join(base, "hooky", "plugin.yaml"), good.manifest)
	mv2Write(t, filepath.Join(base, "hooky", "__init__.py"), good.init)
	byName := map[string]*Manifest{}
	for _, mf := range ScanDirectory(base, "user", nil) {
		byName[mf.Name] = mf
	}
	if _, ok := byName["listy"]; ok || !strings.Contains(logs.String(), "top level must be a mapping") {
		t.Fatalf("manifests %v logs %s", byName, logs)
	}
	if h := byName["hooky"]; h == nil || !reflect.DeepEqual(h.ProvidesHooks, []string{"pre_tool_call"}) || h.Path != filepath.Join(base, "hooky") {
		t.Fatalf("hooky = %+v", h)
	}
}
