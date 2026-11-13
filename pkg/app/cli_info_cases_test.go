package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A bare CLI printing into the recorder of a fresh home, wide enough that no console line wraps
func infoCLI(t *testing.T) (*CLI, *printRecorder) {
	t.Helper()
	t.Setenv("COLUMNS", "500")
	rec := recordPrints(t)
	c := bareCLI()
	c.console = console()
	c.config = map[string]any{}
	return c, rec
}

// Swaps a package seam for the test and restores it afterwards
func swap[T any](t *testing.T, slot *T, v T) {
	t.Helper()
	prev := *slot
	*slot = v
	t.Cleanup(func() { *slot = prev })
}

// Recorded lines that contain the fragment
func linesWith(lines []string, fragment string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, fragment) {
			out = append(out, l)
		}
	}
	return out
}

func tr(key string, kw map[string]any) string { return i18n.T(context.Background(), key, "", kw) }

// Gives the case its own home when it has not made one yet
func caseHome(t *testing.T) {
	t.Helper()
	if os.Getenv("ICEBATH_HOME") == "" {
		t.Setenv("ICEBATH_HOME", t.TempDir())
	}
}

func freshI18n(t *testing.T) {
	t.Helper()
	caseHome(t)
	i18n.ResetLanguageCache()
	t.Cleanup(i18n.ResetLanguageCache)
}

func TestToolProgressLabelIsLocalizedPerModeAndEmptyForUnknown(t *testing.T) {
	freshI18n(t)
	for _, mode := range []string{"off", "new", "all", "verbose"} {
		line := toolProgressLabel(context.Background(), mode)
		if !strings.Contains(line, tr("cli.verbose.label_"+mode, nil)) || !strings.Contains(line, tr("cli.verbose.detail_"+mode, nil)) {
			t.Fatalf("mode %s line %q", mode, line)
		}
	}
	if got := toolProgressLabel(context.Background(), "nope"); got != "" {
		t.Fatalf("unknown mode %q", got)
	}
}

func TestReloadMcpChoicesCarryStableIdsAndCatalogLabels(t *testing.T) {
	freshI18n(t)
	choices := reloadMCPChoices(context.Background())
	var ids []string
	for _, c := range choices {
		ids = append(ids, c.Value)
	}
	if !reflect.DeepEqual(ids, []string{"once", "always", "cancel"}) || choices[0].Label != tr("cli.reload_mcp.choice_once", nil) {
		t.Fatalf("choices %+v", choices)
	}
}

func TestHelpSectionTitlesLocalizeKnownCategoriesAndPassUnknownThrough(t *testing.T) {
	freshI18n(t)
	ctx := context.Background()
	if helpSectionTitle(ctx, "Tools & Skills") != tr("cli.help.section_tools_skills", nil) || helpSectionTitle(ctx, "Custom Plugin Category") != "Custom Plugin Category" {
		t.Fatal("section titles")
	}
}

func TestChatErrorCopyUsesCatalogLeadAndDetails(t *testing.T) {
	freshI18n(t)
	text := chatErrorResponse(context.Background(), "upstream said no", "openrouter", "m", "rate_limit")
	want := tr("cli.error.with_details", map[string]any{"lead": tr("cli.error.rate_limit", map[string]any{"provider": "openrouter", "model": "m"}), "details": "upstream said no"})
	if text != want {
		t.Fatalf("got %q want %q", text, want)
	}
	if got := agentInitFailureMessage(errors.New("boom")); got != tr("cli.error.agent_init_failed", map[string]any{"error": "boom"}) {
		t.Fatalf("init failure %q", got)
	}
}

func TestUnknownCommandLinesLocalizeLeadAndPointer(t *testing.T) {
	freshI18n(t)
	lead, pointer := unknownCommandLines(context.Background(), "/modle", []string{"/model", "/help"})
	hint := " " + tr("cli.command.did_you_mean_one", map[string]any{"suggestion": "/model"})
	if lead != tr("cli.command.unknown_nothing_sent", map[string]any{"command": "/modle", "hint": hint}) || pointer != tr("cli.command.type_help", nil) {
		t.Fatalf("lead %q pointer %q", lead, pointer)
	}
}

