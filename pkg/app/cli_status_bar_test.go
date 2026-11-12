package app

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// A CLI built without its constructor whose session started fourteen and a half minutes ago
func statusBarCLI(t *testing.T, fields []any) *CLI {
	t.Helper()
	recordPrints(t)
	display := map[string]any{}
	if fields != nil {
		display["status_bar"] = map[string]any{"fields": fields}
	}
	SetCLIConfig(map[string]any{"display": display})
	t.Cleanup(func() { SetCLIConfig(nil) })
	c := bareCLI()
	c.model = "anthropic/claude-sonnet-4-20250514"
	c.sessionStart = time.Now().Add(-(14*time.Minute + 32*time.Second))
	c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}}
	return c
}

type usageSpec struct {
	prompt, completion, total, calls, contextTokens, contextLength, compressions, cacheRead, cacheWrite int
	latencies                                                                                           []float64
	outputs                                                                                             []int
}

// Attaches an agent whose status bar usage the test owns and may change between repaints
func attachUsage(t *testing.T, c *CLI, spec *usageSpec) {
	t.Helper()
	c.agent = &GatewayAgent{ctx: context.Background()}
	prev := statusBarUsage
	statusBarUsage = func(*CLI, cliAgent) agent.StatusBarUsage {
		usage := agent.StatusBarUsage{Counters: map[string]int{
			"session_input_tokens": spec.prompt, "session_output_tokens": spec.completion, "session_cache_read_tokens": spec.cacheRead,
			"session_cache_write_tokens": spec.cacheWrite, "session_prompt_tokens": spec.prompt, "session_completion_tokens": spec.completion,
			"session_total_tokens": spec.total, "session_api_calls": spec.calls},
			HasCompressor: true, ContextTokens: spec.contextTokens, ContextLength: spec.contextLength, Compressions: spec.compressions,
			LatencyHistory: spec.latencies, OutputHistory: spec.outputs}
		if spec.contextLength != 0 {
			pct := max(0, min(100, int(math.RoundToEven(float64(spec.contextTokens)/float64(spec.contextLength)*100))))
			usage.ContextPercent = &pct
		}
		return usage
	}
	t.Cleanup(func() { statusBarUsage = prev })
}

func baseUsage() *usageSpec {
	return &usageSpec{prompt: 10_230, completion: 2_220, total: 12_450, calls: 7, contextTokens: 12_450, contextLength: 200_000}
}

func cacheUsage(read, write int) *usageSpec {
	return &usageSpec{prompt: 10_000, completion: 2_000, total: 12_000, calls: 5, contextTokens: 12_000, contextLength: 200_000, cacheRead: read, cacheWrite: write}
}

func barText(c *CLI, width int) string { return c.buildStatusBarText(&width) }

func fixWidth(t *testing.T, width int) {
	t.Helper()
	prev := tuiTerminalWidthFor
	tuiTerminalWidthFor = func(*CLI) int { return width }
	t.Cleanup(func() { tuiTerminalWidthFor = prev })
}

