package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestRenderNotificationCallsRendererOnlyWhenVisible(t *testing.T) {
	for _, setting := range []any{nil, false, true} {
		for _, diagnostic := range []bool{false, true} {
			cfg := map[string]any{}
			if setting != nil {
				cfg = map[string]any{"display": map[string]any{"suppress_warning_notifications": setting}}
			}
			observed := []string{"producer ran"}
			presented, err := RenderNotification(context.Background(), func() error {
				observed = append(observed, "rendered")
				return nil
			}, "cli", diagnostic, cfg)
			expected := !(diagnostic && setting == true)
			if err != nil || presented != expected || len(observed) != map[bool]int{true: 2, false: 1}[expected] {
				t.Fatalf("setting=%v diagnostic=%v presented=%v err=%v observed=%v", setting, diagnostic, presented, err, observed)
			}
		}
	}
}

func TestRenderFailurePropagatesAndRequestedResultDoesNotReadPolicy(t *testing.T) {
	saved := warningPolicy
	warningPolicy = func(context.Context, string, map[string]any) bool {
		t.Fatal("must not consult policy for result")
		return false
	}
	t.Cleanup(func() { warningPolicy = saved })
	failure := errors.New("renderer failed")
	for _, run := range []func() (bool, error){
		func() (bool, error) {
			return RenderNotification(context.Background(), func() error { return failure }, "tui", false, nil)
		},
		func() (bool, error) {
			return PresentNotification(context.Background(), func(context.Context) error { return failure }, "tui", false, nil)
		},
	} {
		if _, err := run(); !errors.Is(err, failure) {
			t.Fatalf("renderer failure lost: %v", err)
		}
	}
}

func TestCanonicalPolicyHasNoRawSurfaceExemption(t *testing.T) {
	ctx := context.Background()
	for _, platform := range []string{"cli", "tui", "api_server", "slack", "telegram"} {
		if WarningNotificationsEnabled(ctx, platform, map[string]any{"display": map[string]any{"suppress_warning_notifications": true}}) {
			t.Fatalf("%s ignored the global suppression", platform)
		}
		if !WarningNotificationsEnabled(ctx, platform, map[string]any{"display": map[string]any{"warning_notifications": false}}) {
			t.Fatalf("%s honoured the unrecognized key", platform)
		}
		override := map[string]any{"display": map[string]any{"suppress_warning_notifications": true,
			"platforms": map[string]any{platform: map[string]any{"suppress_warning_notifications": false}}}}
		if !WarningNotificationsEnabled(ctx, platform, override) {
			t.Fatalf("%s ignored its platform override", platform)
		}
	}
}

