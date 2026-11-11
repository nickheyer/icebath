package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestGatewayRejectsNonAdminPersistentApprovalChange(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	telegram := gateway.NewPlatformConfig()
	telegram.Extra = map[string]any{"allow_admin_from": []any{"admin-1"}, "user_allowed_commands": []any{"approvals"}}
	r.config.Platforms[gateway.PlatformTelegram] = telegram
	called := false
	prev := RunApprovalModeCommand
	RunApprovalModeCommand = func(context.Context, string, *tools.Env, string) (string, error) {
		called = true
		return "", nil
	}
	t.Cleanup(func() { RunApprovalModeCommand = prev })
	event := gateway.NewMessageEvent("/approvals off")
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, UserID: kstr("user-1"), ChatID: "chat-1", ChatType: "dm"}
	output, err := r.handleApprovalsCommand(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(output), "admin") || called {
		t.Fatalf("output %q called %v", output, called)
	}
}

// Telegram dm yolo event of a chat
func yoloEvent(chatID string) *gateway.MessageEvent {
	event := gateway.NewMessageEvent("/yolo")
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, UserID: kstr("user-" + chatID), ChatID: chatID,
		UserName: kstr("tester"), ChatType: "dm"}
	return event
}

// Clears the yolo state of both test chats before and after the case
func cleanYoloState(t *testing.T) {
	t.Setenv("ICEBATH_YOLO_MODE", "")
	for _, k := range []string{"agent:main:telegram:dm:chat-a", "agent:main:telegram:dm:chat-b"} {
		tools.DisableSessionYolo(k)
	}
	t.Cleanup(func() {
		for _, k := range []string{"agent:main:telegram:dm:chat-a", "agent:main:telegram:dm:chat-b"} {
			tools.DisableSessionYolo(k)
		}
	})
}

func TestYoloCommandTogglesOnlyCurrentSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cleanYoloState(t)
	ctx := context.Background()
	r := allocGatewayRunner()
	eventA := yoloEvent("chat-a")
	sessionA, err := r.sessionKeyForSource(ctx, eventA.Source)
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := r.sessionKeyForSource(ctx, yoloEvent("chat-b").Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.handleYoloCommand(ctx, eventA); err != nil {
		t.Fatal(err)
	}
	if !tools.IsSessionYoloEnabled(sessionA) || tools.IsSessionYoloEnabled(sessionB) || os.Getenv("ICEBATH_YOLO_MODE") != "" {
		t.Fatal("first toggle leaked")
	}
	if _, err := r.handleYoloCommand(ctx, eventA); err != nil {
		t.Fatal(err)
	}
	if tools.IsSessionYoloEnabled(sessionA) || os.Getenv("ICEBATH_YOLO_MODE") != "" {
		t.Fatal("second toggle left yolo on")
	}
}

func TestYoloSurvivesGatewayRestartAndDiesAtSessionBoundary(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cleanYoloState(t)
	ctx := context.Background()
	sessions := t.TempDir()
	newRunner := func() *GatewayRunner {
		r := allocGatewayRunner()
		r.sessionStore = gateway.NewSessionStore(ctx, sessions, gateway.DefaultGatewayConfig(ctx), nil)
		return r
	}
	persistedYolo := func(r *GatewayRunner, source *gateway.SessionSource) bool {
		entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true)
		if err != nil {
			t.Fatal(err)
		}
		return entry.Yolo != nil && *entry.Yolo
	}
	event := yoloEvent("chat-a")
	first := newRunner()
	key, err := first.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.handleYoloCommand(ctx, event); err != nil {
		t.Fatal(err)
	}
	if !tools.IsSessionYoloEnabled(key) {
		t.Fatal("yolo not enabled")
	}
	tools.DisableSessionYolo(key)
	second := newRunner()
	tools.RestoreSessionYolo(key, persistedYolo(second, event.Source))
	if !tools.IsSessionYoloEnabled(key) {
		t.Fatal("restart did not re-arm yolo")
	}
	second.clearSessionBoundarySecurityState(ctx, key)
	if tools.IsSessionYoloEnabled(key) {
		t.Fatal("boundary kept yolo")
	}
	third := newRunner()
	tools.RestoreSessionYolo(key, persistedYolo(third, event.Source))
	if tools.IsSessionYoloEnabled(key) {
		t.Fatal("boundary did not clear the persisted copy")
	}
}

// Home whose config.yaml holds the text, with a bare runner and a Telegram verbose event
func verboseFixture(t *testing.T, configText string) (string, *GatewayRunner, *gateway.MessageEvent) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "hermes")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	r := allocGatewayRunner()
	r.adapters = adapterDict()
	event := gateway.NewMessageEvent("/verbose")
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, UserID: kstr("12345"), ChatID: "67890", UserName: kstr("testuser")}
	return configPath, r, event
}

func TestVerboseCommandDisabledByDefault(t *testing.T) {
	_, r, event := verboseFixture(t, "display:\n  tool_progress: all\n")
	result, err := r.handleVerboseCommand(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(result), "not enabled") || !strings.Contains(result, "tool_progress_command") {
		t.Fatalf("result %q", result)
	}
}

func TestVerboseCommandEnabledCyclesMode(t *testing.T) {
	configPath, r, event := verboseFixture(t, "display:\n  tool_progress_command: true\n  tool_progress: all\n")
	result, err := r.handleVerboseCommand(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "VERBOSE") || !strings.Contains(strings.ToLower(result), "telegram") {
		t.Fatalf("result %q", result)
	}
	doc, err := config.ReadDocument(configPath)
	if err != nil {
		t.Fatal(err)
	}
	display, _ := doc["display"].(map[string]any)
	platforms, _ := display["platforms"].(map[string]any)
	telegram, _ := platforms["telegram"].(map[string]any)
	if telegram["tool_progress"] != "verbose" {
		t.Fatalf("saved %v", doc)
	}
}

func TestVerboseCommandQuotedFalseKeepsCommandDisabled(t *testing.T) {
	_, r, event := verboseFixture(t, "display:\n  tool_progress_command: \"false\"\n  tool_progress: all\n")
	result, err := r.handleVerboseCommand(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(result), "not enabled") || !strings.Contains(result, "tool_progress_command") {
		t.Fatalf("result %q", result)
	}
}
