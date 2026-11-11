package plugins_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Empty home with a fresh discovered plugin manager, the real builtin task keys and captured plugin logs
func auxInheritHome(t *testing.T) (context.Context, *plugins.Manager, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	for _, dir := range []string{h, filepath.Join(root, "bundled")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", root)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", filepath.Join(root, "bundled"))
	previousServices := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{BuiltinAuxTaskKeys: provider.BuiltinAuxTaskKeys})
	plugins.ResetPluginManagersForTests()
	var logs bytes.Buffer
	previousLogger := plugins.Logger
	plugins.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	t.Cleanup(func() {
		plugins.Logger = previousLogger
		plugins.ResetPluginManagersForTests()
		plugins.InstallServices(previousServices)
	})
	ctx := home.WithOverride(context.Background(), h)
	manager := plugins.GetPluginManager(ctx)
	manager.DiscoverAndLoad(ctx, false)
	return ctx, manager, &logs
}

func registerPlugAux(manager *plugins.Manager, key any, inheritFrom any) error {
	_, err := plugins.NewContext(&plugins.Manifest{Name: "plug"}, manager).RegisterAuxiliaryTask(key,
		plugins.AuxTaskOptions{DisplayName: "Plug Aux", Description: "d", InheritFrom: inheritFrom})
	return err
}

func TestBadInheritFromWarnsAndRegistersWithoutInheritance(t *testing.T) {
	for _, bad := range []string{"no_such_task", "plug_aux", ""} {
		t.Run(bad, func(t *testing.T) {
			ctx, manager, logs := auxInheritHome(t)

			if err := registerPlugAux(manager, "plug_aux", bad); err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(logs.String(), "ignoring inherit_from") {
				t.Fatalf("no warning: %s", logs)
			}
			entry := manager.AuxiliaryTasks()["plug_aux"]
			if value, present := entry["inherit_from"]; !present || value != nil {
				t.Fatalf("entry %v", entry)
			}
			resolved, err := provider.AuxiliaryTaskConfig(ctx, "plug_aux")
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(resolved["timeout"]) != "60" {
				t.Fatalf("resolved %v", resolved)
			}
		})
	}
}

func TestAuxiliaryTaskRegistrarIgnoresNonStringInheritFromAndRefusesNonStringKey(t *testing.T) {
	ctx, manager, logs := auxInheritHome(t)
	if err := registerPlugAux(manager, "5", nil); err != nil {
		t.Fatal(err)
	}
	if err := registerPlugAux(manager, "plug_aux", int64(5)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "auxiliary task 'plug_aux': ignoring inherit_from=5 ") {
		t.Fatalf("warning %s", logs)
	}
	if entry := manager.AuxiliaryTasks()["plug_aux"]; entry["inherit_from"] != nil {
		t.Fatalf("entry %v", entry)
	}
	if resolved, err := provider.AuxiliaryTaskConfig(ctx, "plug_aux"); err != nil || fmt.Sprint(resolved["timeout"]) != "60" {
		t.Fatalf("resolved %v err %v", resolved, err)
	}
	for raw, want := range map[string]any{"5": int64(5), "None": nil, "True": true} {
		err := registerPlugAux(manager, want, nil)
		if err == nil || err.Error() != "Plugin 'plug' tried to register auxiliary task with invalid key "+raw {
			t.Fatalf("key %s err %v", raw, err)
		}
	}
	if err := registerPlugAux(manager, []any{"plug_aux"}, nil); err == nil || err.Error() != "unhashable type: 'list'" {
		t.Fatalf("list key err %v", err)
	}
	if err := registerPlugAux(manager, "plug_ok", "5"); err != nil {
		t.Fatal(err)
	}
	if entry := manager.AuxiliaryTasks()["plug_ok"]; entry["inherit_from"] != "5" || len(entry["defaults"].(map[string]any)) != 0 {
		t.Fatalf("entry %v", entry)
	}
}

func TestAuxiliaryTaskRegistrarKeepsPluginValuesAndRefusesNonMappingDefaults(t *testing.T) {
	_, manager, _ := auxInheritHome(t)
	register := func(key string, opts plugins.AuxTaskOptions) error {
		_, err := plugins.NewContext(&plugins.Manifest{Name: "plug"}, manager).RegisterAuxiliaryTask(key, opts)
		return err
	}
	if err := register("plug_raw", plugins.AuxTaskOptions{DisplayName: nil, Description: int64(7), Defaults: map[string]any{"timeout": int64(9), "extra": "kept"}}); err != nil {
		t.Fatal(err)
	}
	entry := manager.AuxiliaryTasks()["plug_raw"]
	defaults := entry["defaults"].(map[string]any)
	if entry["display_name"] != nil || entry["description"] != int64(7) || defaults["timeout"] != int64(9) || defaults["extra"] != "kept" || defaults["provider"] != "auto" || len(defaults) != 7 {
		t.Fatalf("entry %v", entry)
	}
	for _, falsy := range []any{nil, false, int64(0), "", []any{}, map[string]any{}} {
		if err := register("plug_falsy", plugins.AuxTaskOptions{DisplayName: "n", Description: "d", Defaults: falsy}); err != nil {
			t.Fatalf("defaults %v: %v", falsy, err)
		}
		if got := manager.AuxiliaryTasks()["plug_falsy"]["defaults"].(map[string]any); len(got) != 6 || fmt.Sprint(got["timeout"]) != "60" {
			t.Fatalf("defaults %v gave %v", falsy, got)
		}
	}
	pairs := []any{[]any{"timeout", int64(5)}, "ab"}
	if err := register("plug_pairs", plugins.AuxTaskOptions{DisplayName: "n", Description: "d", Defaults: pairs}); err == nil || err.Error() != "'list' object is not a mapping" {
		t.Fatalf("pairs without inheritance %v", err)
	}
	if err := register("plug_pairs", plugins.AuxTaskOptions{DisplayName: "n", Description: "d", Defaults: pairs, InheritFrom: "plug_raw"}); err != nil {
		t.Fatal(err)
	}
	if got := manager.AuxiliaryTasks()["plug_pairs"]["defaults"].(map[string]any); len(got) != 2 || got["timeout"] != int64(5) || got["a"] != "b" {
		t.Fatalf("pairs %v", got)
	}
	for want, bad := range map[string]any{
		"dictionary update sequence element #0 has length 1; 2 is required":  "x",
		"'int' object is not iterable":                                       int64(3),
		"dictionary update sequence element #1 has length 3; 2 is required":  []any{[]any{"a", int64(1)}, []any{"b", int64(2), int64(3)}},
		"cannot convert dictionary update sequence element #0 to a sequence": []any{int64(1)},
		"unhashable type: 'list'":                                            []any{[]any{[]any{}, int64(1)}},
	} {
		if err := register("plug_bad", plugins.AuxTaskOptions{DisplayName: "n", Description: "d", Defaults: bad, InheritFrom: "plug_raw"}); err == nil || err.Error() != want {
			t.Fatalf("defaults %v: %v", bad, err)
		}
	}
	if _, registered := manager.AuxiliaryTasks()["plug_bad"]; registered {
		t.Fatal("refused task registered")
	}
}
