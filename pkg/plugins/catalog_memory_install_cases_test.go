package plugins_test

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	memoryCatalogHook     = "def register(ctx):\n    ctx.register_hook('pre_llm_call', print)\n"
	memoryCatalogProvider = "def register(ctx):\n    ctx.register_memory_provider(object())\n"
)

// Catalog world of cat-plugin whose install answers the memory provider contract through the real detector
func memoryCatalogWorld(t *testing.T) *plugins.LoginWorld {
	t.Helper()
	world := plugins.NewLoginWorld(t, "cat-plugin", "def register(ctx):\n    pass\n")
	next := *plugins.InstalledServices()
	next.DefinesMemoryProvider = memory.DefinesMemoryProvider
	next.MemoryProviderDir = memory.ProviderDirForHome
	next.PrepareMemoryProviderDependencies = func(ctx context.Context, name string) error {
		h := home.Dir(ctx)
		cfg, _ := config.LoadReadonly(ctx, h, nil)
		return memory.PrepareMemoryProviderDependenciesForPlugin(ctx, &tools.Env{Home: h, Config: cfg, Cwd: h, Log: slog.Default()}, name)
	}
	plugins.InstallServices(&next)
	return world
}

func TestMemoryCategoryInstallActivatesViaMemoryProviderNotPluginsEnable(t *testing.T) {
	for _, tc := range []struct {
		label, init string
		enable      bool
	}{
		{"provider-enable", memoryCatalogProvider, true},
		{"provider-decline", memoryCatalogProvider, false},
		{"hook", memoryCatalogHook, true},
		{"hook-docstring", "\"\"\"Recall hook. Not a MemoryProvider: it only injects context.\"\"\"\n" + memoryCatalogHook, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			world := memoryCatalogWorld(t)
			world.SetCategory("memory")
			world.RepinTo(t, tc.init, "memory category plugin")
			var printed bytes.Buffer
			enable := tc.enable
			if tc.init != memoryCatalogProvider {
				world.SaveMemoryProvider(t, "honcho")
				if err := plugins.CmdInstall(world.Ctx(), plugins.InstallCommandOptions{Identifier: "cat-plugin", Enable: &enable}, &printed); err != nil {
					t.Fatal(err)
				}
				if provider, enabled := world.MemoryProvider(), world.EnabledSet(); provider != "honcho" || !maps.Equal(enabled, map[string]bool{"cat-plugin": true}) {
					t.Fatalf("hook plugin left provider %q enabled %v", provider, enabled)
				}
				return
			}
			if err := plugins.CmdInstall(world.Ctx(), plugins.InstallCommandOptions{Identifier: "cat-plugin", Enable: &enable}, &printed); err != nil {
				t.Fatal(err)
			}
			want := map[bool]string{true: "cat-plugin", false: ""}[tc.enable]
			if provider := world.MemoryProvider(); provider != want {
				t.Fatalf("memory provider %q, want %q", provider, want)
			}
			if enabled := world.EnabledSet(); len(enabled) != 0 {
				t.Fatalf("plugins enabled %v", enabled)
			}
			text := printed.String()
			if !strings.Contains(text, "icebath memory setup") || strings.Contains(text, "plugins enable cat-plugin") {
				t.Fatalf("install output %q", text)
			}
		})
	}
}

func TestUrlInstallOfAMemoryProviderDirGetsTheProviderHint(t *testing.T) {
	for _, prior := range []string{"", "disabled", "enabled"} {
		t.Run(map[string]string{"": "None", "disabled": "disabled", "enabled": "enabled"}[prior], func(t *testing.T) {
			world := memoryCatalogWorld(t)
			repo := filepath.Join(world.Root(), "mem-repo")
			casesExternalWrite(t, filepath.Join(repo, "plugin.yaml"), "name: mem-plugin\nversion: 1.0.0\ndescription: d\n")
			casesExternalWrite(t, filepath.Join(repo, "__init__.py"), "from agent.memory_provider import MemoryProvider\n\n\nclass Mem(MemoryProvider):\n    pass\n")
			world.InitRepo(t, repo, "v1")
			uri := "file://" + filepath.ToSlash(repo)
			var printed bytes.Buffer
			if prior == "enabled" {
				disable := false
				if err := plugins.CmdInstall(world.Ctx(), plugins.InstallCommandOptions{Identifier: uri, Enable: &disable}, &printed); err != nil {
					t.Fatal(err)
				}
			}
			if prior != "" {
				world.SetPluginsList(t, prior, "mem-plugin")
			}
			enable := true
			if err := plugins.CmdInstall(world.Ctx(), plugins.InstallCommandOptions{Identifier: uri, Enable: &enable, Force: prior == "enabled"}, &printed); err != nil {
				t.Fatal(err)
			}
			if provider := world.MemoryProvider(); provider != "mem-plugin" {
				t.Fatalf("memory provider %q", provider)
			}
			want := map[bool]map[string]bool{true: {"mem-plugin": true}, false: {}}[prior == "enabled"]
			if enabled := world.EnabledSet(); !maps.Equal(enabled, want) {
				t.Fatalf("plugins enabled %v, want %v", enabled, want)
			}
			if world.DisabledSet()["mem-plugin"] {
				t.Fatal("the provider stays in plugins.disabled")
			}
			if !strings.Contains(printed.String(), "memory.provider") {
				t.Fatalf("install output %q", printed.String())
			}
		})
	}
}
