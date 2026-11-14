package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
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

// Replaces the soul, environment and context file builders, the context builder recording the cwd it was handed
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

// Replaces only the soul and environment builders, the context files built for real
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
		t.Setenv("ICEBATH_HOME", t.TempDir())
		dir := t.TempDir()
		initCodeRepo(t, dir)
		t.Setenv("TERMINAL_CWD", dir)
		a := spAgent(t, "read_file")
		a.platform = "cli"
		a.env.Config = config.Tree{"agent": map[string]any{"coding_context": "off"}}
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

var _ = time.Now

// Session store answering only the lineage root walk the conversation start reads
type rootOnlyStore struct {
	SessionStore
	root func(sessionID string) (string, error)
}

func (s rootOnlyStore) GetConversationRoot(sessionID string) (string, error) {
	return s.root(sessionID)
}

func TestSessionStartLike(t *testing.T) {
	utc := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }
	day := func(v time.Time) string { return v.Format("2006-01-02") }
	t.Run("test_uses_session_id_embedded_timestamp", func(t *testing.T) {
		a := &AIAgent{sessionID: "20260101_120000_abc123", sessionStart: time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)}
		start := sessionStartLike(a, utc(2026, 1, 2, 9))
		if day(start) != "2026-01-01" || start.Location() == nil {
			t.Fatalf("start %v", start)
		}
	})
	t.Run("test_prefers_lineage_root_over_rotated_segment_id", func(t *testing.T) {
		a := &AIAgent{sessionID: "20260615_090000_seg9", sessionStart: time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local),
			sessionDB: rootOnlyStore{root: func(sid string) (string, error) {
				if sid != "20260615_090000_seg9" {
					t.Fatalf("walked %q", sid)
				}
				return "20260101_120000_root", nil
			}}}
		if got := day(sessionStartLike(a, utc(2026, 6, 16, 9))); got != "2026-01-01" {
			t.Fatalf("start %s", got)
		}
	})
	t.Run("test_lineage_walk_failure_falls_open_to_segment_id", func(t *testing.T) {
		a := &AIAgent{sessionID: "20260615_090000_seg9", sessionStart: time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local),
			sessionDB: rootOnlyStore{root: func(string) (string, error) { return "", fmt.Errorf("db locked") }}}
		if got := day(sessionStartLike(a, utc(2026, 6, 16, 9))); got != "2026-06-15" {
			t.Fatalf("start %s", got)
		}
	})
	t.Run("test_nontimestamp_root_falls_through_to_segment_id", func(t *testing.T) {
		a := &AIAgent{sessionID: "20260615_090000_seg9", sessionStart: time.Date(2026, 6, 15, 9, 0, 0, 0, time.Local),
			sessionDB: rootOnlyStore{root: func(string) (string, error) { return "imported-legacy-root", nil }}}
		if got := day(sessionStartLike(a, utc(2026, 6, 16, 9))); got != "2026-06-15" {
			t.Fatalf("start %s", got)
		}
	})
	t.Run("test_falls_back_to_session_start", func(t *testing.T) {
		a := &AIAgent{sessionStart: time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)}
		if got := day(sessionStartLike(a, utc(2026, 1, 2, 9))); got != "2026-01-01" {
			t.Fatalf("start %s", got)
		}
	})
	t.Run("test_falls_back_to_now_when_no_start_known", func(t *testing.T) {
		now := utc(2026, 1, 2, 9)
		if got := sessionStartLike(&AIAgent{}, now); !got.Equal(now) {
			t.Fatalf("start %v", got)
		}
	})
	t.Run("test_nonmatching_session_id_uses_session_start", func(t *testing.T) {
		a := &AIAgent{sessionID: "plugin-section-test", sessionStart: time.Date(2026, 1, 1, 7, 30, 0, 0, time.Local)}
		if got := day(sessionStartLike(a, utc(2026, 1, 2, 9))); got != "2026-01-01" {
			t.Fatalf("start %s", got)
		}
	})
	t.Run("test_stamp_from_other_dst_half_keeps_its_own_offset", func(t *testing.T) {
		london, err := time.LoadLocation("Europe/London")
		if err != nil {
			t.Fatal(err)
		}
		prev := time.Local
		time.Local = london
		t.Cleanup(func() { time.Local = prev })
		for _, c := range []struct {
			sid  string
			now  time.Time
			want string
		}{
			{"20260115_003000_jan", time.Date(2026, 7, 16, 9, 0, 0, 0, london), "2026-01-15T00:30:00+00:00"},
			{"20260715_003000_jul", time.Date(2026, 12, 16, 9, 0, 0, 0, london), "2026-07-15T00:30:00+01:00"},
		} {
			if got := sessionStartLike(&AIAgent{sessionID: c.sid}, c.now).Format("2006-01-02T15:04:05-07:00"); got != c.want {
				t.Fatalf("%s -> %s", c.sid, got)
			}
		}
	})
}