func TestWarningPolicyReadsTheScopedProfileConfigWhenNoSnapshot(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	for i, value := range []string{"true", "typo"} {
		if err := os.WriteFile(filepath.Join(dirs[i], "config.yaml"), []byte("display: {suppress_warning_notifications: "+value+"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []bool{false, true, false} {
		ctx := home.WithOverride(context.Background(), dirs[i%2])
		if got := WarningNotificationsEnabled(ctx, "slack", nil); got != want {
			t.Fatalf("profile %d enabled=%v want %v", i%2, got, want)
		}
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "config.yaml"), []byte("display: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := EffectiveUserConfig(home.WithOverride(context.Background(), broken)); len(got) != 0 {
		t.Fatalf("unreadable config served %v", got)
	}
}

func TestWarningStatusAndDiagnosticNoticeClassification(t *testing.T) {
	if !IsWarningStatus("warn", "x") || !IsWarningStatus("lifecycle", DiagnosticText("x")) || IsWarningStatus("lifecycle", "x") {
		t.Fatal("warning status classification")
	}
	for _, c := range []struct {
		level, key string
		want       bool
	}{{"warn", "", true}, {"error", "", true}, {"info", "credits.usage", true}, {"success", "credits.restored", true}, {"info", "other", false}} {
		if IsDiagnosticNotice(c.level, c.key) != c.want {
			t.Fatalf("%v", c)
		}
	}
}

type fakeWake struct {
	internal   bool
	metadata   map[string]any
	muted, set bool
	platform   string
}

func (w fakeWake) WakeInternal() bool           { return w.internal }
func (w fakeWake) WakeMetadata() map[string]any { return w.metadata }
func (w fakeWake) WakeReplyMuted() (bool, bool) { return w.muted, w.set }
func (w fakeWake) WakeSourcePlatform() string   { return w.platform }

func TestDiagnosticWakeAdmission(t *testing.T) {
	ctx := context.Background()
	diag := map[string]any{"notification_category": "diagnostic"}
	hidden := map[string]any{"display": map[string]any{"suppress_warning_notifications": true}}
	if got := DiagnosticMetadata(fakeWake{internal: true, metadata: diag}); got["notification_category"] != "diagnostic" {
		t.Fatalf("trusted wake lost its category %v", got)
	}
	if got := DiagnosticMetadata(fakeWake{metadata: diag}); len(got) != 0 {
		t.Fatalf("human content classified %v", got)
	}
	if !DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, platform: "telegram"}, hidden) {
		t.Fatal("hidden diagnostic wake not muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{metadata: diag, platform: "telegram"}, hidden) {
		t.Fatal("human content muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, platform: "telegram"}, map[string]any{}) {
		t.Fatal("visible diagnostic wake muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, set: true}, hidden) || !DiagnosticWakeMuted(ctx, fakeWake{muted: true, set: true}, map[string]any{}) {
		t.Fatal("recorded mute snapshot not honoured")
	}
	if len(DiagnosticMetadata(nil)) != 0 || DiagnosticWakeMuted(ctx, nil, hidden) {
		t.Fatal("absent event classified")
	}
	if DiagnosticTurnMuted(ctx, nil, "cli", hidden) {
		t.Fatal("uncategorized turn muted")
	}
}

func TestResolveDisplaySettingOverridesAndLegacy(t *testing.T) {
	ctx := context.Background()
	disp := func(d map[string]any) map[string]any { return map[string]any{"display": d} }
	cases := []struct {
		cfg           map[string]any
		platform, key string
		want          any
	}{
		{disp(map[string]any{"tool_progress": "all", "platforms": map[string]any{"telegram": map[string]any{"tool_progress": "verbose"}}}), "telegram", "tool_progress", "verbose"},
		{disp(map[string]any{"tool_progress": "new", "platforms": map[string]any{}}), "telegram", "tool_progress", "new"},
		{disp(map[string]any{"tool_progress": "all", "platforms": map[string]any{"slack": map[string]any{"tool_progress": "off"}}}), "slack", "tool_progress", "off"},
		{disp(map[string]any{"tool_progress": "all", "platforms": map[string]any{"slack": map[string]any{"tool_progress": "off"}}}), "telegram", "tool_progress", "all"},
		{disp(map[string]any{"tool_progress": "all", "tool_progress_overrides": map[string]any{"signal": "off", "telegram": "verbose"}}), "signal", "tool_progress", "off"},
		{disp(map[string]any{"tool_progress": "all", "tool_progress_overrides": map[string]any{"signal": "off", "telegram": "verbose"}}), "telegram", "tool_progress", "verbose"},
		{disp(map[string]any{"tool_progress": false}), "telegram", "tool_progress", "off"},
		{disp(map[string]any{"platforms": map[string]any{"whatsapp": map[string]any{"thinking_progress": "generic", "interim_assistant_messages": "generic", "long_running_notifications": "generic"}}}), "whatsapp", "thinking_progress", false},
		{disp(map[string]any{"platforms": map[string]any{"whatsapp": map[string]any{"interim_assistant_messages": "generic"}}}), "whatsapp", "interim_assistant_messages", false},
		{disp(map[string]any{"platforms": map[string]any{"whatsapp": map[string]any{"long_running_notifications": "generic"}}}), "whatsapp", "long_running_notifications", "generic"},
		{disp(map[string]any{"platforms": map[string]any{"whatsapp": map[string]any{"thinking_progress": "false"}}}), "whatsapp", "thinking_progress", false},
		{disp(map[string]any{"platforms": map[string]any{"telegram": map[string]any{"streaming": false}}}), "telegram", "streaming", false},
		{disp(map[string]any{"streaming": true}), "telegram", "streaming", nil},
		{map[string]any{}, "webhook", "tool_progress", "off"},
		{map[string]any{}, "api_server", "tool_preview_length", 0},
		{disp(map[string]any{"tool_preview_length": "12"}), "api_server", "tool_preview_length", 12},
		{disp(map[string]any{"reasoning_style": "QUOTE"}), "cli", "reasoning_style", "code"},
		{disp(map[string]any{"live_status": "all"}), "cli", "live_status", "full"},
		{disp(map[string]any{"suppress_warning_notifications": []any{}}), "cli", "suppress_warning_notifications", false},
	}
	for i, c := range cases {
		if got := ResolveDisplaySetting(ctx, c.cfg, c.platform, c.key, nil); got != c.want {
			t.Fatalf("case %d %s/%s = %#v, want %#v", i, c.platform, c.key, got, c.want)
		}
	}
	for _, val := range []string{"true", "yes", "on", "1"} {
		cfg := disp(map[string]any{"platforms": map[string]any{"telegram": map[string]any{"cleanup_progress": val}}})
		if got := ResolveDisplaySetting(ctx, cfg, "telegram", "cleanup_progress", nil); got != true {
			t.Fatalf("cleanup_progress %q = %#v", val, got)
		}
	}
}

func TestToolProgressWinningSourceControlsModeAndIntent(t *testing.T) {
	saved := PlatformDisplayLookup
	PlatformDisplayLookup = func(_ context.Context, key string) (string, map[string]any) {
		if key != "slack" {
			return "", nil
		}
		return "medium", map[string]any{"tool_progress": "off", "long_running_notifications": false, "busy_ack_detail": false}
	}
	t.Cleanup(func() { PlatformDisplayLookup = saved })
	cases := []struct {
		display  map[string]any
		env      string
		mode     any
		explicit bool
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
	}
	for i, c := range cases {
		mode, explicit := ResolveToolProgress(context.Background(), map[string]any{"display": c.display}, "slack", c.env)
		if mode != c.mode || explicit != c.explicit {
			t.Fatalf("case %d = (%v, %v), want (%v, %v)", i, mode, explicit, c.mode, c.explicit)
		}
	}
}

func TestLocalDisplayConfigOverlaysTheCliKey(t *testing.T) {
	display := map[string]any{"show_reasoning": false, "runtime_footer": map[string]any{"enabled": false, "fields": []any{"model"}},
		"platforms": map[string]any{"cli": map[string]any{"show_reasoning": "yes", "runtime_footer": map[string]any{"enabled": true}}}}
	got := LocalDisplayConfig(display)
	footer, _ := got["runtime_footer"].(map[string]any)
	if got["show_reasoning"] != true || footer["enabled"] != true || footer["fields"] == nil {
		t.Fatalf("local overlay %v", got)
	}
	if display["show_reasoning"] != false {
		t.Fatal("source block mutated")
	}
	if plain := LocalDisplayConfig("not a mapping"); len(plain) != 0 {
		t.Fatalf("non mapping display %v", plain)
	}
	if keys := DisplayOverrideableKeys(); len(keys) != 13 || !keys["suppress_warning_notifications"] || keys["thinking_progress"] {
		t.Fatalf("overrideable keys %v", keys)
	}
}
