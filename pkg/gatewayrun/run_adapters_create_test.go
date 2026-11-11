package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestFactoryBindsEveryAdapterToRunner(t *testing.T) {
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	adapter := newRestartTestAdapter()
	var built any = adapter
	entry := gateway.NewPlatformEntry("signal", "Signal", func(context.Context, *gateway.PlatformConfig) (any, error) { return built, nil },
		func() bool { return true })
	if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gateway.Registry.Unregister(ctx, "signal", "") })
	got, err := r.createAdapter(ctx, gateway.PlatformSignal, &gateway.PlatformConfig{Enabled: true})
	if err != nil || got != gateway.PlatformAdapter(adapter) {
		t.Fatalf("createAdapter = %v, %v", got, err)
	}
	if adapter.GatewayRunner != r {
		t.Fatalf("gateway_runner = %v", adapter.GatewayRunner)
	}
	built = nil
	got, err = r.createAdapter(ctx, gateway.PlatformSignal, &gateway.PlatformConfig{Enabled: true})
	if err != nil || got != nil {
		t.Fatalf("createAdapter = %v, %v", got, err)
	}
}