// Replaces the coding posture parts with fixed blocks for the rest of the test
func fixedCodingParts(t *testing.T, prefix, workspace, trailing []string) {
	t.Helper()
	prev := codingSystemPromptPartsFn
	codingSystemPromptPartsFn = func(*tools.Env, string, string, string, map[string]bool, *string) ([]string, []string, []string) {
		return prefix, workspace, trailing
	}
	t.Cleanup(func() { codingSystemPromptPartsFn = prev })
}

func defaultProfileLine(h string) string {
	return "Active Icebath profile: default. Other profiles (if any) live under " + h + "/profiles/<name>/. Each profile has its own skills/, " +
		"plugins/, cron/, and memories/ that affect a different session than this one. Do not modify another profile's skills/plugins/cron/memories " +
		"unless the user explicitly directs you to."
}

func TestCodingPromptOrdersSharedContextBeforeWorkspace(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	a := spAgent(t, "read_file")
	a.parallelToolCallGuidance = false
	emptyPromptProbes(t)
	buildContextFilesPromptFn = func(context.Context, *tools.Env, ContextFilesOptions) (string, error) { return "CONTEXT_FILES", nil }
	fixedCodingParts(t, []string{"CODING_STABLE"}, []string{"WORKSPACE"}, []string{"Operator instructions (from config):\nOPERATOR"})
	fixedPromptClock(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	prevTZ := hermesTimezone
	hermesTimezone = func(context.Context) (*time.Location, bool) { return nil, false }
	t.Cleanup(func() { hermesTimezone = prevTZ })
	prevFmt := safeStrftime
	safeStrftime = func(v time.Time, format string) string {
		if format == "%Z" || format == "%z" {
			return ""
		}
		return prevFmt(v, format)
	}
	t.Cleanup(func() { safeStrftime = prevFmt })
	system := "SYSTEM_MESSAGE"
	prompt, err := BuildSystemPrompt(context.Background(), a, &system)
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{config.DefaultIdentity, IcebathAgentHelpGuidanceNoSkills, SteerChannelNote, "CODING_STABLE", "SYSTEM_MESSAGE",
		"CONTEXT_FILES", "WORKSPACE", "Operator instructions (from config):\nOPERATOR", defaultProfileLine(h), "Conversation started: Friday, January 02, 2026"}, "\n\n")
	if prompt != expected {
		t.Fatalf("prompt\n%q\nwant\n%q", prompt, expected)
	}
	if want := strings.Join([]string{config.DefaultIdentity, IcebathAgentHelpGuidanceNoSkills, SteerChannelNote, "CODING_STABLE"}, "\n\n"); a.cachedSystemPromptStatic != want {
		t.Fatalf("static %q", a.cachedSystemPromptStatic)
	}
}

