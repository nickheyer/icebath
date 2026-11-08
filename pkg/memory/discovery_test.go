package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const discoveryProviderSource = `from agent.memory_provider import MemoryProvider

class Provider(MemoryProvider):
    @property
    def name(self):
        return "%s"
    def is_available(self):
        return True
    def initialize(self, *a, **kw):
        pass
    def get_tool_schemas(self):
        return []

def register(ctx):
    ctx.register_memory_provider(Provider())
`

func discWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discProvider(t *testing.T, root, name string) string {
	dir := filepath.Join(root, name)
	discWrite(t, filepath.Join(dir, "__init__.py"), strings.Replace(discoveryProviderSource, "%s", name, 1))
	return dir
}

// Fresh home, an empty import path and the system python3 launching every plugin host child
func discHome(t *testing.T) (context.Context, string) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	savedKeys, savedAdapters, savedSecrets := tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets
	t.Cleanup(func() {
		tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets = savedKeys, savedAdapters, savedSecrets
	})
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return nil }
	tools.ProfileSecrets = brvSecretSource{}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("PYTHONPATH", "")
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
	return context.Background(), h
}

// Installs an entry point of the memory group on the import path
func discEntryPoint(t *testing.T, name, value string) string {
	root := t.TempDir()
	discWrite(t, filepath.Join(root, name+"_dist-1.0.dist-info", "entry_points.txt"), "[icebath_agent.memory_providers]\n"+name+" = "+value+"\n")
	t.Setenv("PYTHONPATH", root)
	return root
}

func TestProjectDirIsIgnoredWithoutOptIn(t *testing.T) {
	ctx, _ := discHome(t)
	cwd := t.TempDir()
	discProvider(t, filepath.Join(cwd, ".icebath", "plugins"), "projectmem")
	t.Chdir(cwd)
	if slices.Contains(ListMemoryProviderNames(ctx, nil), "projectmem") || FindProviderDir(ctx, nil, "projectmem") != "" {
		t.Fatal("project provider visible without opt in")
	}
}

func TestProjectDirIsDiscoveredWhenOptedIn(t *testing.T) {
	ctx, _ := discHome(t)
	cwd := t.TempDir()
	provider := discProvider(t, filepath.Join(cwd, ".icebath", "plugins"), "projectmem")
	t.Chdir(cwd)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "1")
	if !slices.Contains(ListMemoryProviderNames(ctx, nil), "projectmem") {
		t.Fatal("opted in project provider not listed")
	}
	if got := FindProviderDir(ctx, nil, "projectmem"); got != provider {
		t.Fatalf("dir %q want %q", got, provider)
	}
	if r := FindProvider(ctx, nil, "projectmem"); r.Source != SourceProject {
		t.Fatalf("source %q", r.Source)
	}
}

func TestBundledStillWinsOverProject(t *testing.T) {
	ctx, _ := discHome(t)
	cwd := t.TempDir()
	discProvider(t, filepath.Join(cwd, ".icebath", "plugins"), "byterover")
	t.Chdir(cwd)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "1")
	if r := FindProvider(ctx, nil, "byterover"); r == nil || r.Source != SourceBundled {
		t.Fatalf("byterover resolved to %+v", r)
	}
}

func TestEntryPointProviderIsListed(t *testing.T) {
	ctx, _ := discHome(t)
	discEntryPoint(t, "pipmem", "pipmem_pkg")
	if !slices.Contains(ListMemoryProviderNames(ctx, nil), "pipmem") {
		t.Fatal("entry point provider not listed")
	}
}

func TestFindProviderDirResolvesAPackageEntryPoint(t *testing.T) {
	ctx, _ := discHome(t)
	root := discEntryPoint(t, "pipmem", "pipmem_pkg:register")
	pkg := filepath.Join(root, "pipmem_pkg")
	discWrite(t, filepath.Join(pkg, "__init__.py"), strings.Replace(discoveryProviderSource, "%s", "pipmem", 1))
	discWrite(t, filepath.Join(pkg, "config_schema.py"), "CONFIG_SCHEMA = None\n")
	if got := FindProviderDir(ctx, nil, "pipmem"); got != pkg {
		t.Fatalf("dir %q want %q", got, pkg)
	}
}

func TestResolvingAnEntryPointDoesNotImportIt(t *testing.T) {
	ctx, _ := discHome(t)
	root := discEntryPoint(t, "sideeffect", "sideeffect_pkg")
	pkg := filepath.Join(root, "sideeffect_pkg")
	discWrite(t, filepath.Join(pkg, "__init__.py"), "import pathlib\npathlib.Path(__file__).with_name(\"IMPORTED\").write_text(\"x\")\n")
	if got := FindProviderDir(ctx, nil, "sideeffect"); got != pkg {
		t.Fatalf("dir %q want %q", got, pkg)
	}
	ListMemoryProviderNames(ctx, nil)
	if _, err := os.Stat(filepath.Join(pkg, "IMPORTED")); err == nil {
		t.Fatal("resolving the entry point ran its module")
	}
}

