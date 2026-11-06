package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestASecondaryRegistrationCannotCostTheProvider(t *testing.T) {
	ctx, h := discHome(t)
	dir := discProvider(t, filepath.Join(h, "plugins"), "auxmem")
	src := strings.Replace(discoveryProviderSource, "%s", "auxmem", 1)
	src = strings.Replace(src, "    ctx.register_memory_provider(Provider())\n", "    instance = Provider()\n    instance.marked = True\n"+
		"    ctx.register_memory_provider(instance)\n    ctx.register_auxiliary_task(\n        'auxmem_filter', display_name='Aux', description='d'\n    )\n", 1)
	discWrite(t, filepath.Join(dir, "__init__.py"), src)
	p := LoadMemoryProvider(ctx, nil, "auxmem")
	if p == nil || p.Name() != "auxmem" {
		t.Fatalf("provider %v", p)
	}
	marked, err := p.(*hostedProvider).obj.Get(ctx, "marked")
	if err != nil || !pyrepr.Truthy(marked) {
		t.Fatalf("registered instance replaced: %v %v", marked, err)
	}
}

func TestActivationIsNotGatedOnPluginsEnabled(t *testing.T) {
	ctx, h := discHome(t)
	discProvider(t, filepath.Join(h, "plugins"), "gatedmem")
	if LoadMemoryProvider(ctx, nil, "gatedmem") == nil {
		t.Fatal("provider gated on plugins.enabled")
	}
}

func TestDisabledUserProviderLoadsOnlyInTheEnabledProfile(t *testing.T) {
	ctx, _ := discHome(t)
	enabled, disabled := t.TempDir(), t.TempDir()
	for _, h := range []string{enabled, disabled} {
		dir := discProvider(t, filepath.Join(h, "plugins"), "fakemem")
		discWrite(t, filepath.Join(dir, "plugin.yaml"), "name: fakemem-manifest\nkind: exclusive\n")
	}
	discWrite(t, filepath.Join(enabled, "config.yaml"), "memory:\n  provider: fakemem\nplugins:\n  disabled: []\n")
	discWrite(t, filepath.Join(disabled, "config.yaml"), "memory:\n  provider: fakemem\nplugins:\n  disabled: [fakemem-manifest]\n")
	for i, h := range []string{enabled, disabled, enabled} {
		t.Setenv("ICEBATH_HOME", h)
		if p := LoadMemoryProvider(ctx, nil, "fakemem"); (p != nil) != (h == enabled) {
			t.Fatalf("step %d home %s loaded %v", i, h, p)
		}
	}
}

// Writes a user provider whose package and cli.py both import a sibling module relatively, active in the home
func hostCLIProvider(t *testing.T, name string) (context.Context, string) {
	ctx, h := discHome(t)
	dir := filepath.Join(h, "plugins", name)
	discWrite(t, filepath.Join(dir, "__init__.py"), "from agent.memory_provider import MemoryProvider\nfrom . import config\n"+
		"class MyProvider(MemoryProvider):\n    @property\n    def name(self): return "+pyrepr.Repr(name)+"\n"+
		"    def is_available(self): return True\n    def initialize(self, **kw): pass\n    def sync_turn(self, *a, **kw): pass\n"+
		"    def get_tool_schemas(self): return []\n    def handle_tool_call(self, *a, **kw): return '{}'\n"+
		"def register(ctx):\n    ctx.register_memory_provider(MyProvider())\n")
	discWrite(t, filepath.Join(dir, "config.py"), "STATUS = 'ok'\n")
	discWrite(t, filepath.Join(dir, "cli.py"), "from . import config\ndef register_cli(subparser):\n    subparser.add_argument('--status', action='store_true')\n"+
		"def "+name+"_command(args):\n    import pathlib\n    pathlib.Path(__file__).with_name('ran.txt').write_text(config.STATUS + ' ' + str(args.status))\n    print('status printed')\n    return 4\n")
	discWrite(t, filepath.Join(h, "config.yaml"), "memory:\n  provider: "+name+"\n")
	return ctx, dir
}

func TestCLIDiscoveredForUserPluginWithRelativeImport(t *testing.T) {
	ctx, dir := hostCLIProvider(t, "extcli")
	cmds := DiscoverPluginCLICommands(ctx, nil)
	if len(cmds) != 1 || cmds[0].Name != "extcli" || cmds[0].Setup == nil || cmds[0].Handler == nil {
		t.Fatalf("commands %+v", cmds)
	}
	root := &cobra.Command{Use: "icebath"}
	cmd := &cobra.Command{Use: "extcli", Short: cmds[0].Help}
	root.AddCommand(cmd)
	cmds[0].Setup(cmd)
	cmd.RunE = cmds[0].Handler
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"extcli", "--status"})
	root.SetContext(ctx)
	var exit *PluginCLIExit
	if err := root.Execute(); !errors.As(err, &exit) || exit.Code != 4 {
		t.Fatalf("run error %v", err)
	}
	ran, err := os.ReadFile(filepath.Join(dir, "ran.txt"))
	if err != nil || string(ran) != "ok True" || !strings.Contains(out.String(), "status printed") {
		t.Fatalf("handler wrote %q %v, printed %q", ran, err, out.String())
	}
}

func TestProviderLoadAfterCLIDiscovery(t *testing.T) {
	ctx, _ := hostCLIProvider(t, "extcliload")
	if n := len(DiscoverPluginCLICommands(ctx, nil)); n != 1 {
		t.Fatalf("%d commands", n)
	}
	p := LoadMemoryProvider(ctx, nil, "extcliload")
	if p == nil || p.Name() != "extcliload" {
		t.Fatalf("provider %v", p)
	}
}

func TestUserProviderSchemaFileRunsInThePluginHost(t *testing.T) {
	isolateRegistry(t)
	ctx, h := discHome(t)
	dir := discProvider(t, filepath.Join(h, "plugins"), "memprobe")
	marker := filepath.Join(t.TempDir(), "schema_pid")
	discWrite(t, filepath.Join(dir, "config_schema.py"), "import os, pathlib\npathlib.Path("+pyrepr.Repr(marker)+").write_text(str(os.getpid()))\n"+
		"CONFIG_SCHEMA = {'name': 'memprobe', 'label': 'Probe', 'fields': [{'key': 'api_key', 'label': 'Key', 'kind': 'secret'}]}\n")
	s := GetProviderConfigSchema(ctx, nil, "memprobe")
	if s == nil || s.Label != "Probe" || len(s.Fields) != 1 || s.Fields[0].Kind != "secret" {
		t.Fatalf("schema %+v", s)
	}
	pid, err := os.ReadFile(marker)
	if err != nil || string(pid) == "" || string(pid) == strconv.Itoa(os.Getpid()) {
		t.Fatalf("schema ran in pid %q (%v)", pid, err)
	}
}