// A banner test CLI with the agent's context window the case sets
func lowContextCLI(t *testing.T, contextLength int, columns string) (*CLI, *printRecorder) {
	t.Helper()
	c, rec := infoCLI(t)
	t.Setenv("COLUMNS", columns)
	c.model, c.enabledToolsets, c.compact, c.apiKey, c.baseURL, c.provider = "test-model", []string{"icebath-core"}, false, "test", "", "test"
	c.setAgent(&GatewayAgent{ctx: context.Background()})
	c.env = &tools.Env{Home: home.Dir(c.ctx)}
	swap(t, &readAgentUsage, func(*CLI, *GatewayAgent) agentUsageReading {
		return agentUsageReading{HasCompressor: true, ContextLength: contextLength}
	})
	swap(t, &toolDefinitionsFor, func(*CLI, bool) []map[string]any { return []map[string]any{} })
	swap(t, &welcomeBannerPrinter, func(context.Context, *pyrich.Console, welcomeBanner) error { return nil })
	return c, rec
}

func TestLowContextWarning(t *testing.T) {
	minimum := provider.MinimumContextLength
	t.Run("test_warning_for_below_minimum_context", func(t *testing.T) {
		c, rec := lowContextCLI(t, 32768, "500")
		c.showBanner()
		calls := rec.plain()
		if len(linesWith(calls, "too low")) != 1 || len(linesWith(calls, "64,000")) == 0 {
			t.Fatalf("calls %q", calls)
		}
	})
	t.Run("test_no_warning_at_boundary", func(t *testing.T) {
		c, rec := lowContextCLI(t, minimum, "500")
		c.showBanner()
		if n := len(linesWith(rec.plain(), "too low")); n != 0 {
			t.Fatalf("%d warnings", n)
		}
	})
	t.Run("test_ollama_specific_hint", func(t *testing.T) {
		c, rec := lowContextCLI(t, 4096, "500")
		c.baseURL = "http://localhost:11434/v1"
		c.showBanner()
		hints := linesWith(rec.plain(), "OLLAMA_CONTEXT_LENGTH")
		if len(hints) != 1 || !strings.Contains(hints[0], "64000") {
			t.Fatalf("hints %q", hints)
		}
	})
	t.Run("test_generic_hint_for_other_servers", func(t *testing.T) {
		c, rec := lowContextCLI(t, 4096, "500")
		c.baseURL = "http://localhost:8080/v1"
		c.showBanner()
		if hints := linesWith(rec.plain(), "config.yaml"); len(hints) != 1 {
			t.Fatalf("hints %q", hints)
		}
	})
	t.Run("test_compact_banner_does_not_crash_on_narrow_terminal", func(t *testing.T) {
		c, rec := lowContextCLI(t, 4096, "70")
		c.showBanner()
		if n := len(linesWith(rec.plain(), "too low")); n != 1 {
			t.Fatalf("%d warnings in %q", n, rec.plain())
		}
	})
}

// A watcher test CLI over the home config.yaml with the reload call counted
func watchCLI(t *testing.T, servers map[string]any, extra map[string]any) (*CLI, string, *atomic.Int32, *printRecorder) {
	t.Helper()
	c, rec := infoCLI(t)
	cfg := map[string]any{"mcp_servers": servers}
	if servers == nil {
		cfg["mcp_servers"] = map[string]any{}
		servers = map[string]any{}
	}
	for k, v := range extra {
		cfg[k] = v
	}
	c.config = cfg
	c.tui.lastConfigCheck = 0
	c.tui.configMCPServers = servers
	path := home.ConfigPath(home.Dir(c.ctx))
	writeFile(t, path, "mcp_servers: {}\n")
	sig, err := config.ConfigVersionOrNone(c.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c.tui.configSig = sig
	calls := &atomic.Int32{}
	c.reloadMCPHook = func() { calls.Add(1) }
	return c, path, calls, rec
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Waits for the background reload the watcher started
func waitCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d reloads, saw %d", want, calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != want {
		t.Fatalf("reload called %d times, want %d", got, want)
	}
}