func TestBareModuleEntryPointHasNoDirectory(t *testing.T) {
	ctx, _ := discHome(t)
	root := discEntryPoint(t, "flatmem", "flatmem")
	discWrite(t, filepath.Join(root, "flatmem.py"), strings.Replace(discoveryProviderSource, "%s", "flatmem", 1))
	if got := FindProviderDir(ctx, nil, "flatmem"); got != "" {
		t.Fatalf("bare module resolved to %q", got)
	}
	if r := FindProvider(ctx, nil, "flatmem"); r == nil || r.Source != SourceEntryPoint {
		t.Fatalf("bare module entry point not registered: %+v", r)
	}
}

func TestProviderRegisteredPastTheFirst8KBIsDiscovered(t *testing.T) {
	ctx, h := discHome(t)
	provider := filepath.Join(h, "plugins", "longdoc")
	doc := `"""` + strings.Repeat("Plain prose about storage.\n", 400) + "\"\"\"\n"
	discWrite(t, filepath.Join(provider, "__init__.py"), doc+strings.Replace(discoveryProviderSource, "%s", "longdoc", 1))
	if !slices.Contains(ListMemoryProviderNames(ctx, nil), "longdoc") {
		t.Fatal("provider past 8 KB not listed")
	}
	if got := FindProviderDir(ctx, nil, "longdoc"); got != provider {
		t.Fatalf("dir %q want %q", got, provider)
	}
}

func TestUnreadableUserPluginDoesNotAbortMemoryDiscovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission bits do not deny root, run the suite as a regular user")
	}
	ctx, h := discHome(t)
	root := filepath.Join(h, "plugins")
	discProvider(t, root, "goodmem")
	denied := filepath.Join(root, "denied")
	discWrite(t, filepath.Join(denied, "__init__.py"), "")
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(denied, 0o755) })
	names := ListMemoryProviderNames(ctx, nil)
	if !slices.Contains(names, "goodmem") || slices.Contains(names, "denied") || !slices.Contains(names, "byterover") {
		t.Fatalf("names %v", names)
	}
	if FindProviderDir(ctx, nil, "denied") != "" {
		t.Fatal("unreadable directory resolved")
	}
}

func TestDefinesMemoryProviderParsesInsteadOfMatchingText(t *testing.T) {
	dir := t.TempDir()
	discWrite(t, filepath.Join(dir, "__init__.py"), "\"\"\"Mentions MemoryProvider and register_memory_provider in prose only.\"\"\"\n")
	discWrite(t, filepath.Join(dir, "broken.py"), "def (:\n")
	if definesMemoryProvider(dir) {
		t.Fatal("docstring counted as a definition")
	}
	discWrite(t, filepath.Join(dir, "impl.py"), "from agent.memory_provider import MemoryProvider as Base\n\nclass Store(Base):\n    pass\n")
	if !definesMemoryProvider(dir) {
		t.Fatal("aliased subclass not found")
	}
	other := t.TempDir()
	discWrite(t, filepath.Join(other, "__init__.py"), "\xef\xbb\xbfdef register(ctx):\n    ctx.register_memory_provider(object())\n")
	if !definesMemoryProvider(other) {
		t.Fatal("register call behind a BOM not found")
	}
}

func TestDisabledUserProviderIsFoundButNeverLoaded(t *testing.T) {
	ctx, _ := discHome(t)
	disabled := t.TempDir()
	dir := discProvider(t, filepath.Join(disabled, "plugins"), "fakemem")
	discWrite(t, filepath.Join(dir, "plugin.yaml"), "name: fakemem-manifest\nkind: exclusive\n")
	discWrite(t, filepath.Join(disabled, "config.yaml"), "memory:\n  provider: fakemem\nplugins:\n  disabled: [fakemem-manifest]\n")
	t.Setenv("ICEBATH_HOME", disabled)
	if got := FindProviderDir(ctx, nil, "fakemem"); got != dir {
		t.Fatalf("dir %q want %q", got, dir)
	}
	if p := LoadMemoryProvider(ctx, nil, "fakemem"); p != nil {
		t.Fatal("parked provider loaded")
	}
}

// Exported provider probe agrees with the internal one on an empty directory
func TestDefinesMemoryProviderExported(t *testing.T) {
	dir := t.TempDir()
	if DefinesMemoryProvider(dir) != definesMemoryProvider(dir) || DefinesMemoryProvider(dir) {
		t.Fatal("empty dir defines a memory provider")
	}
}
