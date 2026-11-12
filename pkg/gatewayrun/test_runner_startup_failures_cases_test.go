package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// test_runner_startup_failures.py test_is_global_startup_conflict_matches_lock_code_families
func TestIsGlobalStartupConflictMatchesLockCodeFamilies(t *testing.T) {
	for code, expected := range map[string]bool{
		"telegram-bot-token_lock": true, "discord-bot-token_lock": true, "whatsapp-session_lock": true,
		"feishu_app_lock": true, "lock_conflict": true, "telegram_connect_error": false, "telegram_auth_error": false,
		"relay_membership_required": false, "duplicate_credential": false, "": false,
	} {
		if got := gateway.IsGlobalStartupConflict(code); got != expected {
			t.Fatalf("is_global_startup_conflict(%q) = %v, want %v", code, got, expected)
		}
	}
}

// Field of a JSON object read from the runtime status, nil when absent
func runtimeStatusField(obj any, keys ...string) any {
	for _, k := range keys {
		o, ok := obj.(*pyjson.Object)
		if !ok {
			return nil
		}
		obj, _ = o.Get(k)
	}
	return obj
}

// Runtime status record the runner wrote under the case home
func caseRuntimeStatus(t *testing.T, home string) *pyjson.Object {
	t.Helper()
	state := gateway.ReadRuntimeStatus(filepath.Join(home, "gateway_state.json"))
	if state == nil {
		t.Fatal("no runtime status written")
	}
	return state
}

// test_runner_startup_failures.py test_runner_degrades_gracefully_when_all_adapters_missing
func TestRunnerDegradesGracefullyWhenAllAdaptersMissing(t *testing.T) {
	r := parallelConnectRunner(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ok, err := r.Start(context.Background())
	slog.SetDefault(prev)
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if r.exitCleanly {
		t.Fatal("should_exit_cleanly is True")
	}
	if r.adapters.Len() != 0 {
		t.Fatalf("adapters = %v", r.adapters.Keys())
	}
	state := caseRuntimeStatus(t, filepath.Dir(r.config.SessionsDir))
	if got := runtimeStatusField(state, "gateway_state"); got != "running" {
		t.Fatalf("gateway_state = %v", got)
	}
	failed := r.failedPlatforms.Keys()
	if len(failed) != 2 || !r.failedPlatforms.Has(gateway.PlatformTelegram) || !r.failedPlatforms.Has(gateway.PlatformDiscord) {
		t.Fatalf("failed platforms = %v", failed)
	}
	for _, platform := range []string{"telegram", "discord"} {
		if runtimeStatusField(state, "platforms", platform, "state") != "retrying" ||
			runtimeStatusField(state, "platforms", platform, "error_code") != "adapter_unavailable" ||
			runtimeStatusField(state, "platforms", platform, "needs_attention") != true {
			t.Fatalf("%s status = %v", platform, runtimeStatusField(state, "platforms", platform))
		}
	}
	if !strings.Contains(logs.String(), "No adapter could be created") {
		t.Fatalf("Expected degraded-mode warning when all adapters are missing, logs:\n%s", logs.String())
	}
}

// Discord adapter whose connect fails with a non retryable token collision
type nonRetryableFailureAdapter struct{ *gateway.BasePlatformAdapter }

func newNonRetryableFailureAdapter() *nonRetryableFailureAdapter {
	a := &nonRetryableFailureAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(&gateway.PlatformConfig{Enabled: true, Token: "***"}, gateway.PlatformDiscord, a)
	return a
}

func (a *nonRetryableFailureAdapter) Connect(ctx context.Context, _ bool) (bool, error) {
	a.SetFatalError(ctx, "discord-bot-token_lock", "Discord bot token already in use (PID 999). Stop the other gateway first.", false)
	return false, nil
}
func (a *nonRetryableFailureAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	return nil
}
func (a *nonRetryableFailureAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	panic("NotImplementedError")
}
func (a *nonRetryableFailureAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// test_runner_startup_failures.py test_runner_exits_with_ex_config_on_nonretryable_startup_error
func TestRunnerExitsWithExConfigOnNonretryableStartupError(t *testing.T) {
	r := parallelConnectRunner(t)
	delete(r.config.Platforms, gateway.PlatformTelegram)
	injectRegistryAdapters(t, func(gateway.Platform) gateway.PlatformAdapter { return newNonRetryableFailureAdapter() }, gateway.PlatformDiscord)
	ok, err := r.Start(context.Background())
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if !r.exitCleanly {
		t.Fatal("should_exit_cleanly is False")
	}
	if r.exitCode == nil || *r.exitCode != gateway.GatewayFatalConfigExitCode {
		t.Fatalf("exit_code = %v", r.exitCode)
	}
	if got := runtimeStatusField(caseRuntimeStatus(t, filepath.Dir(r.config.SessionsDir)), "gateway_state"); got != "startup_failed" {
		t.Fatalf("gateway_state = %v", got)
	}
}