func TestCLIStatusBar(t *testing.T) {
	t.Run("test_session_title_is_right_aligned_after_it_is_queued", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		c.pendingTitle = "weekly-digest"
		text := barText(c, 80)
		if !strings.HasSuffix(text, " weekly-digest ") || statusBarDisplayWidth(text) != 80 {
			t.Fatalf("text %q width %d", text, statusBarDisplayWidth(text))
		}
	})
	t.Run("test_snapshot_refreshes_persisted_session_title", func(t *testing.T) {
		rec := recordPrints(t)
		_ = rec
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		if c.sessionDB == nil {
			t.Fatal("the configured CLI has no session store")
		}
		writer, err := state.NewSessionDB(c.ctx, home.StatePath(home.Dir(c.ctx)), false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { writer.Close() })
		if err := writer.EnsureSession(state.NewSession{ID: c.sessionID, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.SetSessionTitle(c.ctx, c.sessionID, "user-profiles"); err != nil {
			t.Fatal(err)
		}
		if got := c.getStatusBarSnapshot()["session_title"]; got != "user-profiles" {
			t.Fatalf("snapshot title %q", got)
		}
	})
	t.Run("test_build_status_bar_text_for_wide_terminal", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, baseUsage())
		text := barText(c, 120)
		for _, want := range []string{"claude-sonnet-4-20250514", "12.4K/200K", "6%", "15m"} {
			if !strings.Contains(text, want) {
				t.Fatalf("text %q lacks %q", text, want)
			}
		}
		if strings.Contains(text, "$0.06") {
			t.Fatalf("cost leaked into %q", text)
		}
	})
	t.Run("test_compression_count_shown_in_wide_status_bar", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		spec := baseUsage()
		spec.compressions = 3
		attachUsage(t, c, spec)
		if text := barText(c, 120); !strings.Contains(text, "🗜️ 3") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_spinner_height_uses_display_width_for_wide_characters", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		c.spinnerText = strings.Repeat("你", 40)
		width := 64
		if got := c.spinnerWidgetHeightAt(&width); got != 2 {
			t.Fatalf("height %d", got)
		}
	})
	t.Run("test_voice_status_bar_renders_configured_ctrl_letter", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		c.voiceMode = true
		c.setVoiceRecordKeyCache("ctrl+o")
		wide, narrow := 120, 50
		if text := fragmentText(c.getVoiceStatusFragmentsAt(&wide)); !strings.Contains(text, "Ctrl+O to record") {
			t.Fatalf("wide voice bar %q", text)
		}
		compact := c.getVoiceStatusFragmentsAt(&narrow)
		if len(compact) != 1 || compact[0] != (styledFragment{"class:voice-status", " 🎤 Ctrl+O "}) {
			t.Fatalf("compact voice bar %v", compact)
		}
	})
}

func TestStatusBarWidthSource(t *testing.T) {
	wide := func(t *testing.T) *CLI {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, &usageSpec{prompt: 100_000, completion: 5_000, total: 105_000, calls: 20, contextTokens: 100_000, contextLength: 200_000})
		c.statusBarVisible = true
		return c
	}
	t.Run("test_fragments_fit_within_announced_width", func(t *testing.T) {
		c := wide(t)
		for _, width := range []int{40, 52, 76, 80, 120, 200} {
			fixWidth(t, width)
			text := fragmentText(c.getStatusBarFragments())
			if got := statusBarDisplayWidth(text); got > width+4 {
				t.Fatalf("at width %d the fragments take %d cells (%q)", width, got, text)
			}
		}
	})
	t.Run("test_fragments_put_session_title_at_far_right", func(t *testing.T) {
		c := wide(t)
		c.pendingTitle = "weekly-digest"
		fixWidth(t, 100)
		text := fragmentText(c.getStatusBarFragments())
		if !strings.HasSuffix(text, " weekly-digest ") || statusBarDisplayWidth(text) != 100 {
			t.Fatalf("text %q width %d", text, statusBarDisplayWidth(text))
		}
	})
}

func TestIdleSinceLastTurn(t *testing.T) {
	t.Run("test_hidden_before_first_turn", func(t *testing.T) {
		if formatIdleSince(nil, false) != "" {
			t.Fatal("no turn has finished yet")
		}
	})
	t.Run("test_hidden_while_turn_is_live", func(t *testing.T) {
		at := wallClock() - 30
		if formatIdleSince(&at, true) != "" {
			t.Fatal("a live turn hides the idle time")
		}
	})
	t.Run("test_shows_compact_idle_time_after_turn", func(t *testing.T) {
		at := wallClock() - 42
		if got := formatIdleSince(&at, false); got != "✓ 42s" {
			t.Fatalf("label %q", got)
		}
	})
}