func TestConversationStartUsesSessionStartNotBuildTime(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := spAgent(t, "read_file")
	a.parallelToolCallGuidance, a.sessionID = false, "20260101_120000_abc123"
	emptyPromptProbes(t)
	buildContextFilesPromptFn = func(context.Context, *tools.Env, ContextFilesOptions) (string, error) { return "CONTEXT_FILES", nil }
	fixedCodingParts(t, nil, nil, nil)
	fixedPromptClock(t, time.Date(2026, 1, 2, 9, 0, 0, 0, time.Local))
	system := "SYSTEM_MESSAGE"
	prompt, err := BuildSystemPrompt(context.Background(), a, &system)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Conversation started: Thursday, January 01, 2026") || strings.Contains(prompt, "Conversation started: Friday") {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestConversationStartedTwoLine(t *testing.T) {
	volatile := func(t *testing.T, sessionID string, timeless bool) string {
		t.Helper()
		t.Setenv("ICEBATH_HOME", t.TempDir())
		a := spAgent(t)
		a.sessionID, a.botChatTimelessPrompt = sessionID, timeless
		emptyPromptProbes(t)
		return promptParts(t, a, nil).Volatile
	}
	t.Run("test_old_session_gets_rebuild_date_line", func(t *testing.T) {
		v := volatile(t, "20200110_090000_old", false)
		if !strings.Contains(v, "Conversation started:") || !strings.Contains(v, "as of the last context rebuild") {
			t.Fatalf("volatile %q", v)
		}
	})
	t.Run("test_same_day_session_keeps_single_line", func(t *testing.T) {
		sid := config.TimeNow(context.Background()).Format("20060102_150405") + "_fresh"
		v := volatile(t, sid, false)
		if !strings.Contains(v, "Conversation started:") || strings.Contains(v, "as of the last context rebuild") {
			t.Fatalf("volatile %q", v)
		}
	})
	t.Run("test_surrogate_zone_name_does_not_abort_prompt", func(t *testing.T) {
		zone := time.FixedZone("Paris, Madrid (heure d'\xe9t\xe9)", 2*3600)
		prev := hermesNow
		hermesNow = func(context.Context) time.Time { return time.Date(2026, 7, 14, 13, 5, 0, 0, zone) }
		t.Cleanup(func() { hermesNow = prev })
		prevTZ := hermesTimezone
		hermesTimezone = func(context.Context) (*time.Location, bool) { return nil, false }
		t.Cleanup(func() { hermesTimezone = prevTZ })
		v := volatile(t, "20260714_090000_fresh", false)
		if !utf8.ValidString(v) {
			t.Fatalf("volatile carries invalid text %q", v)
		}
		if !strings.Contains(v, "Conversation started: Tuesday, July 14, 2026") || !strings.Contains(v, "Paris, Madrid (heure d'") || !strings.Contains(v, "UTC+02:00") {
			t.Fatalf("volatile %q", v)
		}
	})
	t.Run("test_timeless_bot_chat_unaffected", func(t *testing.T) {
		v := volatile(t, "20200110_090000_old", true)
		if strings.Contains(v, "Conversation started:") || strings.Contains(v, "as of the last context rebuild") {
			t.Fatalf("volatile %q", v)
		}
	})
}

func fullPrompt(p SystemPromptParts) string {
	return p.Stable + "\n\n" + p.Context + "\n\n" + p.Volatile
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

func TestSharedProjectContextPrecedesWorktreeBytes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TERMINAL_ENV", "local")
	root := t.TempDir()
	var prompts []string
	for _, name := range []string{"worktree-a", "worktree-b"} {
		cwd := filepath.Join(root, name)
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("Shared project instructions."), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TERMINAL_CWD", cwd)
		a := spAgent(t)
		a.platform = "cli"
		parts := promptParts(t, a, nil)
		full := fullPrompt(parts)
		if strings.Index(full, "Shared project instructions.") > strings.Index(full, "Current working directory:") || strings.Index(full, "Current working directory:") < 0 {
			t.Fatalf("order %q", full)
		}
		if strings.Contains(parts.Stable, cwd) {
			t.Fatalf("cwd in the stable tier %q", parts.Stable)
		}
		if again := fullPrompt(promptParts(t, a, nil)); again != full {
			t.Fatal("rebuild differs")
		}
		prompts = append(prompts, full)
	}
	if !strings.Contains(commonPrefix(prompts[0], prompts[1]), "Shared project instructions.") {
		t.Fatal("shared context not in the common prefix")
	}
}

