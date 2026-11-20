package acp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func blockedDiscovery(t *testing.T, threadName string) (release func(), done <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	config.ResetCache()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mcp_servers:\n  slow:\n    url: https://mcp.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	block, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(block) }) }
	previous := mcp.SwapDiscoveryPass(func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		select {
		case <-block:
		case <-time.After(5 * time.Second):
		}
		close(finished)
		return nil, nil
	})
	ctx := context.Background()
	t.Cleanup(func() {
		release()
		timeout := 2 * time.Second
		mcp.JoinDiscovery(ctx, &timeout)
		mcp.SwapDiscoveryPass(previous)
	})
	env := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
	started := time.Now()
	mcp.StartBackgroundDiscovery(ctx, env, slog.New(slog.DiscardHandler), threadName)
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("StartBackgroundDiscovery blocked for %s", elapsed)
	}
	return release, finished
}

func TestAcpBackgroundDiscoveryDoesNotBlockStartup(t *testing.T) {
	release, _ := blockedDiscovery(t, "test-acp-discovery")
	if !mcp.DiscoveryInFlight(context.Background()) {
		t.Fatal("discovery is not in flight")
	}
	release()
	timeout := 2 * time.Second
	if !mcp.JoinDiscovery(context.Background(), &timeout) {
		t.Fatal("discovery did not finish")
	}
}
