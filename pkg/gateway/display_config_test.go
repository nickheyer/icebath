package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Registers a plugin platform entry with display overrides for the life of the test
func withDisplayEntry(t *testing.T, name, tier string, overrides map[string]any) {
	t.Helper()
	ctx := context.Background()
	Registry.register(&PlatformEntry{Name: name, Label: name, Source: "plugin", DisplayTier: tier, DisplayOverrides: overrides}, "")
	t.Cleanup(func() { Registry.unregister(ctx, name, "") })
}

func TestWinningSourceControlsModeAndIntent(t *testing.T) {
	withDisplayEntry(t, "slack", "", map[string]any{"tool_progress": "off"})
	ctx := context.Background()
	for i, c := range []struct {
		display map[string]any
		env     string
		mode    string
		intent  bool
	}{
		{map[string]any{}, "", "off", false},
		{map[string]any{}, "all", "all", true},
		{map[string]any{"tool_progress": nil}, "all", "all", true},
		{map[string]any{"platforms": map[string]any{"slack": map[string]any{"tool_progress": nil}}}, "off", "off", true},
		{map[string]any{"tool_progress_overrides": map[string]any{"slack": nil}}, "new", "new", true},
		{map[string]any{"tool_progress": false}, "all", "off", true},
		{map[string]any{"tool_progress": "all", "platforms": map[string]any{"slack": map[string]any{"tool_progress": nil}}}, "off", "all", true},
		{map[string]any{"tool_progress": "off", "tool_progress_overrides": map[string]any{"slack": "new"}}, "all", "new", true},
		{map[string]any{"tool_progress_overrides": map[string]any{"slack": "off"}, "platforms": map[string]any{"slack": map[string]any{"tool_progress": "all"}}}, "", "all", true},
	} {
		mode, intent := config.ResolveToolProgress(ctx, map[string]any{"display": c.display}, "slack", c.env)
		if mode != c.mode || intent != c.intent {
			t.Fatalf("case %d: %q %v", i, mode, intent)
		}
	}
}

func TestResolveDisplaySettingOrder(t *testing.T) {
	ctx := context.Background()
	check := func(cfg map[string]any, platform, setting string, want any) {
		t.Helper()
		if got := config.ResolveDisplaySetting(ctx, cfg, platform, setting, nil); got != want {
			t.Fatalf("%s %s: %v want %v", platform, setting, got, want)
		}
	}
	plats := func(p map[string]any) map[string]any { return map[string]any{"platforms": p} }
	disp := func(d map[string]any) map[string]any { return map[string]any{"display": d} }
	explicit := disp(map[string]any{"tool_progress": "all", "platforms": map[string]any{"telegram": map[string]any{"tool_progress": "verbose"}}})
	check(explicit, "telegram", "tool_progress", "verbose")
	check(disp(map[string]any{"tool_progress": "new", "platforms": map[string]any{}}), "telegram", "tool_progress", "new")
	only := disp(map[string]any{"tool_progress": "all", "platforms": map[string]any{"slack": map[string]any{"tool_progress": "off"}}})
	check(only, "slack", "tool_progress", "off")
	check(only, "telegram", "tool_progress", "all")
	legacy := disp(map[string]any{"tool_progress": "all", "tool_progress_overrides": map[string]any{"signal": "off", "telegram": "verbose"}})
	check(legacy, "signal", "tool_progress", "off")
	check(legacy, "telegram", "tool_progress", "verbose")
	check(disp(map[string]any{"tool_progress": false}), "telegram", "tool_progress", "off")
	generic := disp(plats(map[string]any{"whatsapp": map[string]any{
		"thinking_progress": "generic", "interim_assistant_messages": "generic", "long_running_notifications": "generic"}}))
	check(generic, "whatsapp", "thinking_progress", false)
	check(generic, "whatsapp", "interim_assistant_messages", false)
	check(generic, "whatsapp", "long_running_notifications", "generic")
	check(disp(plats(map[string]any{"whatsapp": map[string]any{"thinking_progress": "false"}})), "whatsapp", "thinking_progress", false)
	check(map[string]any{}, "webhook", "tool_progress", "off")
	check(map[string]any{}, "api_server", "tool_preview_length", 0)
	check(disp(map[string]any{"tool_preview_length": "12"}), "telegram", "tool_preview_length", 12)
	check(disp(map[string]any{"tool_preview_length": "x"}), "telegram", "tool_preview_length", 0)
	check(disp(map[string]any{"streaming": true}), "telegram", "streaming", nil)
	check(disp(map[string]any{"live_status": "all"}), "slack", "live_status", "full")
	check(disp(map[string]any{"reasoning_style": "Fancy"}), "slack", "reasoning_style", "code")
	withDisplayEntry(t, "mattermostx", "LOW", nil)
	check(map[string]any{}, "mattermostx", "tool_progress", "off")
}