func TestStoredPromptCwdIgnoresProjectHostDecoys(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	root := t.TempDir()
	cwd := filepath.Join(root, "worktree")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_ENV", "local")
	t.Setenv("TERMINAL_CWD", cwd)
	decoy := "# Hermes runtime environment\n\nHost: Example\nUser home directory: /example\nCurrent working directory: /example\n"
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(decoy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_ENVIRONMENT_HINT", decoy+"\nModel: decoy\nProvider: decoy\nPlatform: decoy")
	memDir := filepath.Join(h, "memories")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(decoy), 0o600); err != nil {
		t.Fatal(err)
	}
	a := spAgent(t)
	a.platform, a.model, a.provider = "cli", "test-model", "test-provider"
	a.memoryEnabled, a.userProfileEnabled = true, false
	a.memoryStore = memory.NewStore(memDir, 2200, 1375, true, false)
	if err := a.memoryStore.LoadFromDisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	full := fullPrompt(promptParts(t, a, nil))
	matches := func(prompt string) bool {
		ok, err := storedPromptMatchesRuntime(a, prompt)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !matches(full) {
		t.Fatal("own prompt does not match")
	}
	t.Setenv("TERMINAL_CWD", root)
	if matches(full) {
		t.Fatal("another cwd matched")
	}
	legacy := "Host: Example\nUser home directory: " + root + "\nCurrent working directory: " + cwd + "\n\n# Project Context\n\n" + decoy + "\nModel: test-model\nProvider: test-provider\nPlatform: cli"
	if matches(legacy) {
		t.Fatal("legacy prompt matched another cwd")
	}
	t.Setenv("TERMINAL_CWD", cwd)
	if !matches(legacy) {
		t.Fatal("legacy prompt lost its anchor")
	}
}

func TestStoredPromptStampedForAnotherSessionIsNotRestored(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TERMINAL_ENV", "local")
	t.Setenv("TERMINAL_CWD", t.TempDir())
	agentFor := func(sid string) *AIAgent {
		a := spAgent(t)
		a.platform, a.model, a.provider, a.passSessionID, a.sessionID = "cli", "test-model", "test-provider", true, sid
		return a
	}
	parentPrompt, err := BuildSystemPrompt(context.Background(), agentFor("parent-sid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := storedPromptMatchesRuntime(agentFor("parent-sid"), parentPrompt); !ok {
		t.Fatal("parent prompt does not match its session")
	}
	if ok, _ := storedPromptMatchesRuntime(agentFor("child-sid"), parentPrompt); ok {
		t.Fatal("child session restored the parent prompt")
	}
}

// Prompt of a read_file agent with the coding posture off under the real profile resolution chain
func profilePrompt(t *testing.T) SystemPromptParts {
	t.Helper()
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	a := spAgent(t, "read_file")
	a.env.Config = config.Tree{"agent": map[string]any{"coding_context": "off"}}
	emptyPromptProbes(t)
	return promptParts(t, a, nil)
}

func TestNamedProfileHintIntegration(t *testing.T) {
	t.Run("test_real_hermes_home_under_profiles_renders_correct_paths", func(t *testing.T) {
		userHome := t.TempDir()
		root := filepath.Join(userHome, ".icebath")
		profileHome := filepath.Join(root, "profiles", "coder")
		if err := os.MkdirAll(profileHome, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", userHome)
		t.Setenv("ICEBATH_HOME", profileHome)
		home.ResetKeyCache()
		prompt := fullPrompt(profilePrompt(t))
		for _, want := range []string{"Active Icebath profile: coder.", "reads and writes " + profileHome + "/.", "The default profile's data lives at " + root + "/skills/"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("missing %q in %q", want, prompt)
			}
		}
		if strings.Contains(prompt, profileHome+"/profiles/coder") || strings.Contains(prompt, profileHome+"/skills/") {
			t.Fatalf("doubled or profile scoped default paths %q", prompt)
		}
	})
	t.Run("test_real_default_home_renders_default_branch", func(t *testing.T) {
		userHome := t.TempDir()
		root := filepath.Join(userHome, ".icebath")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", userHome)
		t.Setenv("ICEBATH_HOME", root)
		home.ResetKeyCache()
		prompt := fullPrompt(profilePrompt(t))
		if !strings.Contains(prompt, "Active Icebath profile: default.") || !strings.Contains(prompt, "under "+root+"/profiles/<name>/.") {
			t.Fatalf("prompt %q", prompt)
		}
	})
}

func TestStableTierIsIdenticalAcrossHomes(t *testing.T) {
	var tiers []string
	base := t.TempDir()
	for _, name := range []string{"a", "b"} {
		root := filepath.Join(base, name, ".icebath")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", filepath.Dir(root))
		t.Setenv("ICEBATH_HOME", root)
		home.ResetKeyCache()
		parts := profilePrompt(t)
		if !strings.Contains(parts.Volatile, "under "+root+"/profiles/<name>/.") {
			t.Fatalf("volatile %q", parts.Volatile)
		}
		tiers = append(tiers, parts.Stable)
	}
	if tiers[0] != tiers[1] {
		t.Fatal("stable tier differs across homes")
	}
}

func TestInvalidateClearsPluginFreeze(t *testing.T) {
	t.Run("test_invalidate_stashes_and_clears_plugin_snapshot", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		frozen := []plugins.RenderedSystemPromptSection{{ID: "frozen-section"}}
		a := &AIAgent{cachedSystemPrompt: "OLD PROMPT", cachedSystemPromptStatic: "OLD", pluginSectionsSnapshot: frozen, pluginSectionsFrozen: true}
		if err := InvalidateSystemPrompt(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if a.pluginSectionsFrozen || a.pluginSectionsSnapshot != nil || len(a.pluginSectionsPrevious) != 1 || a.pluginSectionsPrevious[0].ID != "frozen-section" || a.cachedSystemPrompt != "" {
			t.Fatalf("agent %+v", a.pluginSectionsPrevious)
		}
	})
}

// Makes the plugin section render raise for the rest of the test
func explodingPluginRender(t *testing.T) {
	t.Helper()
	prev := renderPromptSectionsFn
	renderPromptSectionsFn = func(context.Context, map[string]any) []plugins.RenderedSystemPromptSection {
		panic(fmt.Errorf("plugin exploded"))
	}
	t.Cleanup(func() { renderPromptSectionsFn = prev })
}

func TestPluginRerenderFailOpen(t *testing.T) {
	t.Run("test_raising_plugin_render_falls_back_to_previous_bytes", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		explodingPluginRender(t)
		a := &AIAgent{pluginSectionsPrevious: []plugins.RenderedSystemPromptSection{{ID: "last-good"}}}
		if got := frozenPluginPromptSections(context.Background(), a); len(got) != 1 || got[0].ID != "last-good" {
			t.Fatalf("rendered %v", got)
		}
	})
	t.Run("test_raising_plugin_render_without_previous_is_empty", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		explodingPluginRender(t)
		if got := frozenPluginPromptSections(context.Background(), &AIAgent{}); len(got) != 0 {
			t.Fatalf("rendered %v", got)
		}
	})
}

