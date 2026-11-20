package cron

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/home"
)

const sharedRoutePrimaryYAML = "gateway:\n  multiplex_profiles: true\n  profile_routes:\n" +
	"  - {name: fit, platform: discord, chat_id: '1543065293755256852', profile: fitness}\n" +
	"  - {name: 'off', platform: discord, chat_id: '999', profile: fitness, enabled: false}\n" +
	"  - {name: other, platform: discord, chat_id: '777', profile: other}\n"

// Primary adapter double recording each chat it sent to
type sharedRoutePrimary struct {
	mu   sync.Mutex
	sent []string
}

func (a *sharedRoutePrimary) Send(_ context.Context, chatID, _, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, chatID)
	return gateway.SendResult{Success: true, MessageID: "m1"}, nil
}

func (a *sharedRoutePrimary) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.sent
	a.sent = nil
	return out
}

// Fitness satellite under a primary root with the given config, the ctx served as the satellite
func sharedRouteSatellite(t *testing.T, primaryYAML string) context.Context {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	fitness := filepath.Join(root, "profiles", "fitness")
	writeTestFile(t, filepath.Join(root, "config.yaml"), primaryYAML)
	writeTestFile(t, filepath.Join(fitness, "config.yaml"), "cron:\n  wrap_response: false\n")
	if err := os.MkdirAll(filepath.Join(fitness, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	registerHomePlatforms(t, "discord")
	useRealDelivery(t)
	return home.WithOverride(context.Background(), fitness)
}

// Drives deliverResult on a live loop with a discord enabled gateway config, returning the error and standalone chats
func runSharedRouteDelivery(t *testing.T, ctx context.Context, chatID string, adapters any) (*string, []string) {
	t.Helper()
	var mu sync.Mutex
	var standalone []string
	prevSend, prevCfg := sendToPlatform, loadGatewayConfig
	sendToPlatform = func(_ context.Context, _ gateway.Platform, _ *gateway.PlatformConfig, chat, _ string, _ messagetools.SendOptions) (map[string]any, error) {
		mu.Lock()
		standalone = append(standalone, chat)
		mu.Unlock()
		return map[string]any{"success": false, "error": "DISCORD_BOT_TOKEN is not set"}, nil
	}
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) {
		return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformDiscord: {Enabled: true}}}, nil
	}
	defer func() { sendToPlatform, loadGatewayConfig = prevSend, prevCfg }()
	loop := gateway.NewCallbackLoop()
	loopCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = loop.Run(loopCtx); close(done) }()
	defer func() { stop(); loop.Close(); <-done }()
	if !waitUntil(loop.IsRunning) {
		t.Fatal("loop never ran")
	}
	s := openCronStore(home.Dir(ctx), nil)
	job := &Job{ID: "a7ae1520356c", Name: "brief", Deliver: strPtr("discord:" + chatID)}
	errText, err := s.deliverResult(ctx, job, "hello", adapters, loop, false)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return errText, slices.Clone(standalone)
}

func TestSatelliteRoutesExactTargetThroughPrimaryAdapter(t *testing.T) {
	ctx := sharedRouteSatellite(t, sharedRoutePrimaryYAML)
	primary := &sharedRoutePrimary{}
	shared := newSharedRouteAdapters(map[string]any{string(gateway.PlatformDiscord): primary}, primaryProfileRoutesForCurrentHome(ctx))
	errText, standalone := runSharedRouteDelivery(t, ctx, "1543065293755256852", shared)
	if errText != nil {
		t.Fatal(*errText)
	}
	if sent := primary.take(); !slices.Equal(sent, []string{"1543065293755256852"}) || len(standalone) != 0 {
		t.Fatalf("sent %v standalone %v", sent, standalone)
	}
	for _, chat := range []string{"424242", "999", "777"} {
		errText, standalone := runSharedRouteDelivery(t, ctx, chat, shared)
		if errText == nil || !strings.Contains(*errText, "DISCORD_BOT_TOKEN") {
			t.Fatalf("chat %s error = %v", chat, errText)
		}
		if sent := primary.take(); len(sent) != 0 || !slices.Equal(standalone, []string{chat}) {
			t.Fatalf("chat %s sent %v standalone %v", chat, sent, standalone)
		}
	}
}

func TestGuildScopedRouteAuthorizesCronTargetEvenWhenSatelliteHasNoPlatformBlock(t *testing.T) {
	ctx := sharedRouteSatellite(t, "gateway:\n  multiplex_profiles: true\n  profile_routes:\n"+
		"  - {platform: discord, guild_id: G1, chat_id: C1, profile: fitness}\n")
	primary := &sharedRoutePrimary{}
	shared := newSharedRouteAdapters(map[string]any{string(gateway.PlatformDiscord): primary}, primaryProfileRoutesForCurrentHome(ctx))
	for range 2 {
		errText, standalone := runSharedRouteDelivery(t, ctx, "C1", shared)
		if errText != nil {
			t.Fatal(*errText)
		}
		if sent := primary.take(); !slices.Equal(sent, []string{"C1"}) || len(standalone) != 0 {
			t.Fatalf("sent %v standalone %v", sent, standalone)
		}
	}
}