func TestLocalDisplayConfigOverlaysTheLocalKey(t *testing.T) {
	display := map[string]any{
		"show_reasoning": false, "runtime_footer": map[string]any{"enabled": false, "fields": "x"},
		"platforms": map[string]any{"cli": map[string]any{"show_reasoning": "yes", "runtime_footer": map[string]any{"enabled": true}}},
	}
	got := config.LocalDisplayConfig(display)
	footer := got["runtime_footer"].(map[string]any)
	if got["show_reasoning"] != true || footer["enabled"] != true || footer["fields"] != "x" || display["show_reasoning"] != false {
		t.Fatalf("%v", got)
	}
	if len(config.LocalDisplayConfig("nope")) != 0 {
		t.Fatal("non mapping display")
	}
}

type wakeDouble struct {
	internal bool
	meta     map[string]any
	snapshot *bool
}

func (w wakeDouble) WakeInternal() bool           { return w.internal }
func (w wakeDouble) WakeMetadata() map[string]any { return w.meta }
func (w wakeDouble) WakeSourcePlatform() string   { return string(PlatformTelegram) }
func (w wakeDouble) WakeReplyMuted() (bool, bool) {
	if w.snapshot == nil {
		return false, false
	}
	return *w.snapshot, true
}

func TestWarningNotificationPolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	if !config.WarningNotificationsEnabled(ctx, "telegram", nil) {
		t.Fatal("default must show warnings")
	}
	off := map[string]any{"display": map[string]any{"platforms": map[string]any{"telegram": map[string]any{"suppress_warning_notifications": "on"}}}}
	if config.WarningNotificationsEnabled(ctx, "telegram", off) || !config.WarningNotificationsEnabled(ctx, "slack", off) {
		t.Fatal("platform opt out")
	}
	if !config.WarningNotificationsEnabled(ctx, "telegram", map[string]any{"display": map[string]any{"suppress_warning_notifications": "maybe"}}) {
		t.Fatal("unknown value opted in")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display:\n  suppress_warning_notifications: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if config.WarningNotificationsEnabled(ctx, "telegram", nil) || !config.WarningNotificationsEnabled(ctx, "telegram", map[string]any{}) {
		t.Fatal("profile config must decide only without a snapshot")
	}
	if !config.IsWarningStatus("warn", "x") || !config.IsWarningStatus("status", config.DiagnosticText("x")) || config.IsWarningStatus("status", "x") {
		t.Fatal("warning status")
	}
	if !config.IsDiagnosticNotice("info", "credits.band") || config.IsDiagnosticNotice("info", "") || !config.IsDiagnosticNotice("error", "") {
		t.Fatal("diagnostic notice")
	}
	diag := map[string]any{"notification_category": "diagnostic"}
	if len(config.DiagnosticMetadata(wakeDouble{internal: false, meta: diag})) != 0 || config.DiagnosticMetadata(wakeDouble{internal: true, meta: diag})["notification_category"] != "diagnostic" {
		t.Fatal("diagnostic metadata")
	}
	if !config.DiagnosticWakeMuted(ctx, wakeDouble{internal: true, meta: diag}, off) || config.DiagnosticWakeMuted(ctx, wakeDouble{internal: false, meta: diag}, off) {
		t.Fatal("wake mute")
	}
	no := false
	if config.DiagnosticWakeMuted(ctx, wakeDouble{internal: true, meta: diag, snapshot: &no}, off) {
		t.Fatal("snapshot must win")
	}
	ran := 0
	hiddenRan, _ := config.RenderNotification(ctx, func() error { ran++; return nil }, "telegram", true, off)
	shownRan, _ := config.RenderNotification(ctx, func() error { ran++; return nil }, "telegram", false, off)
	if hiddenRan || !shownRan || ran != 1 {
		t.Fatal("render gate")
	}
	presented, err := config.PresentNotification(ctx, func(context.Context) error { ran++; return nil }, "slack", true, off)
	if !presented || err != nil || ran != 2 {
		t.Fatal("present gate")
	}
}