// A git repo with one committed source file, the hermes _init_repo fixture
func initCommittedRepo(t *testing.T, path, firstCommit string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}, {"config", "core.autocrlf", "false"}} {
		gitIn(t, path, args...)
	}
	if err := os.WriteFile(filepath.Join(path, "main.py"), []byte("print(1)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, path, "add", "-A")
	gitIn(t, path, "commit", "-qm", firstCommit)
	return path
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v %v %s", args, err, out)
	}
}

// Agent of the workspace pin cases, every gate off and the terminal tool loaded
func pinAgent(t *testing.T, names ...string) *AIAgent {
	t.Helper()
	if len(names) == 0 {
		names = []string{"terminal"}
	}
	a := spAgent(t, names...)
	a.skipContextFiles, a.platform, a.model, a.sessionID = true, "cli", "gpt-4o", "s1"
	a.parallelToolCallGuidance, a.executionGuidance, a.botModeProtocol = false, false, false
	return a
}

// Empties soul and context files and answers the environment hints with the text
func pinProbes(t *testing.T, hints string) {
	t.Helper()
	emptyPromptProbes(t)
	buildEnvironmentHintsFn = func(context.Context, *tools.Env) (string, error) { return hints, nil }
}

func buildPrompt(t *testing.T, a *AIAgent) string {
	t.Helper()
	p, err := BuildSystemPrompt(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Session store answering only the session row read the workspace pin seeds from
type promptRowStore struct {
	SessionStore
	prompt string
}

// The hermes double has no lineage walk, the AttributeError the start line reader swallows
func (s promptRowStore) GetConversationRoot(string) (string, error) {
	return "", fmt.Errorf("AttributeError: 'SimpleNamespace' object has no attribute 'get_conversation_root'")
}

func (s promptRowStore) GetSession(context.Context, string) (*pyjson.Object, error) {
	row := pyjson.NewObject()
	row.Set("system_prompt", s.prompt)
	return row, nil
}

func TestWorkspaceSnapshotPinnedAcrossCompaction(t *testing.T) {
	t.Run("test_workspace_snapshot_replayed_across_rebuilds_when_repo_mutates", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		repo := initCommittedRepo(t, filepath.Join(t.TempDir(), "proj"), "init commit")
		t.Setenv("TERMINAL_CWD", repo)
		pinProbes(t, "ENV HINTS")
		a := pinAgent(t, "terminal", "file_write")
		p1 := buildPrompt(t, a)
		if !strings.Contains(p1, "Workspace (snapshot at session start") {
			t.Fatalf("p1 %q", p1)
		}
		if err := os.WriteFile(filepath.Join(repo, "new_file.py"), []byte("print(2)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-qm", "second commit")
		_ = os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("wip\n"), 0o600)
		if err := InvalidateSystemPrompt(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		p2 := buildPrompt(t, a)
		if p1 != p2 || strings.Contains(p2, "second commit") {
			t.Fatalf("rebuild differs\n%q\n%q", p1, p2)
		}
	})
	t.Run("test_workspace_snapshot_reprobes_when_cwd_changes", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		base := t.TempDir()
		repo1 := initCommittedRepo(t, filepath.Join(base, "r1"), "init r1")
		repo2 := initCommittedRepo(t, filepath.Join(base, "r2"), "init r2")
		pinProbes(t, "ENV HINTS")
		a := pinAgent(t)
		t.Setenv("TERMINAL_CWD", repo1)
		if p1 := buildPrompt(t, a); !strings.Contains(p1, "init r1") {
			t.Fatalf("p1 %q", p1)
		}
		t.Setenv("TERMINAL_CWD", repo2)
		if p2 := buildPrompt(t, a); !strings.Contains(p2, "init r2") {
			t.Fatalf("p2 %q", p2)
		}
	})
	t.Run("test_session_boundary_drops_the_pin_so_a_new_session_resnapshots", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		repo := initCommittedRepo(t, filepath.Join(t.TempDir(), "proj"), "init commit")
		t.Setenv("TERMINAL_CWD", repo)
		pinProbes(t, "ENV HINTS")
		a := pinAgent(t)
		buildPrompt(t, a)
		gitIn(t, repo, "commit", "-qm", "second commit", "--allow-empty")
		a.ResetSessionState()
		if err := InvalidateSystemPrompt(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		if p := buildPrompt(t, a); !strings.Contains(p, "second commit") {
			t.Fatalf("prompt %q", p)
		}
	})
	t.Run("test_binding_the_launch_dir_explicitly_replays_the_pin", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		repo := initCommittedRepo(t, filepath.Join(t.TempDir(), "proj"), "init commit")
		t.Chdir(repo)
		pinProbes(t, "ENV HINTS")
		a := pinAgent(t)
		t.Setenv("TERMINAL_CWD", "")
		os.Unsetenv("TERMINAL_CWD")
		p1 := buildPrompt(t, a)
		if !strings.Contains(p1, "Status: clean") {
			t.Fatalf("p1 %q", p1)
		}
		_ = os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("wip\n"), 0o600)
		if err := InvalidateSystemPrompt(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TERMINAL_CWD", repo)
		if p2 := buildPrompt(t, a); p2 != p1 {
			t.Fatalf("rebuild differs\n%q\n%q", p1, p2)
		}
	})
	t.Run("test_agent_that_did_not_build_the_prompt_replays_the_persisted_snapshot", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		base := t.TempDir()
		repo, other := initCommittedRepo(t, filepath.Join(base, "proj"), "init commit"), initCommittedRepo(t, filepath.Join(base, "other"), "init other")
		envHints := func(cwd string) string { return "Host: x\nUser home directory: /h\nCurrent working directory: " + cwd }
		pinProbes(t, envHints(repo))
		t.Setenv("TERMINAL_CWD", repo)
		stored := buildPrompt(t, pinAgent(t))
		if !strings.Contains(stored, "Status: clean") {
			t.Fatalf("stored %q", stored)
		}
		_ = os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("wip\n"), 0o600)
		resumed := pinAgent(t)
		resumed.sessionDB = promptRowStore{prompt: stored}
		if got := buildPrompt(t, resumed); got != stored {
			t.Fatalf("resumed\n%q\nstored\n%q", got, stored)
		}
		buildEnvironmentHintsFn = func(context.Context, *tools.Env) (string, error) { return envHints(other), nil }
		t.Setenv("TERMINAL_CWD", other)
		moved := pinAgent(t)
		moved.sessionDB = promptRowStore{prompt: stored}
		if got := buildPrompt(t, moved); !strings.Contains(got, "- Root: "+other) {
			t.Fatalf("moved %q", got)
		}
	})
	t.Run("test_persisted_prompt_without_a_snapshot_does_not_pin_an_empty_one", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		repo := initCommittedRepo(t, filepath.Join(t.TempDir(), "proj"), "init commit")
		stored := "Host: x\nUser home directory: /h\nCurrent working directory: " + repo + "\n\nBODY"
		pinProbes(t, "Host: x\nUser home directory: /h\nCurrent working directory: "+repo)
		t.Setenv("TERMINAL_CWD", repo)
		resumed := pinAgent(t)
		resumed.sessionDB = promptRowStore{prompt: stored}
		if got := buildPrompt(t, resumed); !strings.Contains(got, "- Root: "+repo) {
			t.Fatalf("resumed %q", got)
		}
	})
}

