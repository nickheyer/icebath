package gatewayrun

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Api server adapter that loses the bind race, fatal and non retryable so it is parked
type portTakenAdapter struct{ *gateway.BasePlatformAdapter }

func newPortTakenAdapter() *portTakenAdapter {
	a := &portTakenAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(&gateway.PlatformConfig{Enabled: true, Extra: map[string]any{"port": 8642}}, gateway.PlatformAPIServer, a)
	return a
}

func (a *portTakenAdapter) Connect(ctx context.Context, _ bool) (bool, error) {
	a.SetFatalError(ctx, "api_server_port_in_use", "Port 8642 already in use. Set platforms.api_server.port in config.yaml to a different "+
		"value, then `/platform resume api_server`.", false)
	return false, nil
}
func (a *portTakenAdapter) Disconnect(ctx context.Context) error { a.MarkDisconnected(ctx); return nil }
func (a *portTakenAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	panic("NotImplementedError")
}
func (a *portTakenAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Telegram adapter that connects, and can lose its bot mid run non retryably
type healthyAdapter struct{ *gateway.BasePlatformAdapter }

func newHealthyAdapter(platform gateway.Platform) *healthyAdapter {
	a := &healthyAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(&gateway.PlatformConfig{Enabled: true, Token: "***"}, platform, a)
	return a
}

func (a *healthyAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *healthyAdapter) Disconnect(ctx context.Context) error        { a.MarkDisconnected(ctx); return nil }
func (a *healthyAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	panic("NotImplementedError")
}
func (a *healthyAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Loses the bot mid run with the code and notifies the runner's fatal handler
func (a *healthyAdapter) fail(ctx context.Context, code string) error {
	a.SetFatalError(ctx, code, "Telegram rejected the bot token.", false)
	return a.NotifyFatalError(ctx)
}

// Runner of the degraded cases with the product room server bound, adapters from the registry, bluebubbles adapterless
func degradedRunner(t *testing.T, platforms map[gateway.Platform]*gateway.PlatformConfig, build map[gateway.Platform]func() gateway.PlatformAdapter) *GatewayRunner {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	ctx := context.Background()
	tuiserver.BindHostedRoomServer()
	for platform, make := range build {
		injectRegistryAdapters(t, func(gateway.Platform) gateway.PlatformAdapter { return make() }, platform)
	}
	if _, ok := build[gateway.PlatformBlueBubbles]; !ok {
		entry := gateway.NewPlatformEntry("bluebubbles", "BlueBubbles", func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil },
			func() bool { return true })
		if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, "bluebubbles", "") })
	}
	cfg := gateway.DefaultGatewayConfig(ctx)
	cfg.Platforms = platforms
	cfg.SessionsDir = filepath.Join(tmp, "sessions")
	r, err := NewGatewayRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), false, false, false) })
	return r
}

// Error level records of the default logger while fn runs
func errorLogs(t *testing.T, fn func()) string {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)
	fn()
	return logs.String()
}

// Runtime status after the queued writes are flushed
func flushedStatus(t *testing.T, r *GatewayRunner) *pyjson.Object {
	t.Helper()
	if !gateway.FlushRuntimeStatus(5 * time.Second) {
		t.Fatal("runtime status did not flush")
	}
	return caseRuntimeStatus(t, filepath.Dir(r.config.SessionsDir))
}

// Asserts the platform's runtime status fields
func assertPlatformStatus(t *testing.T, state *pyjson.Object, platform string, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got := runtimeStatusField(state, "platforms", platform, k); got != v {
			t.Fatalf("%s %s = %v, want %v", platform, k, got, v)
		}
	}
}

func oneParkedPlatformRunner(t *testing.T) *GatewayRunner {
	return degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{
		gateway.PlatformAPIServer: {Enabled: true, Extra: map[string]any{"port": 8642}},
		gateway.PlatformTelegram:  {Enabled: true, Token: "***"},
	}, map[gateway.Platform]func() gateway.PlatformAdapter{
		gateway.PlatformAPIServer: func() gateway.PlatformAdapter { return newPortTakenAdapter() },
		gateway.PlatformTelegram:  func() gateway.PlatformAdapter { return newHealthyAdapter(gateway.PlatformTelegram) },
	})
}

// test_startup_failed_platform_reports_degraded.py test_parked_platform_is_logged_at_error_and_the_run_is_degraded
func TestParkedPlatformIsLoggedAtErrorAndTheRunIsDegraded(t *testing.T) {
	r := oneParkedPlatformRunner(t)
	var ok bool
	var err error
	logs := errorLogs(t, func() { ok, err = r.Start(context.Background()) })
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if r.exitCleanly {
		t.Fatal("should_exit_cleanly is True")
	}
	if !strings.Contains(logs, "api_server") {
		t.Fatalf("no ERROR naming api_server:\n%s", logs)
	}
	state := flushedStatus(t, r)
	if got := runtimeStatusField(state, "gateway_state"); got != "degraded" {
		t.Fatalf("gateway_state = %v", got)
	}
	assertPlatformStatus(t, state, "api_server", map[string]any{"state": "fatal", "error_code": "api_server_port_in_use"})
	assertPlatformStatus(t, state, "telegram", map[string]any{"state": "connected"})
}

