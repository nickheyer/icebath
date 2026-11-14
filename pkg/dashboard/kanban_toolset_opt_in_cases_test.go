package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Kanban tool names of the quiet raw schema build for a selection, a nil selection naming every toolset
func optInKanbanNames(ctx context.Context, env *tools.Env, selection, disabled []string) map[string]bool {
	out := map[string]bool{}
	opts := tools.DefinitionOptions{Enabled: selection, Disabled: disabled, Quiet: true, SkipToolSearchAssembly: true}
	for _, row := range tools.Default.ToolDefinitions(ctx, env, opts) {
		fn, _ := row["function"].(map[string]any)
		if name, _ := fn["name"].(string); strings.HasPrefix(name, "kanban_") {
			out[name] = true
		}
	}
	return out
}

// Fresh profile home under a temp home with the kanban worker identity cleared
func optInHome(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_BOARD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return root, h
}

// Config of a profile home the way load_config reads it
func optInLoad(t *testing.T, h string) config.Tree {
	t.Helper()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Writes a config tree into a profile home through save_config
func optInSave(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	if err := config.SaveConfig(context.Background(), h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Sorted toolset keys saved for a platform of a profile home
func optInSelected(t *testing.T, h, platform string, includeDefaultMCP bool) []string {
	t.Helper()
	set := tools.Default.PlatformTools(context.Background(), "", optInLoad(t, h), platform, tools.PlatformToolsOptions{IncludeDefaultMCPServers: includeDefaultMCP})
	out := make([]string, 0, len(set))
	for name, on := range set {
		if on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func optInHas(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

func optInSameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestSavedOptInRoundtripReachesSchemaAndBoard(t *testing.T) {
	for _, surface := range []string{"cli", "http", "rpc"} {
		t.Run(surface, func(t *testing.T) {
			root, h := optInHome(t)
			ctx := context.Background()
			env := &tools.Env{Home: h}
			optInSave(t, h, map[string]any{"platform_toolsets": map[string]any{"cli": []any{"file"}, "telegram": []any{"file"}}})

			if before := optInKanbanNames(ctx, env, optInSelected(t, h, "cli", false), nil); len(before) != 0 {
				t.Fatalf("kanban tools before the opt in %v", before)
			}
			var client *testClient
			if surface == "http" {
				app := NewFastAPI("tools", "0", nil, nil)
				app.IncludeRouter(toolsRouter(), "")
				client = newTestClient(t, app)
			}
			toggle := func(enabled bool) {
				action := "disable"
				if enabled {
					action = "enable"
				}
				switch surface {
				case "http":
					resp := appJSON(t, client, "PUT", "/api/tools/toolsets/kanban", pyjson.ObjectOf("enabled", enabled))
					if resp.Status != 200 {
						t.Fatalf("status %d %s", resp.Status, resp.Body)
					}
					if got, _ := appObject(t, resp).Get("enabled"); got != enabled {
						t.Fatalf("enabled %v want %v", got, enabled)
					}
				case "rpc":
					resp := tuiserver.Method("tools.configure")(ctx, "kanban-opt-in", map[string]any{"action": action, "names": []any{"kanban"}})
					if _, failed := resp["error"]; failed {
						t.Fatal(resp)
					}
					result, _ := resp["result"].(map[string]any)
					changed, _ := result["changed"].([]string)
					if !optInHas(changed, "kanban") {
						t.Fatal(resp)
					}
				default:
					if err := mcp.ApplyToolsetChange(ctx, optInLoad(t, h), "cli", []string{"kanban"}, action); err != nil {
						t.Fatal(err)
					}
				}
			}

			toggle(true)
			enabledNames := optInKanbanNames(ctx, env, optInSelected(t, h, "cli", false), nil)
			for _, name := range []string{"kanban_list", "kanban_create", "kanban_complete"} {
				if !enabledNames[name] {
					t.Fatalf("%s missing from %v", name, enabledNames)
				}
			}
			if leaked := optInKanbanNames(ctx, env, optInSelected(t, h, "telegram", false), nil); len(leaked) != 0 {
				t.Fatalf("CLI opt-in leaked to Telegram %v", leaked)
			}
			if !optInHas(optInSelected(t, h, "cli", false), "file") {
				t.Fatal("file left the cli selection")
			}

			other := filepath.Join(root, "profiles", "observer")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			optInSave(t, other, map[string]any{"platform_toolsets": map[string]any{"cli": []any{"file"}}})
			if borrowed := optInKanbanNames(ctx, &tools.Env{Home: other}, optInSelected(t, other, "cli", false), nil); len(borrowed) != 0 {
				t.Fatalf("a second profile borrowed the grant %v", borrowed)
			}
			if again := optInKanbanNames(ctx, env, optInSelected(t, h, "cli", false), nil); !optInSameSet(again, enabledNames) {
				t.Fatalf("schema after the other profile %v want %v", again, enabledNames)
			}

			if !skills.ScopeFromEnv(nil).DetectKanban() {
				t.Fatal("Saved opt-in still hides the Kanban playbook")
			}
			out := tools.NewDispatcher(tools.Default).RegistryDispatch(ctx, &tools.Call{Name: "kanban_create",
				Args: map[string]any{"title": "opt-in roundtrip", "assignee": "default"}, RawArgs: `{"title": "opt-in roundtrip", "assignee": "default"}`})
			result, err := pyjson.Loads(out)
			if err != nil {
				t.Fatal(out)
			}
			obj, _ := result.(*pyjson.Object)
			if ok, _ := obj.Get("ok"); ok != true {
				t.Fatal(out)
			}
			taskID, _ := obj.Get("task_id")
			err = kanban.ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
				task, err := kanban.GetTask(ctx, conn, taskID.(string))
				if err != nil {
					return err
				}
				if task == nil || task.Title != "opt-in roundtrip" {
					t.Fatalf("task %+v", task)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			toggle(false)
			if left := optInKanbanNames(ctx, env, optInSelected(t, h, "cli", false), nil); len(left) != 0 {
				t.Fatalf("kanban tools after the opt out %v", left)
			}
			if skills.ScopeFromEnv(nil).DetectKanban() {
				t.Fatal("the playbook stays after the opt out")
			}
			if !optInHas(optInSelected(t, h, "cli", false), "file") {
				t.Fatal("file left the cli selection")
			}
			if len(enabledNames) == 0 {
				t.Fatal("Changing config must not mutate an already-built schema")
			}
		})
	}
}

func TestSelectionIsScopedAndPreservesWorkerAndDenyBoundaries(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[legacy], func(t *testing.T) {
			_, h := optInHome(t)
			ctx := context.Background()
			env := &tools.Env{Home: h}
			toolsets := []any{}
			if legacy {
				toolsets = []any{"kanban"}
			}
			optInSave(t, h, map[string]any{"toolsets": toolsets, "platform_toolsets": map[string]any{"telegram": []any{"kanban"}}})

			var named, broad map[string]bool
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); named = optInKanbanNames(ctx, env, []string{"kanban"}, nil) }()
			go func() { defer wg.Done(); broad = optInKanbanNames(ctx, env, []string{"icebath-cli"}, nil) }()
			wg.Wait()
			if !named["kanban_create"] {
				t.Fatalf("named selection %v", named)
			}
			if (len(broad) > 0) != legacy {
				t.Fatalf("bundle selection %v legacy %v", broad, legacy)
			}
			if (len(optInKanbanNames(ctx, env, nil, nil)) > 0) != legacy {
				t.Fatal("default selection")
			}
			if (len(optInKanbanNames(ctx, env, []string{"all"}, nil)) > 0) != legacy {
				t.Fatal("all selection")
			}
			if got := optInKanbanNames(ctx, env, []string{"kanban"}, []string{"kanban"}); len(got) != 0 {
				t.Fatalf("deny list lost %v", got)
			}
			if got := optInKanbanNames(ctx, env, []string{}, nil); len(got) != 0 {
				t.Fatalf("empty selection %v", got)
			}
			if (len(optInKanbanNames(ctx, env, optInSelected(t, h, "cli", true), nil)) > 0) != legacy {
				t.Fatal("cli platform selection")
			}
			cfg := optInLoad(t, h)
			platforms, _ := cfg["platform_toolsets"].(map[string]any)
			platforms["cli"] = []any{"file"}
			optInSave(t, h, cfg)
			if got := optInKanbanNames(ctx, env, optInSelected(t, h, "cli", true), nil); len(got) != 0 {
				t.Fatalf("a saved selection loses to the legacy key %v", got)
			}

			t.Setenv("ICEBATH_KANBAN_TASK", "t_worker")
			worker := optInKanbanNames(ctx, env, []string{"file"}, nil)
			if !worker["kanban_complete"] || worker["kanban_list"] {
				t.Fatalf("worker selection %v", worker)
			}
			if got := optInKanbanNames(tools.WithDelegatedChildContext(ctx, true), env, []string{"kanban"}, nil); len(got) != 0 {
				t.Fatalf("delegate child selection %v", got)
			}
			if !optInKanbanNames(ctx, env, []string{"file"}, nil)["kanban_complete"] {
				t.Fatal("worker selection after the delegate child")
			}
		})
	}
}