func TestStatusBarFieldConfig(t *testing.T) {
	withFields := func(t *testing.T, fields []any) string {
		c := statusBarCLI(t, fields)
		spec := baseUsage()
		spec.compressions = 7
		attachUsage(t, c, spec)
		return barText(c, 120)
	}
	t.Run("test_only_model_and_duration", func(t *testing.T) {
		text := withFields(t, []any{"model", "duration"})
		if !strings.Contains(text, "claude-sonnet-4-20250514") || !strings.Contains(text, "15m") ||
			strings.Contains(text, "12.4K/200K") || strings.Contains(text, "🗜️") || strings.Contains(text, "%") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_total_tokens_when_explicitly_requested", func(t *testing.T) {
		if text := withFields(t, []any{"model", "total_tokens"}); !strings.Contains(text, "Σ12.4K") || !strings.Contains(text, "claude-sonnet-4-20250514") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_field_config_never_empties_the_bar", func(t *testing.T) {
		if text := withFields(t, []any{"nonexistent_field"}); !strings.Contains(text, "claude-sonnet-4-20250514") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_fragments_respect_field_config", func(t *testing.T) {
		c := statusBarCLI(t, []any{"model", "duration"})
		spec := baseUsage()
		spec.compressions = 7
		attachUsage(t, c, spec)
		c.statusBarVisible = true
		fixWidth(t, 120)
		text := fragmentText(c.getStatusBarFragments())
		if !strings.Contains(text, "claude-sonnet-4-20250514") || !strings.Contains(text, "15m") || strings.Contains(text, "🗜️") || strings.Contains(text, "12.4K") {
			t.Fatalf("fragments %q", text)
		}
	})
	t.Run("test_empty_fields_list_uses_defaults", func(t *testing.T) {
		text := withFields(t, []any{})
		if !strings.Contains(text, "claude-sonnet-4-20250514") || !strings.Contains(text, "12.4K/200K") || !strings.Contains(text, "🗜️") {
			t.Fatalf("text %q", text)
		}
	})
}

func TestCacheHitRate(t *testing.T) {
	t.Run("test_cache_hit_rate_shown_in_wide_terminal", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, cacheUsage(7600, 0))
		if text := barText(c, 120); !strings.Contains(text, "◎ 76.0%") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_cache_hit_rate_hidden_when_zero", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, cacheUsage(0, 0))
		if text := barText(c, 120); strings.Contains(text, "◎") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_cache_hit_rate_with_anthropic_style_cache", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, cacheUsage(5000, 2000))
		if text := barText(c, 120); !strings.Contains(text, "◎ 50.0%") {
			t.Fatalf("text %q", text)
		}
	})
}

func TestRollingLatencyVelocity(t *testing.T) {
	t.Run("test_latency_and_tps_shown_in_wide_terminal", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		spec := cacheUsage(0, 0)
		spec.latencies, spec.outputs = []float64{2.0, 4.0}, []int{120, 180}
		attachUsage(t, c, spec)
		if text := barText(c, 140); !strings.Contains(text, "◷ 3.0s") || !strings.Contains(text, "↑ 50 t/s") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_latency_hidden_without_history", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		attachUsage(t, c, cacheUsage(0, 0))
		if text := barText(c, 140); strings.Contains(text, "◷") || strings.Contains(text, "t/s") {
			t.Fatalf("text %q", text)
		}
	})
	t.Run("test_negative_latency_guard", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		spec := cacheUsage(0, 0)
		spec.latencies, spec.outputs = []float64{-0.8}, []int{100}
		attachUsage(t, c, spec)
		snapshot := c.getStatusBarSnapshot()
		if snapshot["avg_latency"] != nil || snapshot["avg_velocity"] != nil {
			t.Fatalf("latency %v velocity %v", snapshot["avg_latency"], snapshot["avg_velocity"])
		}
	})
}

func TestCacheHitBaselineReset(t *testing.T) {
	t.Run("test_baseline_resets_on_model_switch", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		spec := cacheUsage(9_000, 0)
		attachUsage(t, c, spec)
		if got := c.getStatusBarSnapshot()["cache_hit_pct"]; got != 90.0 {
			t.Fatalf("first %v", got)
		}
		c.model = "openai/gpt-5"
		if got := c.getStatusBarSnapshot()["cache_hit_pct"]; got != nil {
			t.Fatalf("after the switch %v", got)
		}
		spec.prompt, spec.cacheRead = 12_000, 9_500
		if got := c.getStatusBarSnapshot()["cache_hit_pct"]; got != 25.0 {
			t.Fatalf("since the switch %v", got)
		}
	})
	t.Run("test_baseline_resets_on_compression", func(t *testing.T) {
		c := statusBarCLI(t, nil)
		spec := cacheUsage(8_000, 0)
		attachUsage(t, c, spec)
		c.getStatusBarSnapshot()
		spec.compressions = 1
		c.getStatusBarSnapshot()
		spec.prompt, spec.cacheRead = 14_000, 8_400
		if got := c.getStatusBarSnapshot()["cache_hit_pct"]; got != 10.0 {
			t.Fatalf("after the compression %v", got)
		}
	})
	t.Run("test_title_field_filter_hides_session_badge", func(t *testing.T) {
		c := statusBarCLI(t, []any{"model", "duration"})
		c.pendingTitle = "weekly-digest"
		if text := barText(c, 80); strings.Contains(text, "weekly-digest") {
			t.Fatalf("text %q", text)
		}
	})
}

