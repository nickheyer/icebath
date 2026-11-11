package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupListsTheRequirementsPmWouldInstall(t *testing.T) {
	for _, manifestLists := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[manifestLists], func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			dir := filepath.Join(h, "plugins", "provider")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := map[string]any{"name": "provider"}
			if manifestLists {
				manifest["pip_dependencies"] = []string{"stale-manifest-dep"}
			}
			raw, _ := json.Marshal(manifest)
			if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte("def register(ctx):\n    ctx.register_memory_provider(None)\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			pyproject := "[project]\nname = \"provider\"\nversion = \"1\"\ndependencies = [\"provider_dep>=1,<2\", \"icebath-agent\"]\n"
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(pyproject), 0o644); err != nil {
				t.Fatal(err)
			}
			setup := memoryProviderSetupInfo(context.Background(), "provider")
			deps, _ := setup.GetOr("pip_dependencies", nil).([]any)
			if len(deps) != 1 || deps[0] != "provider_dep>=1,<2" {
				t.Fatalf("setup %v", setup)
			}
		})
	}
}

func TestCoerceBoolRejectsUnrecognizedText(t *testing.T) {
	for _, c := range []struct {
		in   any
		want bool
	}{{true, true}, {nil, true}, {"", true}, {int64(0), false}, {1.5, true}, {" Yes ", true}, {"off", false}} {
		got, err := coerceBool(c.in, true)
		if err != nil || got != c.want {
			t.Fatalf("%v: %v %v", c.in, got, err)
		}
	}
	if _, err := coerceBool("maybe", false); err == nil || err.Error() != "Invalid boolean value: maybe" {
		t.Fatalf("err %v", err)
	}
}
