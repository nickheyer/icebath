package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reference output of the hermes commands rendered by dev/slices/cmdinfo_s24/info_ref.py
type infoReference struct {
	Columns int                        `json:"columns"`
	Cases   map[string]json.RawMessage `json:"cases"`
}

func loadInfoReference(t *testing.T) infoReference {
	t.Helper()
	raw, err := os.ReadFile("testdata/cli_info_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref infoReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func (r infoReference) text(t *testing.T, name string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(r.Cases[name], &s); err != nil {
		t.Fatalf("case %s: %v", name, err)
	}
	return s
}

// A bare CLI whose prints land in one buffer under a fresh home at the reference width
func referenceCLI(t *testing.T, columns int) (*CLI, *lockedBuffer) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("COLUMNS", strconv.Itoa(columns))
	t.Setenv("LINES", "40")
	t.Setenv("ICEBATH_DEFER_AGENT_STARTUP", "1")
	for _, v := range []string{"TERMUX_VERSION", "PREFIX", "NO_COLOR", "TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "WHATSAPP_ENABLED"} {
		t.Setenv(v, "")
	}
	out := &lockedBuffer{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}, &bytes.Buffer{})
	t.Cleanup(restore)
	modernGapPending.Store(false)
	c := bareCLI()
	c.console = console()
	c.config = map[string]any{}
	c.model, c.sessionID, c.toolProgressMode = "anthropic/claude-sonnet-4", "sess-1", "all"
	c.enabledToolsets, c.disabledToolsets = []string{}, []string{}
	return c, out
}

func assertReference(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s mismatch\n got: %q\nwant: %q", name, got, want)
	}
}

func referenceHelpSeams(t *testing.T, withSkills, withBundles, withNotes bool) {
	t.Helper()
	skillCmds := skills.SkillCommands{}
	if withSkills {
		skillCmds = skills.SkillCommands{"/alpha": {"description": "Alpha skill [x] does things"}, "/beta-tool": {"description": "Beta tool"}}
	}
	bundles := skills.SkillCommands{}
	if withBundles {
		bundles = skills.SkillCommands{"/pack": {"skills": []any{"a", "b"}, "description": ""}, "/review-kit": {"skills": []any{"c"}, "description": "Review kit"}}
	}
	var notes []string
	if withNotes {
		notes = []string{"slash command /handoff unavailable — name taken by built-in; use /skill handoff"}
	}
	swap(t, &helpSkillCommands, func(*CLI) skills.SkillCommands { return skillCmds })
	swap(t, &helpSkillBundles, func(*CLI) skills.SkillCommands { return bundles })
	swap(t, &skillNotesFor, func(*CLI) []string { return notes })
}

var referenceQuick = map[string]any{"deploy": map[string]any{"type": "exec", "command": "x"}, "status": map[string]any{"description": "Show status", "type": "exec"}}

func TestShowHelpMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	cases := []struct {
		name, model, arg                string
		skills, bundles, notes, withQck bool
	}{
		{"help_default", "anthropic/claude-sonnet-4", "", true, true, true, true},
		{"help_skills", "anthropic/claude-sonnet-4", "skills", true, true, true, false},
		{"help_skills_empty", "anthropic/claude-sonnet-4", "Skill", false, true, false, false},
		{"help_query", "anthropic/claude-sonnet-4", "  ALPHA ", true, true, true, true},
		{"help_query_model", "gpt-5.4", "model", false, false, true, false},
		{"help_no_skills", "gpt-5.4", "", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			c.model = tc.model
			if tc.withQck {
				c.config = map[string]any{"quick_commands": referenceQuick}
			}
			referenceHelpSeams(t, tc.skills, tc.bundles, tc.notes)
			c.showHelp(tc.arg)
			assertReference(t, tc.name, out.String(), ref.text(t, tc.name))
		})
	}
}

var referenceTools = []map[string]any{
	{"type": "function", "function": map[string]any{"name": "web_search", "description": "Search the web. Returns results e.g. titles."}},
	{"type": "function", "function": map[string]any{"name": "terminal", "description": "Run shell commands on the host\nSecond line ignored."}},
	{"type": "function", "function": map[string]any{"name": "read_file", "description": "Read a file v2.0 safely"}},
	{"type": "function", "function": map[string]any{"name": "mcp_linear_create", "description": ""}},
}

