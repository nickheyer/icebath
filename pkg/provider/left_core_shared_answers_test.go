package provider

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestMigrateAllLeftCoreHomesSharesDependencyAnswersAcrossAGroup(t *testing.T) {
	root := t.TempDir()
	homes := []string{leftCoreHome(t, root, "one", "HASS_TOKEN=abc\n", ""), leftCoreHome(t, root, "two", "HASS_TOKEN=abc\n", "")}
	var prompts []string
	restore := plugins.SwapTerminalIO(func() bool { return true }, func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "y", nil
	})
	defer restore()
	var installedIn []string
	withPluginHooks(t, func(h *PluginManagerHooks) {
		h.DependencyHomes = func() []string { return homes }
		h.Install = func(ctx context.Context, name string, _ bool) (map[string]any, error) {
			if ok, reason := plugins.ConsentPythonDeps(ctx, name, []string{"hass-client"}, plugins.NewConsole(io.Discard), false); !ok {
				return map[string]any{"ok": false, "error": reason}, nil
			}
			mkPluginDir(t, home.Dir(ctx), name)
			installedIn = append(installedIn, home.Dir(ctx))
			return map[string]any{"ok": true}, nil
		}
	})
	got := MigrateAllLeftCoreHomes(context.Background(), func(string) {})
	if !slices.Equal(got, []string{"homeassistant", "homeassistant"}) || !slices.Equal(installedIn, homes) {
		t.Fatalf("installed %v in %v", got, installedIn)
	}
	if len(prompts) != 1 {
		t.Fatalf("dependency question asked %d times across one group: %v", len(prompts), prompts)
	}
}
