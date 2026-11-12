package plugins

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Hermes source files and fixtures the cases read, copied byte for byte under testdata
const hermesReferenceTree = "testdata/hermes"

var coreTreeSkipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true}

// Packages with an __init__.py and root module stems of one import root
func importRootNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(root, e.Name(), "__init__.py")); err == nil && fi.Mode().IsRegular() {
			names[e.Name()] = true
		}
	}
	mods, err := pypath.Glob(root, "*.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		names[pypath.PathStem(m)] = true
	}
	return names
}

// Table lines the plugin SDK roots of the host child contribute, derived the way the source audits read a checkout
func hostChildTableLines(t *testing.T, hostchild string) []string {
	t.Helper()
	sdkRoots := []string{hostchild, filepath.Join(hostchild, "sdk")}
	lines := map[string]bool{"top hermes_plugins": true, "top icebath_plugins": true}
	for _, r := range sdkRoots {
		for n := range importRootNames(t, r) {
			if n != "setup" && n != "conftest" {
				lines["top "+n], lines["pkg "+n] = true, true
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(hostchild, "icebath_cli", "plugins.py"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := pyast.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range pyast.Walk(tree) {
		if !node.Is("ClassDef") {
			continue
		}
		for _, item := range node.Children("body") {
			name, _ := item.Str("name")
			if item.Is("FunctionDef", "AsyncFunctionDef") && strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "__") {
				lines["priv "+name] = true
			}
		}
	}
	for _, root := range sdkRoots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == root {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
			if d.IsDir() && coreTreeSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			fi, err := os.Stat(p)
			if err != nil {
				return nil
			}
			if fi.IsDir() {
				lines["d "+rel] = true
			} else if fi.Mode().IsRegular() && strings.HasSuffix(d.Name(), ".py") {
				lines["f "+rel] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	out := make([]string, 0, len(lines))
	for l := range lines {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func TestCoreTablesHoldTheHostChildTree(t *testing.T) {
	zr, err := gzip.NewReader(bytes.NewReader(coreTablesGz))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	embedded := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		embedded[line] = true
	}
	want := hostChildTableLines(t, "hostchild")
	if len(want) < 100 {
		t.Fatalf("host child gives only %d lines", len(want))
	}
	for _, line := range want {
		if !embedded[line] {
			t.Errorf("embedded core tables lack %q", line)
		}
	}
	for _, tc := range []struct {
		module string
		want   bool
	}{{"agent", true}, {"agent.transports", true}, {"agent.memory_provider", true}, {"plugins.memory.openltm._db", false},
		{"icebath_cli.plugins", true}, {"agent.no_such_module", false}, {"agent.transports..", true}, {"agent.nope.....", false}} {
		if got := coreModuleExists(tc.module); got != tc.want {
			t.Errorf("coreModuleExists(%q) = %v", tc.module, got)
		}
	}
}
