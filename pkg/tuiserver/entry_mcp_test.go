package tuiserver

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Sets whether this process armed entry discovery for the case and restores the old value after it
func swapDiscoveryEnabled(t *testing.T, enabled bool) {
	t.Helper()
	saved := mcpDiscoveryEnabled.Load()
	mcpDiscoveryEnabled.Store(enabled)
	t.Cleanup(func() { mcpDiscoveryEnabled.Store(saved) })
}

// Records the thread name of every shared discovery start the case triggers
func recordDiscoveryStarts(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	swapVar(t, &entryStartBackgroundDiscovery, func(_ context.Context, _ *tools.Env, _ *slog.Logger, threadName string) {
		*calls = append(*calls, threadName)
	})
	return calls
}

// Empties the shared discovery slot of the case home, or fills it with a run ending when done closes
func swapHomeDiscoveryRun(t *testing.T, done chan struct{}) {
	t.Helper()
	t.Cleanup(mcp.SetHomeDiscoveryRunForTests(context.Background(), done))
}

func TestWaitFallsThroughToSharedOwner(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapDiscoveryEnabled(t, true)
	recordDiscoveryStarts(t)
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()
	swapHomeDiscoveryRun(t, done)
	timeout := 2 * time.Second
	start := time.Now()
	waitForEntryMCPDiscovery(context.Background(), &timeout)
	elapsed := time.Since(start)
	select {
	case <-done:
	default:
		t.Fatal("the shared owner's discovery was not joined")
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("returned after %v", elapsed)
	}
}

func TestWaitNoopWhenNoOwnerHasAThread(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapHomeDiscoveryRun(t, nil)
	timeout := 2 * time.Second
	start := time.Now()
	waitForEntryMCPDiscovery(context.Background(), &timeout)
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("waited %v with no discovery running", elapsed)
	}
}

func TestWaitReinvokesSharedSpawnWhenDiscoveryEnabled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapDiscoveryEnabled(t, true)
	calls := recordDiscoveryStarts(t)
	swapHomeDiscoveryRun(t, nil)
	timeout := 100 * time.Millisecond
	waitForEntryMCPDiscovery(context.Background(), &timeout)
	if !reflect.DeepEqual(*calls, []string{"tui-mcp-discovery"}) {
		t.Fatalf("starts = %v", *calls)
	}
}

func TestWaitSkipsSpawnWhenDiscoveryNotEnabled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapDiscoveryEnabled(t, false)
	calls := recordDiscoveryStarts(t)
	swapHomeDiscoveryRun(t, nil)
	timeout := 100 * time.Millisecond
	waitForEntryMCPDiscovery(context.Background(), &timeout)
	if len(*calls) != 0 {
		t.Fatalf("starts = %v", *calls)
	}
}
