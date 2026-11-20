package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Minimal function tool definitions in the test_run_agent _make_tool_defs shape
func makeToolDefs(names ...string) []map[string]any {
	defs := make([]map[string]any, 0, len(names))
	for _, n := range names {
		defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
			"name": n, "description": n + " tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}})
	}
	return defs
}

// Publishes the named tool definitions on the agent as the patched get_tool_definitions would
func withToolDefs(a *AIAgent, names ...string) *AIAgent {
	valid := map[string]bool{}
	for _, n := range names {
		valid[n] = true
	}
	a.SetToolSnapshot(makeToolDefs(names...), valid)
	return a
}

func mustSystemPrompt(t *testing.T, a *AIAgent) string {
	t.Helper()
	prompt, err := BuildSystemPrompt(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func conversationStartedLine(t *testing.T, prompt string) string {
	t.Helper()
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "Conversation started:") {
			return line
		}
	}
	t.Fatal("Expected a 'Conversation started:' line in the system prompt")
	return ""
}

func TestBuildSystemPromptAlwaysHasIdentity(t *testing.T) {
	if !strings.Contains(mustSystemPrompt(t, withToolDefs(runAgentFixture(t), "web_search")), config.DefaultIdentity) {
		t.Fatal("default identity missing")
	}
}

func TestCanUseSoulIdentityEvenWhenContextFilesAreSkipped(t *testing.T) {
	prev := loadSoulMDFn
	t.Cleanup(func() { loadSoulMDFn = prev })
	loadSoulMDFn = func(context.Context, *tools.Env, *int, string) (string, bool) { return "SOUL IDENTITY", true }
	a := withToolDefs(newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", LoadSoulIdentity: true}), "terminal")
	prompt := mustSystemPrompt(t, a)
	if !strings.Contains(prompt, "SOUL IDENTITY") || strings.Contains(prompt, config.DefaultIdentity) {
		t.Fatalf("prompt %q", firstRunes(prompt, 200))
	}
}

func TestMemoryGuidanceWhenMemoryToolLoaded(t *testing.T) {
	a := withToolDefs(runAgentFixture(t), "web_search", "memory")
	a.memoryEnabled = true
	if !strings.Contains(mustSystemPrompt(t, a), memory.BuildMemoryGuidance(true, true, false)) {
		t.Fatal("memory guidance missing")
	}
}

func TestNoMemoryGuidanceWhenBothBuiltinStoresDisabled(t *testing.T) {
	a := withToolDefs(runAgentFixture(t), "web_search", "memory")
	a.memoryEnabled, a.userProfileEnabled = false, false
	prompt := mustSystemPrompt(t, a)
	if strings.Contains(prompt, memory.MemoryGuidance) || strings.Contains(prompt, memory.UserProfileGuidance) {
		t.Fatal("guidance injected for disabled stores")
	}
}

func TestProfileGuidanceWhenOnlyUserProfileEnabled(t *testing.T) {
	a := withToolDefs(runAgentFixture(t), "web_search", "memory")
	a.memoryEnabled, a.userProfileEnabled = false, true
	prompt := mustSystemPrompt(t, a)
	if strings.Contains(prompt, memory.MemoryGuidance) || !strings.Contains(prompt, memory.BuildMemoryGuidance(false, true, false)) {
		t.Fatal("profile only guidance not injected alone")
	}
}

func TestDatetimeIsDateOnlyNotMinutePrecision(t *testing.T) {
	line := conversationStartedLine(t, mustSystemPrompt(t, withToolDefs(runAgentFixture(t), "web_search")))
	if strings.Contains(line, " AM") || strings.Contains(line, " PM") {
		t.Fatalf("Timestamp line has time-of-day, breaks daily cache stability: %q", line)
	}
	if regexp.MustCompile(`:\d{2}`).MatchString(strings.SplitN(line, " (", 2)[0]) {
		t.Fatalf("Timestamp line has HH:MM, breaks daily cache stability: %q", line)
	}
}

func TestDatetimeIncludesUTCOffset(t *testing.T) {
	line := conversationStartedLine(t, mustSystemPrompt(t, withToolDefs(runAgentFixture(t), "web_search")))
	if !regexp.MustCompile(`UTC[+-]\d{2}:\d{2}`).MatchString(line) {
		t.Fatalf("Timestamp line is missing a UTC offset: %q", line)
	}
}

func TestDatetimeLineIsStableAcrossRebuilds(t *testing.T) {
	a := withToolDefs(runAgentFixture(t), "web_search")
	if first, second := conversationStartedLine(t, mustSystemPrompt(t, a)), conversationStartedLine(t, mustSystemPrompt(t, a)); first != second {
		t.Fatalf("%q != %q", first, second)
	}
}

func TestSkillsPromptDerivesAvailableToolsetsFromLoadedTools(t *testing.T) {
	prev := buildSkillsSystemPromptFn
	t.Cleanup(func() { buildSkillsSystemPromptFn = prev })
	var got []skills.SkillsPromptOptions
	buildSkillsSystemPromptFn = func(_ *skills.Scope, _ context.Context, opts skills.SkillsPromptOptions) (string, error) {
		got = append(got, opts)
		return "SKILLS_PROMPT", nil
	}
	a := withToolDefs(runAgentFixture(t), "web_search", "skills_list", "skill_view", "skill_manage")
	if !strings.Contains(mustSystemPrompt(t, a), "SKILLS_PROMPT") || len(got) == 0 {
		t.Fatal("skills prompt not injected")
	}
	last := got[len(got)-1]
	wantTools := map[string]bool{"web_search": true, "skills_list": true, "skill_view": true, "skill_manage": true}
	if !reflect.DeepEqual(last.AvailableTools, wantTools) || !reflect.DeepEqual(last.AvailableToolsets, map[string]bool{"web": true, "skills": true}) {
		t.Fatalf("tools %v toolsets %v", last.AvailableTools, last.AvailableToolsets)
	}
}

// Fixture agent whose config.yaml carries the agent section and whose tool surface is the named tools
func guidanceAgent(t *testing.T, model, agentYAML string, toolNames ...string) *AIAgent {
	t.Helper()
	o := AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: model}
	if len(toolNames) == 0 {
		o.EnabledToolsets = []string{}
	}
	return withToolDefs(newFixtureAgentWithConfig(t, "agent:\n"+agentYAML, o), toolNames...)
}

