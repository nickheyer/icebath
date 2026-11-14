package main

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestStandaloneDashboardBootArmsDiscoveryWithoutStartingIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DESKTOP", "")
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	mcp.ResetDiscoveryStateForTests()
	t.Cleanup(mcp.ResetDiscoveryStateForTests)
	var mu sync.Mutex
	var calls []string
	t.Cleanup(mcp.ReplaceBackgroundDiscoveryStartForTests(func(_ context.Context, _ *tools.Env, _ *slog.Logger, threadName string) {
		mu.Lock()
		calls = append(calls, threadName)
		mu.Unlock()
	}))
	afterBind, err := dashboardPrepareRuntime(context.Background(), false, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	started := slices.Clone(calls)
	mu.Unlock()
	if afterBind || len(started) != 0 {
		t.Fatalf("after_bind = %v, calls = %v", afterBind, started)
	}
	mcp.StartDeferredDiscoveryNow()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"dashboard-mcp-discovery"}) {
		t.Fatalf("calls = %v", calls)
	}
}
