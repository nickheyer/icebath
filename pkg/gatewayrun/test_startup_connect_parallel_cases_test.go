package gatewayrun

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Connect start and end events in arrival order, independent of clock resolution
type connectOrder struct {
	mu     sync.Mutex
	events [][2]string
}

func (o *connectOrder) add(platform gateway.Platform, kind string) {
	o.mu.Lock()
	o.events = append(o.events, [2]string{string(platform), kind})
	o.mu.Unlock()
}

func (o *connectOrder) indexOf(platform gateway.Platform, kind string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, e := range o.events {
		if e == [2]string{string(platform), kind} {
			return i
		}
	}
	return -1
}

// Adapter whose connect records its events around a sleep, optionally failing with a retryable fatal error
type timingAdapter struct {
	*gateway.BasePlatformAdapter
	order *connectOrder
	sleep time.Duration
	fail  bool
}

func newTimingAdapter(platform gateway.Platform, order *connectOrder, sleep time.Duration, fail bool) *timingAdapter {
	a := &timingAdapter{order: order, sleep: sleep, fail: fail}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(&gateway.PlatformConfig{Enabled: true, Token: "***"}, platform, a)
	return a
}

func (a *timingAdapter) Connect(ctx context.Context, _ bool) (bool, error) {
	if a.order != nil {
		a.order.add(a.Platform, "start")
	}
	select {
	case <-time.After(a.sleep):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if a.fail {
		a.SetFatalError(ctx, "telegram_proxy_dead", "proxy unreachable", true)
		return false, nil
	}
	if a.order != nil {
		a.order.add(a.Platform, "end")
	}
	return true, nil
}
func (a *timingAdapter) Disconnect(ctx context.Context) error { a.MarkDisconnected(ctx); return nil }
func (a *timingAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	panic("NotImplementedError")
}
func (a *timingAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Registers a plugin factory per platform so the runner's create adapter path builds the case's adapters
func injectRegistryAdapters(t *testing.T, build func(gateway.Platform) gateway.PlatformAdapter, platforms ...gateway.Platform) {
	t.Helper()
	ctx := context.Background()
	for _, platform := range platforms {
		entry := gateway.NewPlatformEntry(string(platform), string(platform), func(context.Context, *gateway.PlatformConfig) (any, error) {
			return build(platform), nil
		}, func() bool { return true })
		if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, string(platform), "") })
	}
}

// Runner of the parallel connect cases with Telegram and Discord enabled under a temp home
func parallelConnectRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	ctx := context.Background()
	cfg := gateway.DefaultGatewayConfig(ctx)
	cfg.Platforms = map[gateway.Platform]*gateway.PlatformConfig{
		gateway.PlatformTelegram: {Enabled: true, Token: "***"},
		gateway.PlatformDiscord:  {Enabled: true, Token: "***"},
	}
	cfg.SessionsDir = filepath.Join(tmp, "sessions")
	r, err := NewGatewayRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), false, false, false) })
	return r
}

// test_startup_connect_parallel.py test_startup_connects_platforms_concurrently
func TestStartupConnectsPlatformsConcurrently(t *testing.T) {
	order := &connectOrder{}
	r := parallelConnectRunner(t)
	injectRegistryAdapters(t, func(p gateway.Platform) gateway.PlatformAdapter {
		sleep := time.Duration(0)
		if p == gateway.PlatformTelegram {
			sleep = 300 * time.Millisecond
		}
		return newTimingAdapter(p, order, sleep, false)
	}, gateway.PlatformTelegram, gateway.PlatformDiscord)
	if _, err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fastEnd, slowEnd := order.indexOf(gateway.PlatformDiscord, "end"), order.indexOf(gateway.PlatformTelegram, "end")
	if fastEnd == -1 || slowEnd == -1 {
		t.Fatalf("missing end events: %v", order.events)
	}
	if fastEnd >= slowEnd {
		t.Fatalf("connects did not overlap (serial loop?): events=%v", order.events)
	}
	if !r.adapters.Has(gateway.PlatformTelegram) || !r.adapters.Has(gateway.PlatformDiscord) {
		t.Fatalf("adapters = %v", r.adapters.Keys())
	}
}

