package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent with the attribute surface the hermes _make_agent namespace carries, gates off unless the case sets them
func spAgent(t *testing.T, names ...string) *AIAgent {
	t.Helper()
	empty := ""
	valid := map[string]bool{}
	for _, n := range names {
		valid[n] = true
	}
	a := &AIAgent{env: &tools.Env{Values: &sync.Map{}}, validToolNames: valid, kanbanWorkerGuidance: &empty,
		parallelToolCallGuidance: true, botModeProtocol: true, executionGuidance: "auto", platformHintOverrides: map[string]any{}}
	a.toolUseEnforcement = false
	return a
}

// Stubs the soul, environment and context file builders, the context builder recording the cwd it was handed
func capturedContextCwd(t *testing.T, a *AIAgent) *string {
	t.Helper()
	emptyPromptProbes(t)
	var captured *string
	called := false
	buildContextFilesPromptFn = func(_ context.Context, _ *tools.Env, o ContextFilesOptions) (string, error) {
		called, captured = true, o.Cwd
		return "", nil
	}
	if _, err := BuildSystemPromptParts(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("context files builder never ran")
	}
	return captured
}

func promptParts(t *testing.T, a *AIAgent, system *string) SystemPromptParts {
	t.Helper()
	parts, err := BuildSystemPromptParts(context.Background(), a, system)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func TestKanbanGuidanceRequiresWorkerTaskAtAgentInit(t *testing.T) {
	for _, c := range []struct {
		taskID string
		want   bool
	}{{"", false}, {"t_worker", true}} {
		t.Run(c.taskID, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			t.Setenv("ICEBATH_KANBAN_TASK", c.taskID)
			if c.taskID == "" {
				os.Unsetenv("ICEBATH_KANBAN_TASK")
			}
			prev := loadToolDefinitions
			loadToolDefinitions = func(context.Context, *tools.Env, tools.DefinitionOptions) []map[string]any {
				return []map[string]any{{"function": map[string]any{"name": "kanban_show"}}}
			}
			t.Cleanup(func() { loadToolDefinitions = prev })
			a := &AIAgent{quietMode: true, env: &tools.Env{Values: &sync.Map{}}}
			loadTools(context.Background(), a, []string{"kanban"}, nil)
			if got := a.kanbanWorkerGuidance != nil && *a.kanbanWorkerGuidance == KanbanGuidance; got != c.want {
				t.Fatalf("guidance %v want %v", got, c.want)
			}
		})
	}
}

func TestKanbanGuidanceFallbackRequiresOwnedWorkerTask(t *testing.T) {
	for _, c := range []struct {
		taskID      string
		owner, want bool
	}{{"", true, false}, {"t_worker", true, true}, {"t_worker", false, false}} {
		t.Run(fmt.Sprintf("%s_%v", c.taskID, c.owner), func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			t.Setenv("ICEBATH_KANBAN_TASK", c.taskID)
			if c.taskID == "" {
				os.Unsetenv("ICEBATH_KANBAN_TASK")
			}
			a := spAgent(t, "kanban_show")
			a.kanbanWorkerGuidance = nil
			if !c.owner {
				t.Cleanup(tools.EnterNonDispatcherOwnedContext(a.env))
			}
			if got := toolGuidanceBlock(context.Background(), a) == KanbanGuidance; got != c.want {
				t.Fatalf("guidance %v want %v", got, c.want)
			}
		})
	}
}

