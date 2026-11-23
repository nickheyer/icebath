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
	if !config.WarningNotificationsEnabled(ctx, "telegram") {
		t.Fatal("default must show warnings")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display:\n  platforms:\n    telegram:\n      suppress_warning_notifications: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if config.WarningNotificationsEnabled(ctx, "telegram") || !config.WarningNotificationsEnabled(ctx, "slack") {
		t.Fatal("platform opt out")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display:\n  suppress_warning_notifications: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if config.WarningNotificationsEnabled(ctx, "telegram") {
		t.Fatal("the profile setting must hide warnings")
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
	if !config.DiagnosticWakeMuted(ctx, wakeDouble{internal: true, meta: diag}) || config.DiagnosticWakeMuted(ctx, wakeDouble{internal: false, meta: diag}) {
		t.Fatal("wake mute")
	}
	no := false
	if config.DiagnosticWakeMuted(ctx, wakeDouble{internal: true, meta: diag, snapshot: &no}) {
		t.Fatal("snapshot must win")
	}
	ran := 0
	hiddenRan, _ := config.RenderNotification(ctx, func() error { ran++; return nil }, "telegram", true)
	shownRan, _ := config.RenderNotification(ctx, func() error { ran++; return nil }, "telegram", false)
	if hiddenRan || !shownRan || ran != 1 {
		t.Fatal("render gate")
	}
	presented, err := config.PresentNotification(ctx, func(context.Context) error { ran++; return nil }, "slack", true)
	if !presented || err != nil || ran != 2 {
		t.Fatal("present gate")
	}
}