// Agent of the profile precedence cases, its session store opened in the given home
func profileDBAgent(t *testing.T, h string, names ...string) *AIAgent {
	t.Helper()
	db, err := state.OpenHome(h, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := spAgent(t, names...)
	a.loadSoulIdentity, a.skipContextFiles, a.sessionDB = true, true, db
	return a
}

func TestBoundOverrideWinsOverSharedDbHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	botHome := filepath.Join(root, "profiles", "mybot")
	if err := os.MkdirAll(botHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	a := profileDBAgent(t, root)
	ctx := home.WithOverride(context.Background(), botHome)
	if got := agentOwnHome(ctx, a); got != botHome {
		t.Fatalf("home %q", got)
	}
	if got := profileNameForHome(agentOwnHome(ctx, a)); got != "mybot" {
		t.Fatalf("profile %q", got)
	}
}

func TestDbHomeWinsOnBareThreadWithoutOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	botHome := filepath.Join(root, "profiles", "mybot")
	if err := os.MkdirAll(botHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	a := profileDBAgent(t, botHome)
	got := make(chan string, 1)
	go func() { got <- agentOwnHome(context.Background(), a) }()
	if h := <-got; h != botHome {
		t.Fatalf("home %q", h)
	}
}

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFullPromptScopedToBotOnBareThread(t *testing.T) {
	defaultHome := filepath.Join(t.TempDir(), "root")
	writeSkill(t, filepath.Join(defaultHome, "skills", "general", "leaky-skill"), "leaky-skill", "default-only skill")
	if err := os.WriteFile(filepath.Join(defaultHome, "SOUL.md"), []byte("DEFAULT SOUL"), 0o600); err != nil {
		t.Fatal(err)
	}
	botHome := filepath.Join(defaultHome, "profiles", "mybot")
	writeSkill(t, filepath.Join(botHome, "skills", "general", "bot-skill"), "bot-skill", "bot-only skill")
	if err := os.WriteFile(filepath.Join(botHome, "SOUL.md"), []byte("BOT SOUL"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", defaultHome)
	skills.ClearSkillsSystemPromptCache(defaultHome, false)
	a := profileDBAgent(t, botHome, "skill_view")
	prevHints := buildEnvironmentHintsFn
	buildEnvironmentHintsFn = func(context.Context, *tools.Env) (string, error) { return "", nil }
	t.Cleanup(func() { buildEnvironmentHintsFn = prevHints })
	got := make(chan string, 1)
	go func() {
		p, err := BuildSystemPrompt(context.Background(), a, nil)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- p
	}()
	prompt := <-got
	if !strings.Contains(prompt, "BOT SOUL") || strings.Contains(prompt, "DEFAULT SOUL") {
		t.Fatalf("identity %q", prompt)
	}
	block := ""
	if m := regexp.MustCompile(`(?s)<available_skills>(.*?)</available_skills>`).FindStringSubmatch(prompt); m != nil {
		block = m[1]
	}
	if !strings.Contains(block, "bot-skill") || strings.Contains(block, "leaky-skill") {
		t.Fatalf("skills block %q", block)
	}
	if !strings.Contains(prompt, "Active Icebath profile: mybot") || strings.Contains(prompt, "Active Icebath profile: default") {
		t.Fatalf("profile line %q", prompt)
	}
}

func TestPluginSessionInfoProfileFromAgentHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	botHome := filepath.Join(root, "profiles", "mybot")
	if err := os.MkdirAll(botHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	a := profileDBAgent(t, botHome)
	got := make(chan map[string]any, 1)
	go func() { got <- pluginSessionInfo(context.Background(), a) }()
	if info := <-got; info["profile_name"] != "mybot" {
		t.Fatalf("info %v", info)
	}
}