// test_startup_connect_parallel.py test_startup_one_failing_platform_does_not_block_others
func TestStartupOneFailingPlatformDoesNotBlockOthers(t *testing.T) {
	order := &connectOrder{}
	r := parallelConnectRunner(t)
	injectRegistryAdapters(t, func(p gateway.Platform) gateway.PlatformAdapter {
		if p == gateway.PlatformTelegram {
			return newTimingAdapter(p, nil, 300*time.Millisecond, true)
		}
		return newTimingAdapter(p, order, 0, false)
	}, gateway.PlatformTelegram, gateway.PlatformDiscord)
	if _, err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.adapters.Has(gateway.PlatformDiscord) {
		t.Fatalf("adapters = %v", r.adapters.Keys())
	}
	if !r.failedPlatforms.Has(gateway.PlatformTelegram) {
		t.Fatalf("failed platforms = %v", r.failedPlatforms.Keys())
	}
}

// Runner of the cold start cap cases with no platforms
func coldStartRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	tmp := t.TempDir()
	ctx := context.Background()
	cfg := gateway.DefaultGatewayConfig(ctx)
	cfg.Platforms = map[gateway.Platform]*gateway.PlatformConfig{}
	cfg.SessionsDir = filepath.Join(tmp, "sessions")
	r, err := NewGatewayRunner(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// test_startup_connect_parallel.py TestTelegramColdStartCap.test_initial_telegram_budget_is_capped
func TestTelegramColdStartCapInitialTelegramBudgetIsCapped(t *testing.T) {
	t.Setenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT", "")
	r := coldStartRunner(t)
	initial := r.platformConnectTimeoutSecs(gateway.PlatformTelegram, true)
	full := r.platformConnectTimeoutSecs(gateway.PlatformTelegram, false)
	if initial >= full {
		t.Fatalf("cold-start Telegram budget must be shorter than the reconnect budget (initial=%v, full=%v)", initial, full)
	}
	if initial > 60.0 {
		t.Fatalf("initial = %v", initial)
	}
}

// test_startup_connect_parallel.py TestTelegramColdStartCap.test_other_platforms_unchanged
func TestTelegramColdStartCapOtherPlatformsUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT", "")
	r := coldStartRunner(t)
	if r.platformConnectTimeoutSecs(gateway.PlatformDiscord, true) != r.platformConnectTimeoutSecs(gateway.PlatformDiscord, false) {
		t.Fatal("discord initial budget differs from its reconnect budget")
	}
}

// test_startup_connect_parallel.py TestTelegramColdStartCap.test_env_override_applies_to_initial
func TestTelegramColdStartCapEnvOverrideAppliesToInitial(t *testing.T) {
	t.Setenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT", "12")
	r := coldStartRunner(t)
	if got := r.platformConnectTimeoutSecs(gateway.PlatformTelegram, true); got != 12.0 {
		t.Fatalf("initial = %v", got)
	}
}

// test_startup_connect_parallel.py TestTelegramColdStartCap.test_initial_connect_times_out_at_cap_and_queues_retry
func TestTelegramColdStartCapInitialConnectTimesOutAtCapAndQueuesRetry(t *testing.T) {
	t.Setenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT", "")
	r := parallelConnectRunner(t)
	prev := telegramInitialConnectTimeoutSecsDefault
	telegramInitialConnectTimeoutSecsDefault = 0.2
	t.Cleanup(func() { telegramInitialConnectTimeoutSecsDefault = prev })
	injectRegistryAdapters(t, func(p gateway.Platform) gateway.PlatformAdapter {
		if p == gateway.PlatformTelegram {
			return newTimingAdapter(p, nil, time.Hour, false)
		}
		return newTimingAdapter(p, nil, 0, false)
	}, gateway.PlatformTelegram, gateway.PlatformDiscord)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.adapters.Has(gateway.PlatformDiscord) || r.adapters.Has(gateway.PlatformTelegram) {
		t.Fatalf("adapters = %v", r.adapters.Keys())
	}
	if !r.failedPlatforms.Has(gateway.PlatformTelegram) {
		t.Fatalf("failed platforms = %v", r.failedPlatforms.Keys())
	}
}
