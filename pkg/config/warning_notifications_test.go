package config

import (
	"context"
	"errors"
	"testing"
)

// Installs a warning suppression answer for the test, restored when it ends
func suppressWarnings(t *testing.T, hidden func(platform string) bool) {
	t.Helper()
	saved := SuppressWarningsOn
	t.Cleanup(func() { SuppressWarningsOn = saved })
	SuppressWarningsOn = func(_ context.Context, platform string) bool { return hidden(platform) }
}

func TestRenderNotificationCallsRendererOnlyWhenVisible(t *testing.T) {
	for _, hide := range []bool{false, true} {
		suppressWarnings(t, func(string) bool { return hide })
		for _, diagnostic := range []bool{false, true} {
			observed := []string{"producer ran"}
			presented, err := RenderNotification(context.Background(), func() error {
				observed = append(observed, "rendered")
				return nil
			}, "cli", diagnostic)
			expected := !(diagnostic && hide)
			if err != nil || presented != expected || len(observed) != map[bool]int{true: 2, false: 1}[expected] {
				t.Fatalf("hide=%v diagnostic=%v presented=%v err=%v observed=%v", hide, diagnostic, presented, err, observed)
			}
		}
	}
}

func TestRenderFailurePropagatesAndRequestedResultDoesNotReadPolicy(t *testing.T) {
	saved := warningPolicy
	warningPolicy = func(context.Context, string) bool {
		t.Fatal("must not consult policy for result")
		return false
	}
	t.Cleanup(func() { warningPolicy = saved })
	failure := errors.New("renderer failed")
	for _, run := range []func() (bool, error){
		func() (bool, error) {
			return RenderNotification(context.Background(), func() error { return failure }, "tui", false)
		},
		func() (bool, error) {
			return PresentNotification(context.Background(), func(context.Context) error { return failure }, "tui", false)
		},
	} {
		if _, err := run(); !errors.Is(err, failure) {
			t.Fatalf("renderer failure lost: %v", err)
		}
	}
}

func TestWarningsShowWithoutTheSettingsPolicy(t *testing.T) {
	saved := SuppressWarningsOn
	t.Cleanup(func() { SuppressWarningsOn = saved })
	SuppressWarningsOn = nil
	if !WarningNotificationsEnabled(context.Background(), "slack") {
		t.Fatal("no policy must show warnings")
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
	if got := DiagnosticMetadata(fakeWake{internal: true, metadata: diag}); got["notification_category"] != "diagnostic" {
		t.Fatalf("trusted wake lost its category %v", got)
	}
	if got := DiagnosticMetadata(fakeWake{metadata: diag}); len(got) != 0 {
		t.Fatalf("human content classified %v", got)
	}
	suppressWarnings(t, func(platform string) bool { return platform == "telegram" })
	if !DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, platform: "telegram"}) {
		t.Fatal("hidden diagnostic wake not muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{metadata: diag, platform: "telegram"}) {
		t.Fatal("human content muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, platform: "slack"}) {
		t.Fatal("visible diagnostic wake muted")
	}
	if DiagnosticWakeMuted(ctx, fakeWake{internal: true, metadata: diag, platform: "telegram", set: true}) || !DiagnosticWakeMuted(ctx, fakeWake{muted: true, set: true}) {
		t.Fatal("recorded mute snapshot not honoured")
	}
	if len(DiagnosticMetadata(nil)) != 0 || DiagnosticWakeMuted(ctx, nil) {
		t.Fatal("absent event classified")
	}
	if DiagnosticTurnMuted(ctx, nil, "telegram") {
		t.Fatal("uncategorized turn muted")
	}
}