// test_startup_failed_platform_reports_degraded.py test_sole_parked_platform_keeps_the_gateway_up_degraded
func TestSoleParkedPlatformKeepsTheGatewayUpDegraded(t *testing.T) {
	r := degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{
		gateway.PlatformAPIServer: {Enabled: true, Extra: map[string]any{"port": 8642}},
	}, map[gateway.Platform]func() gateway.PlatformAdapter{
		gateway.PlatformAPIServer: func() gateway.PlatformAdapter { return newPortTakenAdapter() },
	})
	var ok bool
	var err error
	logs := errorLogs(t, func() { ok, err = r.Start(context.Background()) })
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if r.exitCleanly || r.exitCode != nil {
		t.Fatalf("(should_exit_cleanly, exit_code) = (%v, %v)", r.exitCleanly, r.exitCode)
	}
	if !strings.Contains(logs, "api_server") {
		t.Fatalf("no ERROR naming api_server:\n%s", logs)
	}
	state := flushedStatus(t, r)
	if got := runtimeStatusField(state, "gateway_state"); got != "degraded" {
		t.Fatalf("gateway_state = %v", got)
	}
	assertPlatformStatus(t, state, "api_server", map[string]any{"state": "fatal", "error_code": "api_server_port_in_use"})
}

// Starts a runner whose only platform is the returned telegram adapter
func startOnlyTelegram(t *testing.T) (*GatewayRunner, *healthyAdapter) {
	t.Helper()
	adapter := newHealthyAdapter(gateway.PlatformTelegram)
	r := degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: {Enabled: true, Token: "***"}},
		map[gateway.Platform]func() gateway.PlatformAdapter{gateway.PlatformTelegram: func() gateway.PlatformAdapter { return adapter }})
	if ok, err := r.Start(context.Background()); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	return r, adapter
}

// test_startup_failed_platform_reports_degraded.py test_last_platform_lost_non_retryably_mid_run_keeps_the_gateway_up_degraded
func TestLastPlatformLostNonRetryablyMidRunKeepsTheGatewayUpDegraded(t *testing.T) {
	r, adapter := startOnlyTelegram(t)
	logs := errorLogs(t, func() {
		if err := adapter.fail(context.Background(), "telegram_auth_error"); err != nil {
			t.Fatal(err)
		}
	})
	if !r.running.Load() {
		t.Fatal("the runner stopped")
	}
	if !strings.Contains(logs, "telegram") || !strings.Contains(logs, "icebath gateway restart") {
		t.Fatalf("no ERROR naming telegram and the restart command:\n%s", logs)
	}
	state := flushedStatus(t, r)
	if got := runtimeStatusField(state, "gateway_state"); got != "degraded" {
		t.Fatalf("gateway_state = %v", got)
	}
	assertPlatformStatus(t, state, "telegram", map[string]any{"state": "fatal", "error_code": "telegram_auth_error"})
}

// Waits until the runtime status reports the gateway state
func waitGatewayState(t *testing.T, r *GatewayRunner, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if runtimeStatusField(flushedStatus(t, r), "gateway_state") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway_state never became %s", want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// test_startup_failed_platform_reports_degraded.py test_last_platform_parked_during_an_external_drain_keeps_draining_until_release
func TestLastPlatformParkedDuringAnExternalDrainKeepsDrainingUntilRelease(t *testing.T) {
	r, adapter := startOnlyTelegram(t)
	ctx := context.Background()
	h := filepath.Dir(r.config.SessionsDir)
	if _, err := gateway.WriteDrainRequest(ctx, "dashboard", false, h); err != nil {
		t.Fatal(err)
	}
	waitGatewayState(t, r, "draining")
	if err := adapter.fail(ctx, "telegram_auth_error"); err != nil {
		t.Fatal(err)
	}
	if got := runtimeStatusField(flushedStatus(t, r), "gateway_state"); got != "draining" {
		t.Fatalf("gateway_state = %v", got)
	}
	gateway.ClearDrainRequest(ctx, h)
	waitGatewayState(t, r, "degraded")
}

// test_startup_failed_platform_reports_degraded.py test_mid_run_ownership_conflict_on_the_last_platform_still_stops
func TestMidRunOwnershipConflictOnTheLastPlatformStillStops(t *testing.T) {
	r, adapter := startOnlyTelegram(t)
	if err := adapter.fail(context.Background(), "telegram_token_lock"); err != nil {
		t.Fatal(err)
	}
	if r.running.Load() {
		t.Fatal("the runner did not stop")
	}
	if r.exitWithFailure {
		t.Fatal("_exit_with_failure is True")
	}
}

// test_startup_failed_platform_reports_degraded.py test_every_platform_connected_still_reports_a_normal_run
func TestEveryPlatformConnectedStillReportsANormalRun(t *testing.T) {
	r := degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: {Enabled: true, Token: "***"}},
		map[gateway.Platform]func() gateway.PlatformAdapter{gateway.PlatformTelegram: func() gateway.PlatformAdapter { return newHealthyAdapter(gateway.PlatformTelegram) }})
	var ok bool
	var err error
	logs := errorLogs(t, func() { ok, err = r.Start(context.Background()) })
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	state := flushedStatus(t, r)
	if got := runtimeStatusField(state, "gateway_state"); got != "running" {
		t.Fatalf("gateway_state = %v", got)
	}
	assertPlatformStatus(t, state, "telegram", map[string]any{"state": "connected"})
	if logs != "" {
		t.Fatalf("a healthy startup must not log ERROR:\n%s", logs)
	}
}