func TestMemoryGuidanceRespectsAvailableWrites(t *testing.T) {
	for _, stores := range [][2]bool{{true, true}, {false, true}, {true, false}, {false, false}} {
		for _, names := range [][]string{{}, {"memory"}, {"memory", "skill_view", "skills_list"}, {"memory", "skill_view", "skills_list", "skill_manage"}} {
			t.Run(fmt.Sprintf("%v_%v", stores, names), func(t *testing.T) {
				t.Setenv("ICEBATH_HOME", t.TempDir())
				t.Chdir(t.TempDir())
				a := spAgent(t, names...)
				a.skipContextFiles = true
				a.memoryEnabled, a.userProfileEnabled = stores[0], stores[1]
				prompt, err := BuildSystemPrompt(context.Background(), a, nil)
				if err != nil {
					t.Fatal(err)
				}
				enabled := a.validToolNames["memory"] && (stores[0] || stores[1])
				if strings.Contains(prompt, "Memory is the narrow exception") != enabled {
					t.Fatalf("memory guidance present %v want %v", !enabled, enabled)
				}
				if strings.Contains(prompt, "(skill_manage)") != (enabled && a.validToolNames["skill_manage"]) {
					t.Fatal("skill_manage pointer mismatch")
				}
				if enabled && !stores[0] && !strings.Contains(prompt, "never target='memory'") {
					t.Fatal("memory target warning missing")
				}
			})
		}
	}
}

func TestContextFileCwd(t *testing.T) {
	t.Run("test_none_when_terminal_cwd_unset", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		t.Setenv("TERMINAL_CWD", "")
		os.Unsetenv("TERMINAL_CWD")
		if got := capturedContextCwd(t, spAgent(t)); got != nil {
			t.Fatalf("cwd %q", *got)
		}
	})
	t.Run("test_configured_dir_when_terminal_cwd_set", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir := t.TempDir()
		t.Setenv("TERMINAL_CWD", dir)
		if got := capturedContextCwd(t, spAgent(t)); got == nil || *got != dir {
			t.Fatalf("cwd %v", got)
		}
	})
	t.Run("test_desktop_launch_artifact_does_not_load_bundled_agents_md", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		t.Setenv("TERMINAL_CWD", "")
		os.Unsetenv("TERMINAL_CWD")
		withPackageRoot(t, dir)
		t.Chdir(dir)
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("bundled contributor instructions"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := spAgent(t)
		a.platform, a.contextCwdIsLaunchArtifact = "desktop", true
		soulAndHintsOnly(t)
		if ctx := promptParts(t, a, nil).Context; strings.Contains(ctx, "bundled contributor instructions") {
			t.Fatalf("context %q", ctx)
		}
	})
	t.Run("test_desktop_launch_artifact_uses_profile_configured_cwd", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		root, _ := filepath.EvalSymlinks(t.TempDir())
		launch, workspace := filepath.Join(root, "launch"), filepath.Join(root, "workspace")
		for _, d := range []string{launch, workspace} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		withPackageRoot(t, launch)
		t.Chdir(launch)
		t.Setenv("TERMINAL_CWD", workspace)
		_ = os.WriteFile(filepath.Join(launch, "AGENTS.md"), []byte("bundled contributor instructions"), 0o600)
		_ = os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("operator workspace rules"), 0o600)
		a := spAgent(t)
		a.platform, a.contextCwdIsLaunchArtifact = "desktop", true
		tools.SetLogicalSessionCwd(a.env, launch)
		soulAndHintsOnly(t)
		ctx := promptParts(t, a, nil).Context
		if !strings.Contains(ctx, "operator workspace rules") || strings.Contains(ctx, "bundled contributor instructions") {
			t.Fatalf("context %q", ctx)
		}
	})
	t.Run("test_desktop_explicit_install_tree_workspace_still_loads_agents_md", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		withPackageRoot(t, dir)
		t.Chdir(dir)
		t.Setenv("TERMINAL_CWD", dir)
		_ = os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("chosen workspace instructions"), 0o600)
		a := spAgent(t)
		a.platform, a.contextCwdIsLaunchArtifact = "desktop", false
		soulAndHintsOnly(t)
		if ctx := promptParts(t, a, nil).Context; !strings.Contains(ctx, "chosen workspace instructions") {
			t.Fatalf("context %q", ctx)
		}
	})
}

