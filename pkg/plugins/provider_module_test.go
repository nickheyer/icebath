package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCallProviderModuleRunsAUserProviderSubmoduleInTheHost(t *testing.T) {
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if host := GetPluginManager(ctx).pluginHostInstance; host != nil {
			host.Shutdown()
		}
	})
	good := filepath.Join(h, "plugins", "mem")
	broken := filepath.Join(h, "plugins", "brokenmem")
	for _, dir := range []string{good, broken} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(good, "__init__.py"), "VALUE = '-x'\n")
	writeFile(t, filepath.Join(good, "cli.py"), "from . import VALUE\n\n"+
		"class ConfigWriteRefused(Exception):\n    pass\n\n"+
		"def clone(name, suffix=''):\n    return {'name': name + VALUE + suffix, 'pkg': __package__}\n\n"+
		"def refuse(name):\n    raise ConfigWriteRefused('no write for ' + name)\n")
	writeFile(t, filepath.Join(broken, "__init__.py"), "raise RuntimeError('import boom')\n")
	module := UserMemoryNamespace + ".mem__source_0123456789abcdef"
	out, loaded, err := CallProviderModule(ctx, good, module, "cli", "clone", []any{"a"}, map[string]any{"suffix": "!"})
	if err != nil || !loaded {
		t.Fatalf("clone %v %v", loaded, err)
	}
	reply, _ := out.(map[string]any)
	if reply["name"] != "a-x!" || reply["pkg"] != module {
		t.Fatalf("reply %v", out)
	}
	_, loaded, err = CallProviderModule(ctx, good, module, "cli", "refuse", []any{"a"}, nil)
	var he *PluginHostError
	if !loaded || !errors.As(err, &he) || he.TypeName != "ConfigWriteRefused" || he.RemoteMessage != "no write for a" {
		t.Fatalf("refuse %v %v", loaded, err)
	}
	_, loaded, err = CallProviderModule(ctx, broken, UserMemoryNamespace+".brokenmem__source_0", "cli", "clone", nil, nil)
	if loaded || err != nil {
		t.Fatalf("broken package %v %v", loaded, err)
	}
}