// test_startup_failed_platform_reports_degraded.py test_missing_adapter_degrades_only_the_unserved_enabled_platform
func TestMissingAdapterDegradesOnlyTheUnservedEnabledPlatform(t *testing.T) {
	r := degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{
		gateway.PlatformTelegram:    {Enabled: true, Token: "***"},
		gateway.PlatformDiscord:     {Enabled: true, Token: "***"},
		gateway.PlatformSlack:       {Enabled: false, Token: "***"},
		gateway.PlatformBlueBubbles: {Enabled: true},
	}, map[gateway.Platform]func() gateway.PlatformAdapter{
		gateway.PlatformTelegram: func() gateway.PlatformAdapter { return newHealthyAdapter(gateway.PlatformTelegram) },
	})
	if ok, err := r.Start(context.Background()); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	state := flushedStatus(t, r)
	assertPlatformStatus(t, state, "telegram", map[string]any{"state": "connected"})
	assertPlatformStatus(t, state, "discord", map[string]any{"state": "retrying", "error_code": "adapter_unavailable", "needs_attention": true})
	if runtimeStatusField(state, "platforms", "slack") != nil {
		t.Fatal("disabled slack appears in status")
	}
	assertPlatformStatus(t, state, "bluebubbles", map[string]any{"state": "fatal", "error_code": "adapter_unavailable", "needs_attention": true})
	if msg, _ := runtimeStatusField(state, "platforms", "bluebubbles", "error_message").(string); strings.Contains(msg, "Retrying") {
		t.Fatalf("bluebubbles error_message = %q", msg)
	}
	if keys := r.failedPlatforms.Keys(); len(keys) != 1 || keys[0] != gateway.PlatformDiscord {
		t.Fatalf("failed platforms = %v", keys)
	}
}

// test_startup_failed_platform_reports_degraded.py test_adapterless_platform_heals_once_its_adapter_appears
func TestAdapterlessPlatformHealsOnceItsAdapterAppears(t *testing.T) {
	ctx := context.Background()
	platform := gateway.PlatformDiscord
	r := degradedRunner(t, map[gateway.Platform]*gateway.PlatformConfig{platform: {Enabled: true, Token: "***"}}, nil)
	if ok, err := r.Start(ctx); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	assertPlatformStatus(t, flushedStatus(t, r), "discord", map[string]any{"error_code": "adapter_unavailable", "needs_attention": true})
	if err := r.reconnectFailedPlatform(ctx, platform, pythread.Monotonic()+60); err != nil {
		t.Fatal(err)
	}
	if info, _ := r.failedPlatforms.Get(platform); info == nil || info["attempts"] != 2 {
		t.Fatalf("queued entry = %v", info)
	}
	if msg, _ := runtimeStatusField(flushedStatus(t, r), "platforms", "discord", "error_message").(string); !strings.Contains(msg, "check the plugin") {
		t.Fatalf("error_message = %q", msg)
	}
	healthy := newHealthyAdapter(platform)
	injectRegistryAdapters(t, func(gateway.Platform) gateway.PlatformAdapter { return healthy }, platform)
	if err := r.reconnectFailedPlatform(ctx, platform, pythread.Monotonic()+3600); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.adapters.Get(platform); got != gateway.PlatformAdapter(healthy) {
		t.Fatalf("adapter = %v", got)
	}
	if r.failedPlatforms.Len() != 0 {
		t.Fatalf("failed platforms = %v", r.failedPlatforms.Keys())
	}
	assertPlatformStatus(t, flushedStatus(t, r), "discord", map[string]any{"state": "connected", "needs_attention": false})
	slack := gateway.PlatformSlack
	entry := gateway.NewPlatformEntry("slack", "Slack", func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil },
		func() bool { return true })
	if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, "slack", "") })
	r.failedPlatforms.Set(slack, r.startupRetryEntry(slack, nil, &gateway.PlatformConfig{Enabled: true, Token: "***"}, true))
	if err := r.reconnectFailedPlatform(ctx, slack, pythread.Monotonic()+3600); err != nil {
		t.Fatal(err)
	}
	if r.failedPlatforms.Has(slack) {
		t.Fatal("slack still queued")
	}
	state := flushedStatus(t, r)
	assertPlatformStatus(t, state, "slack", map[string]any{"state": "fatal", "error_code": "adapter_unavailable"})
	if msg, _ := runtimeStatusField(state, "platforms", "slack", "error_message").(string); strings.Contains(msg, "Retrying") {
		t.Fatalf("slack error_message = %q", msg)
	}
}