var referenceToolsetOf = map[string]string{"web_search": "web", "terminal": "terminal", "read_file": "file"}

func TestShowToolsMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	for name, defs := range map[string][]map[string]any{"tools": referenceTools, "tools_empty": {}} {
		t.Run(name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			swap(t, &toolDefinitionsFor, func(_ *CLI, skip bool) []map[string]any {
				if !skip {
					t.Fatal("/tools must list the catalog without the tool search assembly")
				}
				return defs
			})
			swap(t, &toolsetOfTool, func(_ *CLI, name string) string { return referenceToolsetOf[name] })
			c.showTools()
			assertReference(t, name, out.String(), ref.text(t, name))
		})
	}
}

func TestShowToolsetsMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	info := map[string]tools.ToolsetDetail{"file": {Description: "File tools", ToolCount: 4}, "terminal": {Description: "Shell access", ToolCount: 2},
		"web": {Description: "Web search and extract", ToolCount: 2}}
	for name, enabled := range map[string][]string{"toolsets": {"web", "file"}, "toolsets_none_enabled": nil} {
		t.Run(name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			c.enabledToolsets = enabled
			swap(t, &allToolsetNames, func(context.Context) []string { return []string{"web", "hidden", "terminal", "file"} })
			swap(t, &toolsetInfoOf, func(_ context.Context, name string) (tools.ToolsetDetail, bool) {
				d, ok := info[name]
				return d, ok
			})
			c.showToolsets()
			assertReference(t, name, out.String(), ref.text(t, name))
		})
	}
}

func TestWhoamiMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	for name, user := range map[string]func() (string, error){
		"whoami":         func() (string, error) { return "nick", nil },
		"whoami_unknown": func() (string, error) { return "", errors.New("no user") },
	} {
		t.Run(name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			swap(t, &getpassUser, user)
			c.handleWhoamiCommand()
			assertReference(t, name, out.String(), ref.text(t, name))
		})
	}
}

func TestShowGatewayStatusMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	t.Run("platforms_error", func(t *testing.T) {
		c, out := referenceCLI(t, ref.Columns)
		swap(t, &loadGatewayConfig, func(context.Context) (*gateway.GatewayConfig, error) {
			return nil, errors.New("config.yaml unreadable")
		})
		c.showGatewayStatus()
		want := strings.ReplaceAll(ref.text(t, "platforms_error"), "{HOME}", home.Display(home.Dir(c.ctx)))
		assertReference(t, "platforms_error", out.String(), want)
	})
	t.Run("platforms_clean", func(t *testing.T) {
		c, out := referenceCLI(t, ref.Columns)
		c.showGatewayStatus()
		want := strings.ReplaceAll(ref.text(t, "platforms_clean"), "{HOME}", home.Display(home.Dir(c.ctx)))
		assertReference(t, "platforms_clean", out.String(), want)
	})
}

func TestToggleVerboseMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	for _, mode := range []string{"off", "new", "all", "verbose", "nope"} {
		t.Run(mode, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			c.toolProgressMode = mode
			c.toggleVerbose()
			assertReference(t, mode, out.String(), ref.text(t, "verbose_from_"+mode))
		})
	}
}

// Agent reading of the /usage references, a provider reading unless the real prompt figure differs
func referenceUsage(lastPrompt, lastReal, ctxLen, compressions, reasoning, configCtx, calls int) agentUsageReading {
	source := "provider_usage"
	if lastPrompt > 0 && lastPrompt != lastReal {
		source = "local_estimate"
	}
	return agentUsageReading{Model: "anthropic/claude-sonnet-4", Calls: calls, Input: 10230, Output: 2220, Reasoning: reasoning, Prompt: 10230,
		Completion: 2220, Total: 12450, HasCompressor: true, LastPromptTokens: lastPrompt, ContextLength: ctxLen, Compressions: compressions,
		DisplaySource: source, ConfigContextLength: configCtx}
}

func TestShowUsageMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	cases := []struct {
		name, command string
		reading       *agentUsageReading
		provider      string
	}{
		{"usage_agent", "/usage", ptr(referenceUsage(12450, 12450, 200000, 2, 512, 200000, 7)), ""},
		{"usage_estimated", "", ptr(referenceUsage(99500, 90000, 200000, 0, 0, 0, 7)), ""},
		{"usage_over_window", "", ptr(referenceUsage(250000, 250000, 200000, 11, 512, 200000, 7)), ""},
		{"usage_no_calls", "", ptr(referenceUsage(12450, 12450, 200000, 2, 512, 200000, 0)), ""},
		{"usage_no_agent", "", nil, ""},
		{"usage_reset_wrong_provider", "/usage RESET --force", nil, "openrouter"},
		{"usage_unknown", "/usage foo Bar", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			c.provider = tc.provider
			c.sessionStart = time.Now().Add(-(3*time.Hour + 5*time.Minute))
			c.conversationHistory = []map[string]any{{"role": "user", "content": "a"}, {"role": "user", "content": "a"}, {"role": "user", "content": "a"}, {"role": "user", "content": "a"}}
			if tc.reading != nil {
				c.agent = &agent.AIAgent{}
				reading := *tc.reading
				swap(t, &readAgentUsage, func(*CLI, *agent.AIAgent) agentUsageReading { return reading })
			}
			if tc.command != "" {
				c.handleUsageCommand(tc.command)
			} else {
				c.showUsage()
			}
			assertReference(t, tc.name, out.String(), ref.text(t, tc.name))
		})
	}
	t.Run("usage_reset_codex", func(t *testing.T) {
		c, out := referenceCLI(t, ref.Columns)
		c.provider, c.baseURL, c.apiKey = "openai-codex", "https://chatgpt.com/backend-api", "k"
		var kwargs map[string]any
		swap(t, &redeemCodexReset, func(_ context.Context, baseURL, apiKey string, force bool) provider.CodexResetRedeemResult {
			kwargs = map[string]any{"api_key": apiKey, "base_url": baseURL, "force": force}
			return provider.CodexResetRedeemResult{Message: "Redeemed 1 reset credit; 2 windows reset"}
		})
		c.handleUsageCommand("/usage reset --force")
		assertReference(t, "usage_reset_codex", out.String(), ref.text(t, "usage_reset_codex"))
		got, _ := json.Marshal(kwargs)
		var want map[string]any
		_ = json.Unmarshal([]byte(ref.text(t, "usage_reset_codex_kwargs")), &want)
		wantJSON, _ := json.Marshal(want)
		assertReference(t, "usage_reset_codex_kwargs", string(got), string(wantJSON))
	})
}

func ptr[T any](v T) *T { return &v }

func TestShowContextBreakdownWithoutAgentMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	c, out := referenceCLI(t, ref.Columns)
	c.showContextBreakdown("/context all")
	assertReference(t, "context_no_agent", out.String(), ref.text(t, "context_no_agent"))
}

func TestReloadSkillsMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	results := map[string]map[string]any{
		"reload_skills_changes": {"added": []any{map[string]any{"name": "alpha", "description": "Run alpha to do xyz"}, map[string]any{"name": "beta", "description": ""}},
			"removed": []any{map[string]any{"name": "gamma", "description": "Old removed skill"}}, "total": 3, "commands": 3},
		"reload_skills_added_only": {"added": []any{map[string]any{"name": "solo", "description": "Only one"}}, "removed": []any{}, "total": 1},
		"reload_skills_none":       {"added": []any{}, "removed": []any{}, "total": 5},
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			var want struct {
				Out  string  `json:"out"`
				Note *string `json:"note"`
			}
			if err := json.Unmarshal(ref.Cases[name], &want); err != nil {
				t.Fatal(err)
			}
			c, out := referenceCLI(t, ref.Columns)
			swap(t, &reloadSkillsFor, func(*CLI) (map[string]any, error) { return result, nil })
			c.reloadSkills()
			assertReference(t, name, out.String(), want.Out)
			wantNote := ""
			if want.Note != nil {
				wantNote = *want.Note
			}
			assertReference(t, name+" note", c.pendingSkillsReloadNote, wantNote)
		})
	}
}

func TestReloadMCPMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	cases := []struct {
		name          string
		old, new      []string
		tools         []string
		discoverError string
	}{
		{"reload_mcp_changes", []string{"github", "stale"}, []string{"github", "linear"}, []string{"mcp_github_a", "mcp_linear_b", "mcp_linear_c"}, ""},
		{"reload_mcp_none", nil, nil, nil, ""},
		{"reload_mcp_failed", []string{"github"}, nil, nil, "boom"},
	}
	keys := func(names []string) []mcp.ServerKey {
		out := []mcp.ServerKey{}
		for _, n := range names {
			out = append(out, mcp.ServerKey{Name: n})
		}
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want struct {
				Out     string           `json:"out"`
				History []map[string]any `json:"history"`
			}
			if err := json.Unmarshal(ref.Cases[tc.name], &want); err != nil {
				t.Fatal(err)
			}
			c, out := referenceCLI(t, ref.Columns)
			c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}}
			live := keys(tc.old)
			swap(t, &mcpServerKeys, func() []mcp.ServerKey { return live })
			swap(t, &mcpShutdown, func(context.Context) {})
			swap(t, &mcpReprobe, func() {})
			swap(t, &mcpDiscover, func(*CLI) ([]string, error) {
				if tc.discoverError != "" {
					return nil, errors.New(tc.discoverError)
				}
				live = keys(tc.new)
				return tc.tools, nil
			})
			c.reloadMCP()
			assertReference(t, tc.name, out.String(), want.Out)
			got, _ := json.Marshal(c.conversationHistory)
			wantHistory, _ := json.Marshal(want.History)
			assertReference(t, tc.name+" history", string(got), string(wantHistory))
		})
	}
}

func TestShowBannerWarningsMatchReference(t *testing.T) {
	ref := loadInfoReference(t)
	cases := []struct {
		name, baseURL, model string
		ctxLen               int
	}{
		{"banner_lm_studio", "localhost:1234", "test-model", 4096},
		{"banner_ollama_host", "http://ollama.internal:8080/v1", "test-model", 32768},
		{"banner_remote", "https://api.example.com/v1", "test-model", 8192},
		{"banner_ollama_port", "http://10.0.0.5:11434", "test-model", 16000},
		{"banner_bad_port", "http://host:99999/v1", "test-model", 4096},
		{"banner_ok_context", "", "test-model", 200000},
		{"banner_hermes_model", "", "NousResearch/Hermes-4-70B", 200000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, out := referenceCLI(t, ref.Columns)
			t.Chdir(t.TempDir())
			c.model, c.baseURL, c.enabledToolsets = tc.model, tc.baseURL, []string{"file"}
			c.agent = &agent.AIAgent{}
			swap(t, &readAgentUsage, func(*CLI, *agent.AIAgent) agentUsageReading {
				return agentUsageReading{HasCompressor: true, ContextLength: tc.ctxLen}
			})
			swap(t, &toolDefinitionsFor, func(*CLI, bool) []map[string]any { return []map[string]any{} })
			swap(t, &welcomeBannerPrinter, func(context.Context, *pyrich.Console, welcomeBanner) error { return nil })
			c.showBanner()
			assertReference(t, tc.name, out.String(), ref.text(t, tc.name))
		})
	}
}

func TestShowStatusMatchesReference(t *testing.T) {
	ref := loadInfoReference(t)
	t.Run("status_line", func(t *testing.T) {
		c, out := referenceCLI(t, ref.Columns)
		t.Setenv("ICEBATH_DEFER_AGENT_STARTUP", "")
		c.model, c.apiKey, c.enabledToolsets, c.provider, c.providerSource = "anthropic/claude-sonnet-4-20250514", "k", []string{"file", "web"}, "openrouter", "env"
		swap(t, &toolDefinitionsFor, func(*CLI, bool) []map[string]any { return referenceTools[:3] })
		c.showStatus()
		assertReference(t, "status_line", out.String(), ref.text(t, "status_line"))
	})
	t.Run("status_line_long_model", func(t *testing.T) {
		c, out := referenceCLI(t, ref.Columns)
		t.Setenv("ICEBATH_DEFER_AGENT_STARTUP", "")
		c.model, c.apiKey, c.enabledToolsets, c.provider = "local/"+strings.Repeat("x", 40), "", []string{"all"}, "custom"
		swap(t, &toolDefinitionsFor, func(*CLI, bool) []map[string]any { return referenceTools[:3] })
		c.showStatus()
		assertReference(t, "status_line_long_model", out.String(), ref.text(t, "status_line_long_model"))
	})
}