// Stubs only the soul and environment builders, the context files built for real
func soulAndHintsOnly(t *testing.T) {
	t.Helper()
	prevSoul, prevHints := loadSoulMDFn, buildEnvironmentHintsFn
	t.Cleanup(func() { loadSoulMDFn, buildEnvironmentHintsFn = prevSoul, prevHints })
	loadSoulMDFn = func(context.Context, *tools.Env, *int, string) (string, bool) { return "", true }
	buildEnvironmentHintsFn = func(context.Context, *tools.Env) (string, error) { return "", nil }
}

// Points the install tree root the context discovery guards at the directory
func withPackageRoot(t *testing.T, dir string) {
	t.Helper()
	prev := tools.PackageRoot
	tools.PackageRoot = dir
	t.Cleanup(func() { tools.PackageRoot = prev })
}

// A git repo holding a source file, what the coding posture needs to engage
func initCodeRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodingContextBlock(t *testing.T) {
	t.Run("test_injected_when_active", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir := t.TempDir()
		initCodeRepo(t, dir)
		t.Setenv("TERMINAL_CWD", dir)
		a := spAgent(t, "read_file")
		a.platform = "cli"
		emptyPromptProbes(t)
		parts := promptParts(t, a, nil)
		if !strings.Contains(parts.Stable, "coding agent") || !strings.Contains(parts.Context, "Workspace") {
			t.Fatalf("stable %q context %q", parts.Stable, parts.Context)
		}
	})
	t.Run("test_absent_when_off", func(t *testing.T) {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		dir := t.TempDir()
		initCodeRepo(t, dir)
		t.Setenv("TERMINAL_CWD", dir)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  coding_context: \"off\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := spAgent(t, "read_file")
		a.platform = "cli"
		if strings.Contains(stablePrompt(t, a), "coding agent") {
			t.Fatal("coding posture with the mode off")
		}
	})
	t.Run("test_absent_without_tools", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir := t.TempDir()
		initCodeRepo(t, dir)
		t.Setenv("TERMINAL_CWD", dir)
		a := spAgent(t)
		a.platform = "cli"
		if strings.Contains(stablePrompt(t, a), "coding agent") {
			t.Fatal("coding posture without tools")
		}
	})
}

type promptBlockMemory struct {
	memory.BaseProvider
	block string
}

func (p *promptBlockMemory) Name() string                                                 { return "prompt-block" }
func (p *promptBlockMemory) Available(context.Context) bool                               { return true }
func (p *promptBlockMemory) Initialize(context.Context, string, memory.InitOptions) error { return nil }
func (p *promptBlockMemory) SystemPromptBlock(context.Context) (string, error)            { return p.block, nil }
func (p *promptBlockMemory) ToolSchemas(context.Context) ([]map[string]any, error)        { return nil, nil }

const (
	skillsSentinel  = "SKILLS_INDEX_SENTINEL"
	contextSentinel = "CONTEXT_FILES_SENTINEL"
)

// Builds with a skills index and context files present, the hermes _build helper
func skillsContextPrompt(t *testing.T, a *AIAgent) (SystemPromptParts, string) {
	t.Helper()
	emptyPromptProbes(t)
	buildContextFilesPromptFn = func(context.Context, *tools.Env, ContextFilesOptions) (string, error) { return contextSentinel, nil }
	prevSkills := buildSkillsSystemPromptFn
	buildSkillsSystemPromptFn = func(*skills.Scope, context.Context, skills.SkillsPromptOptions) (string, error) {
		return skillsSentinel, nil
	}
	t.Cleanup(func() { buildSkillsSystemPromptFn = prevSkills })
	parts := promptParts(t, a, nil)
	full, err := BuildSystemPrompt(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return parts, full
}

func TestSkillsInVolatileBand(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_skills_not_in_stable_band", func(t *testing.T) {
		if parts, _ := skillsContextPrompt(t, spAgent(t, "skills_list")); strings.Contains(parts.Stable, skillsSentinel) {
			t.Fatal("skills in the stable band")
		}
	})
	t.Run("test_skills_lead_the_volatile_band", func(t *testing.T) {
		if parts, _ := skillsContextPrompt(t, spAgent(t, "skills_list")); !strings.HasPrefix(parts.Volatile, skillsSentinel) {
			t.Fatalf("volatile %q", parts.Volatile)
		}
	})
	t.Run("test_full_order_is_stable_context_then_skills", func(t *testing.T) {
		_, full := skillsContextPrompt(t, spAgent(t, "skills_list"))
		if !(strings.Index(full, contextSentinel) < strings.Index(full, skillsSentinel) && strings.Index(full, skillsSentinel) < strings.Index(full, "Conversation started:")) {
			t.Fatalf("order %q", full)
		}
	})
}

