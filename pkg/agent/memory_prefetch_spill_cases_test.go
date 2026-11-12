package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMemoryManager(t *testing.T) {
	caseHome(t)
	t.Run("test_prefetch_spill_requires_opt_in_without_changing_hooks", func(t *testing.T) {
		for _, tc := range []struct {
			memory       map[string]any
			hooksEnabled bool
		}{{map[string]any{}, true}, {map[string]any{"prefetch_spill_enabled": false}, true}, {map[string]any{"prefetch_spill_enabled": true}, false}} {
			t.Run("", func(t *testing.T) {
				h := t.TempDir()
				t.Setenv("ICEBATH_HOME", h)
				env := &tools.Env{Home: h}
				ctx := tools.WithToolEnv(context.Background(), env)
				spill := map[string]any{}
				if !tc.hooksEnabled {
					spill["enabled"] = false
				}
				write := func(data map[string]any) {
					if err := config.AtomicConfigWrite(ctx, h, filepath.Join(h, "config.yaml"), data, ""); err != nil {
						t.Fatal(err)
					}
				}
				write(map[string]any{"memory": tc.memory, "hooks": map[string]any{"output_spill": spill}})
				text := "start\n" + strings.Repeat("ranked middle\n", tools.SpillDefaultMaxChars/2) + "end\n"
				p := &prologueMemory{prefetch: text}
				mgr := memory.NewMemoryManager(ctx, env, nil)
				if err := mgr.AddProvider(ctx, p); err != nil {
					t.Fatal(err)
				}
				if got := mgr.PrefetchAll(ctx, "recall", "memory"); got != text {
					t.Fatalf("prefetch %d chars", len(got))
				}
				if _, err := os.Stat(filepath.Join(h, "hook_outputs")); err == nil {
					t.Fatal("prefetch spilled")
				}
				patchPluginHooks(t, func(string, map[string]any) []any { return []any{map[string]any{"context": text}} })
				result := collectPreLLMCallContext(ctx, &AIAgent{sessionID: "hook", model: "test", env: env}, "task", "turn", "recall", nil, []map[string]any{}, nil)
				if tc.hooksEnabled {
					saved, _ := filepath.Glob(filepath.Join(h, "hook_outputs", "hook", "*.txt"))
					if len(saved) != 1 {
						t.Fatalf("saved %v", saved)
					}
					raw, _ := os.ReadFile(saved[0])
					if string(raw) != text || !strings.Contains(result, saved[0]) || !strings.Contains(result, "plugin hook output truncated") {
						t.Fatalf("result %q", result[:min(len(result), 300)])
					}
				} else {
					if result != text {
						t.Fatalf("result %d chars", len(result))
					}
					if _, err := os.Stat(filepath.Join(h, "hook_outputs")); err == nil {
						t.Fatal("hook output spilled with spill disabled")
					}
				}
				write(map[string]any{"memory": map[string]any{"prefetch_spill_enabled": true}, "hooks": map[string]any{"output_spill": map[string]any{"enabled": true}}})
				if got := mgr.PrefetchAll(ctx, "recall again", "memory"); got != text {
					t.Fatalf("second prefetch %d chars", len(got))
				}
				if _, err := os.Stat(filepath.Join(h, "hook_outputs", "memory")); err == nil {
					t.Fatal("prefetch spilled after opt in")
				}
			})
		}
	})
}