func TestNoToolsNeverInjects(t *testing.T) {
	if strings.Contains(mustSystemPrompt(t, guidanceAgent(t, "", "  tool_use_enforcement: true\n")), ToolUseEnforcementGuidance) {
		t.Fatal("enforcement injected without tools")
	}
}

func TestExecutionGuidanceConfig(t *testing.T) {
	cases := []struct {
		name, model, guidance string
		want                  bool
	}{
		{"config_false_suppresses", "deepseek/deepseek-v4-pro", "false", false},
		{"config_list_matches", "moonshotai/kimi-k3", "[kimi]", true},
		{"config_list_non_match_suppresses", "openai/gpt-4.1", "[kimi]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := guidanceAgent(t, c.model, "  tool_use_enforcement: false\n  execution_guidance: "+c.guidance+"\n", "terminal", "web_search")
			if got := strings.Contains(mustSystemPrompt(t, a), OpenAIModelExecutionGuidance); got != c.want {
				t.Fatalf("execution guidance present %v, want %v", got, c.want)
			}
		})
	}
}

func TestTaskCompletionGuidanceDefaultInjectsForClaude(t *testing.T) {
	a := guidanceAgent(t, "anthropic/claude-opus-4.8", "  task_completion_guidance: true\n", "terminal", "web_search")
	if !strings.Contains(mustSystemPrompt(t, a), TaskCompletionGuidance) {
		t.Fatal("task completion guidance missing")
	}
}

func TestTaskCompletionGuidanceNoToolsNoInjection(t *testing.T) {
	if strings.Contains(mustSystemPrompt(t, guidanceAgent(t, "", "  task_completion_guidance: true\n")), TaskCompletionGuidance) {
		t.Fatal("task completion guidance injected without tools")
	}
}

// PATH holding only fake python3 and pip executables that answer the env probe questions
func fakePythonToolchain(t *testing.T, py3Version string, py3HasPip, pep668 bool, pipBound string) {
	t.Helper()
	dir := t.TempDir()
	pipExit, pep := 1, "no"
	if py3HasPip {
		pipExit = 0
	}
	if pep668 {
		pep = "yes"
	}
	py3 := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"-m\" ]; then exit %d; fi\ncase \"$2\" in\n*EXTERNALLY-MANAGED*) echo %s ;;\n*version_info*) echo %s ;;\nesac\n", pipExit, pep, py3Version)
	pip := fmt.Sprintf("#!/bin/sh\necho 'pip 24.0 from /usr/lib/pip (python %s)'\n", pipBound)
	for name, body := range map[string]string{"python3": py3, "pip": pip} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	tools.ResetEnvironmentProbe()
	t.Cleanup(tools.ResetEnvironmentProbe)
}

func TestProbeAppearsWhenProblemDetected(t *testing.T) {
	fakePythonToolchain(t, "3.11.15", false, true, "3.12")
	prompt := mustSystemPrompt(t, guidanceAgent(t, "anthropic/claude-opus-4.8", "  environment_probe: true\n", "terminal"))
	if !strings.Contains(prompt, "Python toolchain:") || !strings.Contains(prompt, "3.11.15") {
		t.Fatal("probe line missing")
	}
}

func TestProbeSilentOnCleanEnv(t *testing.T) {
	fakePythonToolchain(t, "3.13.3", true, false, "3.13")
	if strings.Contains(mustSystemPrompt(t, guidanceAgent(t, "anthropic/claude-opus-4.8", "  environment_probe: true\n", "terminal")), "Python toolchain:") {
		t.Fatal("probe line on a clean toolchain")
	}
}

func TestProbeDisabledByConfig(t *testing.T) {
	fakePythonToolchain(t, "3.11.15", false, true, "3.12")
	if strings.Contains(mustSystemPrompt(t, guidanceAgent(t, "anthropic/claude-opus-4.8", "  environment_probe: false\n", "terminal")), "Python toolchain:") {
		t.Fatal("probe line with the probe disabled")
	}
}

func TestInvalidateSystemPromptClearsCache(t *testing.T) {
	a := runAgentFixture(t)
	a.cachedSystemPrompt = "cached value"
	if err := InvalidateSystemPrompt(context.Background(), a); err != nil || a.cachedSystemPrompt != "" {
		t.Fatalf("cache %q err %v", a.cachedSystemPrompt, err)
	}
}
