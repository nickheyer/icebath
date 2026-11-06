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

	"github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const hermesReferenceTree = "../../references/projects/hermes-agent"

var coreHandleSources = []string{"hermes_state.py", "hermes_state_*.py", "run_agent.py", "agent/turn_*.py",
	"gateway/run.py", "gateway/run_*.py", "hermes_cli/plugins.py"}

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

// Table text derived the way the hermes source audits read their own checkout, plus the plugin SDK roots
func buildCoreTables(t *testing.T, hermes, hostchild string) string {
	t.Helper()
	sdkRoots := []string{hostchild, filepath.Join(hostchild, "sdk")}
	lines := map[string]bool{}
	top := importRootNames(t, hermes)
	rootModules := map[string]bool{}
	mods, _ := pypath.Glob(hermes, "*.py")
	for _, m := range mods {
		rootModules[pypath.PathStem(m)] = true
	}
	sdk := map[string]bool{}
	for _, r := range sdkRoots {
		for n := range importRootNames(t, r) {
			sdk[n] = true
		}
	}
	for n := range sdk {
		top[n] = true
	}
	top["hermes_plugins"], top["icebath_plugins"] = true, true
	packages := map[string]bool{}
	var pyproject struct {
		Tool struct {
			Setuptools struct {
				Packages struct {
					Find struct {
						Include []string `toml:"include"`
					} `toml:"find"`
				} `toml:"packages"`
			} `toml:"setuptools"`
		} `toml:"tool"`
	}
	raw, err := os.ReadFile(filepath.Join(hermes, "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(raw, &pyproject); err != nil || pyproject.Tool.Setuptools.Packages.Find.Include == nil {
		for n := range top {
			packages[n] = !map[string]bool{"hermes_plugins": true, "icebath_plugins": true, "tests": true, "evals": true}[n]
		}
	} else {
		for _, inc := range pyproject.Tool.Setuptools.Packages.Find.Include {
			packages[strings.Split(inc, ".")[0]] = true
		}
	}
	for n := range rootModules {
		packages[n] = true
	}
	for n := range sdk {
		packages[n] = true
	}
	for n, ok := range top {
		if ok && n != "setup" && n != "conftest" {
			lines["top "+n] = true
		}
	}
	for n, ok := range packages {
		if ok && n != "setup" && n != "conftest" {
			lines["pkg "+n] = true
		}
	}
	var sources []string
	for _, pattern := range coreHandleSources {
		found, err := pypath.Glob(hermes, pattern)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, found...)
	}
	sources = append(sources, filepath.Join(hostchild, "icebath_cli", "plugins.py"))
	for _, src := range sources {
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		text, err := pystr.DecodeUTF8Sig(data)
		if err != nil {
			continue
		}
		tree, err := pyast.Parse(text)
		if err != nil {
			continue
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
	}
	for _, root := range append([]string{hermes}, sdkRoots...) {
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
	return strings.Join(out, "\n") + "\n"
}

func TestCoreTablesMatchTheReferenceTree(t *testing.T) {
	want := buildCoreTables(t, hermesReferenceTree, "hostchild")
	if os.Getenv("ICEBATH_WRITE_CORE_TABLES") == "1" {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write([]byte(want))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("core_tables.txt.gz", buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(coreTablesGz))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("embedded core tables are stale (%d bytes, reference gives %d); rerun with ICEBATH_WRITE_CORE_TABLES=1", len(got), len(want))
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