func TestGitBranchSegmentIsOptIn(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	segmentText := func(c *CLI, fields map[string]bool) (string, any) {
		snapshot := c.getStatusBarSnapshot()
		var b strings.Builder
		for _, seg := range c.statusBarSegments(snapshot, 120, fields, false, false) {
			b.WriteString(fragmentText(seg))
		}
		return b.String(), snapshot["git_branch"]
	}
	c := statusBarCLI(t, nil)
	t.Chdir(repo)
	resetGitBranchCache()
	if text, branch := segmentText(c, nil); branch != "" || strings.Contains(text, "⎇") {
		t.Fatalf("the default field set showed the branch: %q %v", text, branch)
	}
	c2 := statusBarCLI(t, []any{"model", "git_branch"})
	t.Chdir(repo)
	resetGitBranchCache()
	if text, branch := segmentText(c2, map[string]bool{"model": true, "git_branch": true}); branch != "main" || !strings.Contains(text, "⎇ main") {
		t.Fatalf("the opted in branch segment: %q %v", text, branch)
	}
}

func TestStatusLineKeepsEveryClassicStatusBarField(t *testing.T) {
	c := statusBarCLI(t, nil)
	c.modernLayout, c.statusBarVisible = true, true
	snapshot := map[string]any{"model_short": "m", "duration": "6h 43m", "context_percent": 7, "context_length": 1_000_000,
		"context_tokens": 73_200, "context_estimated": true, "avg_latency_label": "9.0s",
		"avg_velocity_label": "61 t/s", "cache_hit_pct": 28.9, "cache_read_tokens": 1, "prompt_tokens": 1,
		"active_background_subagents": 3, "prompt_elapsed": "⏱ 10s"}
	prevSnapshot, prevHints, prevCwd := statusBarSnapshotFor, hintPairsFor, cwdLabelFor
	statusBarSnapshotFor = func(*CLI) map[string]any { return snapshot }
	hintPairsFor = func(*CLI) [][2]string { return nil }
	cwdLabelFor = func() string { return "~" }
	t.Cleanup(func() { statusBarSnapshotFor, hintPairsFor, cwdLabelFor = prevSnapshot, prevHints, prevCwd })
	fixWidth(t, 400)
	line := fragmentText(c.getStatusBarFragments())
	for _, piece := range []string{"73.2K/1M", "7%", "◎ 28.9%", "◷ 9.0s", "↑ 61 t/s", "⛓ 3", "6h 43m", "⏱ 10s"} {
		if !strings.Contains(line, piece) {
			t.Fatalf("the footer %q lacks %q", line, piece)
		}
	}
}

func TestStatusBarSegment(t *testing.T) {
	t.Run("test_segment_present_only_when_enabled", func(t *testing.T) {
		if focusStatusbarSegment(true) != FocusStatusbarLabel || focusStatusbarSegment(false) != "" {
			t.Fatal("the focus badge shows only while focus view is on")
		}
	})
	t.Run("test_text_renderer_includes_the_badge_at_every_width_tier", func(t *testing.T) {
		for _, width := range []int{40, 60, 120} {
			c := statusBarCLI(t, nil)
			c.model = "opus"
			c.focusViewEnabled = true
			snapshot := map[string]any{"model_name": "opus", "model_short": "opus", "duration": "1m", "context_percent": 12,
				"context_tokens": 1000, "context_length": 200000, "compressions": 0, "active_background_tasks": 0,
				"active_background_processes": 0, "active_background_subagents": 0, "battery_label": "", "battery_category": "dim",
				"focus_label": focusStatusbarSegment(true)}
			prev := statusBarSnapshotFor
			statusBarSnapshotFor = func(*CLI) map[string]any { return snapshot }
			text := barText(c, width)
			statusBarSnapshotFor = prev
			if !strings.Contains(text, FocusStatusbarLabel) {
				t.Fatalf("width %d bar %q lacks the focus badge", width, text)
			}
		}
	})
}