func TestMemoryProviderSystemPromptGating(t *testing.T) {
	const block = "PROVIDER_BLOCK_SENTINEL"
	build := func(t *testing.T, enabled, disabled []string) string {
		t.Helper()
		t.Setenv("ICEBATH_HOME", t.TempDir())
		a := spAgent(t, "skills_list")
		a.enabledToolsets, a.disabledToolsets = enabled, disabled
		mm := memory.NewMemoryManager(context.Background(), a.env, nil)
		if err := mm.AddProvider(context.Background(), &promptBlockMemory{block: block}); err != nil {
			t.Fatal(err)
		}
		a.memoryManager = mm
		_, full := skillsContextPrompt(t, a)
		return full
	}
	t.Run("test_block_injected_when_memory_toolset_enabled", func(t *testing.T) {
		if !strings.Contains(build(t, []string{"memory"}, nil), block) {
			t.Fatal("provider block missing")
		}
	})
	t.Run("test_block_dropped_when_memory_toolset_disabled", func(t *testing.T) {
		if strings.Contains(build(t, nil, []string{"memory"}), block) {
			t.Fatal("provider block kept")
		}
	})
	t.Run("test_block_dropped_when_memory_not_in_enabled_toolsets", func(t *testing.T) {
		if strings.Contains(build(t, []string{"web_search"}, nil), block) {
			t.Fatal("provider block kept")
		}
	})
}

func executionGuidancePrompt(t *testing.T, model string, guidance any, enforcement any, names ...string) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := spAgent(t, names...)
	a.model, a.executionGuidance, a.toolUseEnforcement = model, guidance, enforcement
	return stablePrompt(t, a)
}

func TestExecutionGuidanceInjection(t *testing.T) {
	tools2 := []string{"terminal", "read_file"}
	t.Run("test_deepseek_gets_guidance_by_default", func(t *testing.T) {
		s := executionGuidancePrompt(t, "deepseek/deepseek-v4-pro", "auto", false, tools2...)
		if !strings.Contains(s, "Execution discipline") || !strings.Contains(s, "<external_state_verification>") {
			t.Fatal("guidance missing")
		}
	})
	t.Run("test_independent_of_tool_use_enforcement", func(t *testing.T) {
		s := executionGuidancePrompt(t, "deepseek/deepseek-v4-flash", "auto", false, tools2...)
		if !strings.Contains(s, "Execution discipline") || strings.Contains(s, "Tool-use enforcement") {
			t.Fatal("gate coupled to enforcement")
		}
	})
	t.Run("test_claude_does_not_get_guidance_by_default", func(t *testing.T) {
		if strings.Contains(executionGuidancePrompt(t, "anthropic/claude-opus-4.8", "auto", false, tools2...), "Execution discipline") {
			t.Fatal("claude got guidance")
		}
	})
	t.Run("test_config_false_suppresses", func(t *testing.T) {
		if strings.Contains(executionGuidancePrompt(t, "openai/gpt-5.5", false, false, tools2...), "Execution discipline") ||
			strings.Contains(executionGuidancePrompt(t, "deepseek/deepseek-v4-pro", "off", false, tools2...), "Execution discipline") {
			t.Fatal("false did not suppress")
		}
	})
	t.Run("test_config_true_forces_for_any_model", func(t *testing.T) {
		if !strings.Contains(executionGuidancePrompt(t, "anthropic/claude-opus-4.8", true, false, tools2...), "Execution discipline") {
			t.Fatal("true did not force")
		}
	})
	t.Run("test_config_list_matches_substring", func(t *testing.T) {
		if !strings.Contains(executionGuidancePrompt(t, "mycorp/custom-llm-7b", []any{"custom-llm", "gpt"}, false, tools2...), "Execution discipline") {
			t.Fatal("list did not match")
		}
	})
	t.Run("test_config_list_non_match_suppresses", func(t *testing.T) {
		if strings.Contains(executionGuidancePrompt(t, "openai/gpt-5.5", []any{"deepseek"}, false, tools2...), "Execution discipline") {
			t.Fatal("non match injected")
		}
	})
	t.Run("test_no_tools_no_guidance", func(t *testing.T) {
		if strings.Contains(executionGuidancePrompt(t, "deepseek/deepseek-v4-pro", "auto", false), "Execution discipline") {
			t.Fatal("guidance without tools")
		}
	})
}