func TestMCPConfigWatch(t *testing.T) {
	github := map[string]any{"github": map[string]any{"url": "https://mcp.github.com"}}
	t.Run("test_new_mcp_server_triggers_reload", func(t *testing.T) {
		c, path, calls, _ := watchCLI(t, map[string]any{}, nil)
		writeFile(t, path, "mcp_servers:\n  github:\n    url: https://mcp.github.com\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		waitCalls(t, calls, 1)
	})
	t.Run("test_removed_mcp_server_triggers_reload", func(t *testing.T) {
		c, path, calls, _ := watchCLI(t, github, nil)
		writeFile(t, path, "mcp_servers: {}\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		waitCalls(t, calls, 1)
	})
	t.Run("test_optout_disables_auto_reload", func(t *testing.T) {
		c, path, calls, rec := watchCLI(t, map[string]any{}, nil)
		writeFile(t, path, "mcp:\n  auto_reload_on_config_change: false\nmcp_servers:\n  github:\n    url: https://mcp.github.com\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		if calls.Load() != 0 || !strings.Contains(strings.Join(rec.plain(), "\n"), "/reload-mcp") {
			t.Fatalf("calls %d out %q", calls.Load(), rec.plain())
		}
	})
	t.Run("test_optout_updates_snapshot_so_reload_mcp_applies_cleanly", func(t *testing.T) {
		c, path, calls, _ := watchCLI(t, map[string]any{}, nil)
		writeFile(t, path, "mcp:\n  auto_reload_on_config_change: false\nmcp_servers:\n  github:\n    url: https://mcp.github.com\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		c.tui.lastConfigCheck = 0
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		if calls.Load() != 0 || !reflect.DeepEqual(c.tui.configMCPServers, github) {
			t.Fatalf("calls %d snapshot %#v", calls.Load(), c.tui.configMCPServers)
		}
	})
	t.Run("test_optout_path_is_top_level_mcp_not_auxiliary", func(t *testing.T) {
		c, path, calls, _ := watchCLI(t, map[string]any{}, nil)
		writeFile(t, path, "auxiliary:\n  mcp:\n    auto_reload_on_config_change: false\nmcp_servers:\n  github:\n    url: https://mcp.github.com\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		waitCalls(t, calls, 1)
	})
	t.Run("test_env_var_templates_do_not_false_positive_on_unrelated_saves", func(t *testing.T) {
		t.Setenv("MCP_GH_API_KEY", "sekrit-token")
		expanded := map[string]any{"github": map[string]any{"url": "https://mcp.github.com", "headers": map[string]any{"Authorization": "Bearer sekrit-token"}}}
		c, path, calls, rec := watchCLI(t, expanded, nil)
		writeFile(t, path, "agent:\n  reasoning_effort: high\nmcp_servers:\n  github:\n    headers:\n      Authorization: Bearer ${MCP_GH_API_KEY}\n    url: https://mcp.github.com\n")
		c.tui.configSig = nil
		c.checkConfigMCPChanges()
		if calls.Load() != 0 || strings.Contains(strings.Join(rec.plain(), "\n"), "MCP server config changed") {
			t.Fatalf("calls %d out %q", calls.Load(), rec.plain())
		}
	})
}

// Blocks until the filesystem timestamp clock of dir passes afterNs
func waitForCtimeTick(t *testing.T, dir string, afterNs int64) {
	t.Helper()
	probe := filepath.Join(dir, ".ctime-probe")
	deadline := time.Now().Add(10 * time.Second)
	for {
		writeFile(t, probe, "")
		if ns := statCtimeNs(t, probe); ns > afterNs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ctime clock never advanced")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPinnedMtimeSameSizeReplacementTriggersReload(t *testing.T) {
	c, path, calls, _ := watchCLI(t, map[string]any{"bb": map[string]any{"command": "b"}}, nil)
	writeFile(t, path, "mcp_servers:\n  bb: {command: b}\n")
	sig, err := config.ConfigVersionOrNone(c.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c.tui.configSig = sig
	waitForCtimeTick(t, filepath.Dir(path), statCtimeNs(t, path))
	other := filepath.Join(filepath.Dir(path), "other.yaml")
	writeFile(t, other, "mcp_servers:\n  aa: {command: a}\n")
	data, _ := os.ReadFile(other)
	writeFile(t, path, string(data))
	mtime := time.Unix(0, sig[0].(int64))
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	c.checkConfigMCPChanges()
	waitCalls(t, calls, 1)
	if !reflect.DeepEqual(c.tui.configMCPServers, map[string]any{"aa": map[string]any{"command": "a"}}) {
		t.Fatalf("snapshot %#v", c.tui.configMCPServers)
	}
}

func TestTuiInitRunStateSeedsConfigSigWhenConfigExists(t *testing.T) {
	t.Setenv("ICEBATH_DEFER_AGENT_STARTUP", "1")
	c, _ := infoCLI(t)
	path := home.ConfigPath(home.Dir(c.ctx))
	writeFile(t, path, "mcp_servers: {}\n")
	c.config = map[string]any{"mcp_servers": map[string]any{}}
	c.pendingInput, c.interruptQueue = &inputQueue{}, &inputQueue{}
	c.tuiInitRunState()
	want, err := config.ConfigVersionOrNone(c.ctx, path)
	if err != nil || want == nil || !reflect.DeepEqual(c.tui.configSig, want) {
		t.Fatalf("sig %v want %v err %v", c.tui.configSig, want, err)
	}
}

func TestReloadSkillsCLI(t *testing.T) {
	t.Run("test_reports_added_and_removed_and_queues_note", func(t *testing.T) {
		c, rec := infoCLI(t)
		c.conversationHistory = []map[string]any{}
		swap(t, &reloadSkillsFor, func(*CLI) (map[string]any, error) {
			return map[string]any{"added": []any{map[string]any{"name": "alpha", "description": "Run alpha to do xyz"},
				map[string]any{"name": "beta", "description": "Run beta to do abc"}},
				"removed":   []any{map[string]any{"name": "gamma", "description": "Old removed skill"}},
				"unchanged": []any{"delta"}, "total": 3, "commands": 3}, nil
		})
		c.reloadSkills()
		out := strings.Join(rec.plain(), "\n")
		for _, name := range []string{"alpha", "beta", "gamma"} {
			if !strings.Contains(out, name) || !strings.Contains(c.pendingSkillsReloadNote, name) {
				t.Fatalf("%s missing from output %q or note %q", name, out, c.pendingSkillsReloadNote)
			}
		}
		if len(c.conversationHistory) != 0 {
			t.Fatal("history changed")
		}
	})
	t.Run("test_reports_no_changes_and_queues_nothing", func(t *testing.T) {
		c, _ := infoCLI(t)
		c.conversationHistory = []map[string]any{}
		swap(t, &reloadSkillsFor, func(*CLI) (map[string]any, error) {
			return map[string]any{"added": []any{}, "removed": []any{}, "unchanged": []any{"alpha"}, "total": 1, "commands": 1}, nil
		})
		c.reloadSkills()
		if len(c.conversationHistory) != 0 || c.pendingSkillsReloadNote != "" {
			t.Fatal("no change still queued a note or touched history")
		}
	})
	t.Run("test_handles_reload_failure_gracefully", func(t *testing.T) {
		c, rec := infoCLI(t)
		c.conversationHistory = []map[string]any{}
		swap(t, &reloadSkillsFor, func(*CLI) (map[string]any, error) { return nil, errors.New("boom") })
		c.reloadSkills()
		if !strings.Contains(strings.Join(rec.plain(), "\n"), "boom") || len(c.conversationHistory) != 0 || c.pendingSkillsReloadNote != "" {
			t.Fatalf("out %q", rec.plain())
		}
	})
}

// A real session store at the home state.db with its bytes before any read
func insightsStore(t *testing.T) (string, []byte) {
	t.Helper()
	path := home.StatePath(home.Dir(context.Background()))
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, before
}

// Asserts the read left the store bytes alone
func assertReadOnly(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("insights mutated state.db")
	}
}

func TestCliInsightsAcceptsPositionalDays(t *testing.T) {
	c, rec := infoCLI(t)
	path, before := insightsStore(t)
	c.showInsights("/insights 7")
	if out := strings.Join(rec.plain(), "\n"); !strings.Contains(out, "No sessions found in the last 7 days.") {
		t.Fatalf("out %q", out)
	}
	assertReadOnly(t, path, before)
}

func TestCliInsightsKeepsDaysFlagAndSource(t *testing.T) {
	c, rec := infoCLI(t)
	path, before := insightsStore(t)
	c.showInsights("/insights --days 14 --source discord")
	if out := strings.Join(rec.plain(), "\n"); !strings.Contains(out, "No sessions found in the last 14 days (source: discord).") {
		t.Fatalf("out %q", out)
	}
	assertReadOnly(t, path, before)
}

func TestInsightsSkipsOpenWhenStoreMissing(t *testing.T) {
	c, rec := infoCLI(t)
	path := home.StatePath(home.Dir(c.ctx))
	c.showInsights("/insights 7")
	memory.CmdInsights(c.ctx, console().Out, 30, "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("insights opened a missing store")
	}
	if n := len(linesWith(rec.plain(), "No session data yet.")); n != 2 {
		t.Fatalf("out %q", rec.plain())
	}
}

func TestShowInsightsOpensReadOnly(t *testing.T) {
	c, rec := infoCLI(t)
	path, before := insightsStore(t)
	c.showInsights("/insights 7")
	held := len(openFDsUnder(t, path))
	c.showInsights("/insights 7")
	if strings.Contains(strings.Join(rec.plain(), "\n"), "Error generating insights:") {
		t.Fatalf("out %q", rec.plain())
	}
	if open := openFDsUnder(t, path); len(open) > 1 || len(open) != held {
		t.Fatalf("descriptors left open %v", open)
	}
	assertReadOnly(t, path, before)
}

// Open descriptors of this process on the store, at most the never closed header probe of pkg/state
func openFDsUnder(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd on this platform")
	}
	var open []string
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && strings.HasPrefix(target, path) {
			open = append(open, target)
		}
	}
	return open
}

func TestBackgroundInlineDetector(t *testing.T) {
	running := func(t *testing.T) *CLI {
		c, _ := infoCLI(t)
		c.agentRunning = true
		return c
	}
	t.Run("test_detects_both_commands", func(t *testing.T) {
		c := running(t)
		if !c.shouldHandleBackgroundCommandInline("/bg do work", false) || !c.shouldHandleBackgroundCommandInline("/btw do work", false) {
			t.Fatal("bg and btw dispatch inline")
		}
	})
	t.Run("test_ignores_background_when_agent_idle", func(t *testing.T) {
		c, _ := infoCLI(t)
		if c.shouldHandleBackgroundCommandInline("/bg do work", false) {
			t.Fatal("idle input dispatched inline")
		}
	})
	t.Run("test_ignores_non_slash_input", func(t *testing.T) {
		c := running(t)
		if c.shouldHandleBackgroundCommandInline("bg without slash", false) || c.shouldHandleBackgroundCommandInline("", false) {
			t.Fatal("non slash input dispatched inline")
		}
	})
	t.Run("test_ignores_other_slash_commands", func(t *testing.T) {
		c := running(t)
		for _, text := range []string{"/steer hello", "/queue hello", "/stop"} {
			if c.shouldHandleBackgroundCommandInline(text, false) {
				t.Fatalf("%q dispatched as background", text)
			}
		}
	})
	t.Run("test_ignores_background_with_attached_images", func(t *testing.T) {
		if running(t).shouldHandleBackgroundCommandInline("/bg look at this", true) {
			t.Fatal("image payload dispatched inline")
		}
	})
	t.Run("test_case_and_whitespace_tolerant", func(t *testing.T) {
		if !running(t).shouldHandleBackgroundCommandInline("/BG do work", false) {
			t.Fatal("upper case bg not detected")
		}
	})
}

func TestBackgroundBusyPolicyContract(t *testing.T) {
	t.Run("test_bg_and_btw_declare_dispatch_while_busy", func(t *testing.T) {
		for _, name := range []string{"bg", "btw"} {
			cmd := commands.Resolve(name)
			if cmd == nil || cmd.Name != name || cmd.BusyPolicy != commands.BusyDispatch {
				t.Fatalf("%s resolves to %+v", name, cmd)
			}
		}
	})
	t.Run("test_background_name_is_retired", func(t *testing.T) {
		if commands.Resolve("background") != nil {
			t.Fatal("background still resolves")
		}
	})
}

// A bang handler test CLI with the seeded history
func bangCLI(t *testing.T, history []map[string]any) (*CLI, *printRecorder) {
	t.Helper()
	for _, v := range []string{"ICEBATH_GATEWAY_SESSION", "ICEBATH_CRON_SESSION", "ICEBATH_SESSION_PLATFORM"} {
		t.Setenv(v, "")
	}
	c, rec := infoCLI(t)
	c.sessionID = "test-session"
	c.conversationHistory = history
	if history == nil {
		c.conversationHistory = []map[string]any{}
	}
	return c, rec
}

// Counts bang runner calls, answering exit code 0
func countRunner(t *testing.T) *int {
	t.Helper()
	calls := new(int)
	swap(t, &runBangCommand, func(context.Context, *tools.Env, string, string, int, func(string)) int {
		*calls++
		return 0
	})
	return calls
}

// Records the commands the approval gate saw and answers with the verdict
func fakeGate(t *testing.T, verdict tools.GuardResult) *[]string {
	t.Helper()
	seen := &[]string{}
	swap(t, &checkBangApproval, func(_ context.Context, _ *tools.Env, command string) tools.GuardResult {
		*seen = append(*seen, command)
		return verdict
	})
	return seen
}

func TestBangHandlerDispatch(t *testing.T) {
	t.Run("test_non_bang_text_is_not_handled", func(t *testing.T) {
		c, _ := bangCLI(t, nil)
		if c.handleBangShell("please fix this bug!") || c.handleBangShell("/help") {
			t.Fatal("non bang text handled")
		}
	})
	t.Run("test_bare_bang_prints_usage_and_runs_nothing", func(t *testing.T) {
		c, rec := bangCLI(t, nil)
		calls := countRunner(t)
		if !c.handleBangShell("!") || *calls != 0 || len(linesWith(rec.plain(), commands.BangUsageHint)) == 0 {
			t.Fatalf("runner calls %d out %q", *calls, rec.plain())
		}
	})
	t.Run("test_command_output_is_printed", func(t *testing.T) {
		c, rec := bangCLI(t, nil)
		if !c.handleBangShell("!echo hello-bang") || len(linesWith(rec.plain(), "hello-bang")) == 0 {
			t.Fatalf("out %q", rec.plain())
		}
	})
	t.Run("test_nonzero_exit_is_surfaced_to_the_user", func(t *testing.T) {
		c, rec := bangCLI(t, nil)
		if !c.handleBangShell("!exit 3") || len(linesWith(rec.plain(), "exited 3")) == 0 {
			t.Fatalf("out %q", rec.plain())
		}
	})
	t.Run("test_disabled_context_falls_through", func(t *testing.T) {
		c, _ := bangCLI(t, nil)
		t.Setenv("ICEBATH_GATEWAY_SESSION", "1")
		calls := countRunner(t)
		if c.handleBangShell("!echo nope") || *calls != 0 {
			t.Fatal("gateway session ran a bang command")
		}
	})
}

func TestBangApprovalGate(t *testing.T) {
	t.Run("test_approval_gate_is_invoked_for_a_dangerous_command", func(t *testing.T) {
		c, _ := bangCLI(t, nil)
		seen := fakeGate(t, tools.GuardResult{Approved: true})
		countRunner(t)
		c.handleBangShell("!rm -rf ./build")
		if len(*seen) != 1 || (*seen)[0] != "rm -rf ./build" {
			t.Fatalf("gate saw %q", *seen)
		}
	})
	t.Run("test_gate_is_invoked_for_every_command_not_just_dangerous_ones", func(t *testing.T) {
		c, _ := bangCLI(t, nil)
		seen := fakeGate(t, tools.GuardResult{Approved: true})
		countRunner(t)
		c.handleBangShell("!ls")
		if len(*seen) != 1 {
			t.Fatalf("gate saw %q", *seen)
		}
	})
	t.Run("test_denied_command_is_not_executed", func(t *testing.T) {
		c, rec := bangCLI(t, nil)
		fakeGate(t, tools.GuardResult{Approved: false, Message: "Command denied: recursive delete"})
		calls := countRunner(t)
		if !c.handleBangShell("!rm -rf /important") || *calls != 0 || len(linesWith(rec.plain(), "denied")) == 0 {
			t.Fatalf("runner calls %d out %q", *calls, rec.plain())
		}
	})
	t.Run("test_real_gate_blocks_a_hardline_command", func(t *testing.T) {
		c, _ := bangCLI(t, nil)
		calls := countRunner(t)
		if !c.handleBangShell("!rm -rf /") || *calls != 0 {
			t.Fatalf("hardline command ran, runner calls %d", *calls)
		}
	})
}

func seedHistory() []map[string]any {
	return []map[string]any{
		{"role": "system", "content": "You are Icebath."},
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "Hi there."},
		{"role": "user", "content": "list the files"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": `{"command": "ls"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "a.py b.py"},
	}
}

func historyJSON(t *testing.T, h []map[string]any) string {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBangLeavesHistoryByteIdentical(t *testing.T) {
	t.Run("test_history_is_byte_identical_before_and_after", func(t *testing.T) {
		for _, submission := range []string{"!echo history-check", "!exit 7", "!", "!definitely-not-a-real-binary-xyz"} {
			c, _ := bangCLI(t, seedHistory())
			before := historyJSON(t, c.conversationHistory)
			c.handleBangShell(submission)
			if historyJSON(t, c.conversationHistory) != before || len(c.conversationHistory) != len(seedHistory()) {
				t.Fatalf("bang command %q mutated conversation history", submission)
			}
		}
	})
	t.Run("test_history_unchanged_when_command_is_denied", func(t *testing.T) {
		c, _ := bangCLI(t, seedHistory())
		before := historyJSON(t, c.conversationHistory)
		fakeGate(t, tools.GuardResult{Approved: false, Message: "nope"})
		c.handleBangShell("!rm -rf /important")
		if historyJSON(t, c.conversationHistory) != before {
			t.Fatal("denied bang command mutated history")
		}
	})
	t.Run("test_agent_is_never_invoked", func(t *testing.T) {
		c, _ := bangCLI(t, seedHistory())
		c.setAgent(&GatewayAgent{ctx: context.Background()})
		c.handleBangShell("!echo no-model-turn")
		if !reflect.DeepEqual(c.agentRef(), &GatewayAgent{ctx: context.Background()}) || historyJSON(t, c.conversationHistory) != historyJSON(t, seedHistory()) {
			t.Fatal("bang command touched the agent or the history")
		}
	})
}

func TestCLIUsageNoAgentAccountLimits(t *testing.T) {
	noAgentCLI := func(t *testing.T, snapshot bool) (*CLI, *printRecorder, *string) {
		c, rec := infoCLI(t)
		c.provider, c.baseURL, c.apiKey = "openai-codex", "", ""
		seen := new(string)
		swap(t, &fetchAccountUsage, func(_ context.Context, providerName string, _ provider.AccountUsageFetchOptions) *provider.AccountUsageSnapshot {
			*seen = providerName
			if !snapshot {
				return nil
			}
			used := 1.0
			return &provider.AccountUsageSnapshot{Provider: providerName, Source: "usage_api", FetchedAt: time.Now(), Title: "Account limits", Plan: "Pro",
				Windows: []provider.AccountUsageWindow{{Label: "Weekly", UsedPercent: &used}}}
		})
		return c, rec, seen
	}
	t.Run("test_no_agent_prints_codex_account_limits", func(t *testing.T) {
		c, rec, seen := noAgentCLI(t, true)
		c.showUsage()
		out := strings.Join(rec.plain(), "\n")
		if *seen != "openai-codex" || !strings.Contains(out, "Account limits") || !strings.Contains(out, "Weekly: 99% remaining (1% used)") || strings.Contains(out, "No active agent") {
			t.Fatalf("provider %q out %q", *seen, out)
		}
	})
	t.Run("test_no_agent_keeps_fallback_message_when_limits_unavailable", func(t *testing.T) {
		c, rec, _ := noAgentCLI(t, false)
		c.showUsage()
		out := strings.Join(rec.plain(), "\n")
		if !strings.Contains(out, "No active agent") || strings.Contains(out, "Account limits") {
			t.Fatalf("out %q", out)
		}
	})
}

func TestCommandRegistrationVerboseCycleReleasesFocusView(t *testing.T) {
	c, _ := infoCLI(t)
	c.toolProgressMode, c.focusViewEnabled, c.focusSavedToolProgress, c.focusHiddenLines, c.focusLastCountedTool = "off", true, "all", 3, "terminal"
	c.toggleVerbose()
	if c.focusViewEnabled || c.focusSavedToolProgress != "" || c.focusHiddenLines != 0 || c.toolProgressMode != "new" {
		t.Fatalf("focus %v saved %q hidden %d mode %q", c.focusViewEnabled, c.focusSavedToolProgress, c.focusHiddenLines, c.toolProgressMode)
	}
}

func TestFastModeRoutingFastCommandExposedForModelEvenWhenProviderIsAuto(t *testing.T) {
	c := bareCLI()
	c.provider, c.requestedProvider, c.model = "auto", "auto", "gpt-5.4"
	if !c.fastCommandAvailable() {
		t.Fatal("/fast hidden for gpt-5.4 on auto")
	}
}

func TestAnthropicFastModeFastCommandHiddenForAnthropicSonnet(t *testing.T) {
	c := bareCLI()
	c.provider, c.requestedProvider, c.model = "anthropic", "anthropic", "claude-sonnet-4-6"
	if c.fastCommandAvailable() {
		t.Fatal("/fast shown for sonnet")
	}
}
