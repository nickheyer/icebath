package config

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

const ladderLegacySoul = "# Beta identity\n\n## Messaging other agents\nKEEP THIS PROFILE SECTION\n"

// Every file under root keyed by its relative path, a directory mapping to nil
func homeFiles(t *testing.T, root string) map[string]*string {
	t.Helper()
	out := map[string]*string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			out[rel] = nil
			return nil
		}
		data, err := os.ReadFile(path)
		text := string(data)
		out[rel] = &text
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameFiles(a, b map[string]*string) bool {
	return maps.EqualFunc(a, b, func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) })
}

// The ladder from the support floor in config only mode outputs the document alone, a file mode run changes the home
func TestConfigOnlyMigrationLadderTouchesNoFile(t *testing.T) {
	for _, configOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "config_only", false: "file_mode"}[configOnly], func(t *testing.T) {
			h := filepath.Join(t.TempDir(), "home")
			if err := os.MkdirAll(filepath.Join(h, "profiles", "beta"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			for _, name := range []string{BackendEnv, "LLM_MODEL", "OPENAI_MODEL", "TERMINAL_VERCEL_RUNTIME"} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			os.WriteFile(filepath.Join(h, ".env"), []byte("LLM_MODEL=old-model\nTERMINAL_VERCEL_RUNTIME="+LegacyVercelRuntime+"\n"), 0o644)
			os.WriteFile(filepath.Join(h, "SOUL.md"), []byte(ladderLegacySoul), 0o644)
			os.WriteFile(filepath.Join(h, "profiles", "beta", "SOUL.md"), []byte(ladderLegacySoul), 0o644)
			doc := map[string]any{"_config_version": SupportFloorVersion, "model": map[string]any{"default": "m"}}
			var persisted []map[string]any
			src := migrationTestSources(h, nil)
			src.ReadRaw = func(string) (map[string]any, error) {
				if len(persisted) > 0 {
					return maps.Clone(persisted[len(persisted)-1]), nil
				}
				return maps.Clone(doc), nil
			}
			src.Persist = func(_ string, cfg map[string]any) error {
				persisted = append(persisted, maps.Clone(cfg))
				return nil
			}
			src.BotRoster = func(string) []BotProfile {
				return []BotProfile{{Name: "default", Dir: h}, {Name: "beta", Dir: filepath.Join(h, "profiles", "beta")}}
			}
			before := homeFiles(t, h)
			res := &MigrationResults{EnvAdded: []string{}, ConfigAdded: []string{}, Warnings: []string{}}
			RunMigrations(h, SupportFloorVersion, res, true, MigrationOptions{ConfigOnly: configOnly}, src)
			if len(persisted) == 0 {
				t.Fatal("the document was not migrated")
			}
			if same := sameFiles(homeFiles(t, h), before); same != configOnly {
				t.Fatalf("files unchanged %v in config only %v", same, configOnly)
			}
		})
	}
}

// A config read on the bootstrapping call itself returns at once instead of waiting on the bootstrap
func TestSameThreadReentryDuringTheBootstrapDoesNotDeadlock(t *testing.T) {
	t.Setenv(BackendEnv, "")
	os.Unsetenv(BackendEnv)
	bootstrapped.Store(false)
	var seen []string
	RegisterConfigBootstrapEnv(func(ctx context.Context, _ func(string) []string, _, _ string) error {
		backend, err := GetConfigBackend(ctx)
		if err != nil {
			return err
		}
		seen = append(seen, backend.Name())
		return nil
	})
	t.Cleanup(func() { RegisterConfigBootstrapEnv(nil) })
	backend, err := GetConfigBackend(context.Background())
	if err != nil || backend.Name() != "file" || len(seen) != 1 || seen[0] != "file" || !bootstrapped.Load() {
		t.Fatalf("backend %v %v seen %v", backend, err, seen)
	}
}