func TestBuildSystemPromptRecordsStablePrefix(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := spAgent(t)
	emptyPromptProbes(t)
	buildContextFilesPromptFn = func(context.Context, *tools.Env, ContextFilesOptions) (string, error) { return "context", nil }
	prompt, err := BuildSystemPrompt(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, a.cachedSystemPromptStatic) || !strings.HasPrefix(prompt[len(a.cachedSystemPromptStatic):], "\n\ncontext") {
		t.Fatalf("prompt %q static %q", prompt, a.cachedSystemPromptStatic)
	}
}

func TestTelegramRichMessagesHint(t *testing.T) {
	telegram := func(t *testing.T, cfg string) string {
		t.Helper()
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		a := spAgent(t)
		a.platform = "telegram"
		return stablePrompt(t, a)
	}
	rich := func(s string) bool { return strings.Contains(s, "lean into it") }
	t.Run("test_base_hint_without_rich_messages", func(t *testing.T) {
		s := telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        rich_messages: false\n")
		if !strings.Contains(s, "Standard Markdown auto-converts") || rich(s) || strings.Contains(s, "task lists") {
			t.Fatalf("stable %q", s)
		}
	})
	t.Run("test_rich_hint_with_rich_messages_enabled", func(t *testing.T) {
		s := telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        rich_messages: true\n")
		if !rich(s) || !strings.Contains(s, "task lists") || !strings.Contains(s, "math/formulas") {
			t.Fatalf("stable %q", s)
		}
	})
	t.Run("test_rich_hint_from_top_level_platforms", func(t *testing.T) {
		s := telegram(t, "platforms:\n  telegram:\n    extra:\n      rich_messages: true\n")
		if !rich(s) || !strings.Contains(s, "task lists") {
			t.Fatalf("stable %q", s)
		}
	})
	t.Run("test_top_level_overrides_gateway_rich_messages", func(t *testing.T) {
		if !rich(telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        rich_messages: false\nplatforms:\n  telegram:\n    extra:\n      rich_messages: true\n")) {
			t.Fatal("top level did not win")
		}
	})
	t.Run("test_gateway_extra_other_keys_does_not_block_top_level_rich_messages", func(t *testing.T) {
		if !rich(telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        disable_link_previews: true\nplatforms:\n  telegram:\n    extra:\n      rich_messages: true\n")) {
			t.Fatal("other gateway keys blocked the top level")
		}
	})
	t.Run("test_gateway_rich_messages_integration_via_real_config", func(t *testing.T) {
		s := telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra:\n        rich_messages: true\n")
		if !rich(s) || !strings.Contains(s, "task lists") {
			t.Fatalf("stable %q", s)
		}
	})
	t.Run("test_malformed_extra_value_falls_back_to_base_hint", func(t *testing.T) {
		s := telegram(t, "gateway:\n  platforms:\n    telegram:\n      extra: not-a-map\n")
		if !strings.Contains(s, "Standard Markdown auto-converts") || rich(s) {
			t.Fatalf("stable %q", s)
		}
	})
}

var _ = time.Now
