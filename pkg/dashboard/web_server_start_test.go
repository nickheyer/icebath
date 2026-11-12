package dashboard

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const launchFreezeProbeKey = "LAUNCH_FREEZE_PROBE_TOKEN"

// Replaces the updater hook the command wiring installs, the dashboard test binary carrying no updater
func installProxyEnvRestore(t *testing.T) {
	t.Helper()
	previous := RestoreUserProxyEnv
	RestoreUserProxyEnv = func(map[string]string) {}
	t.Cleanup(func() { RestoreUserProxyEnv = previous })
}

func TestBootTimeCredentialInjectionIsInsideTheFrozenLaunchEnv(t *testing.T) {
	app := dashboardTestApp(t)
	installProxyEnvRestore(t)
	provider.ResetLaunchEnvSnapshotForTests()
	t.Cleanup(provider.ResetLaunchEnvSnapshotForTests)
	if previous, set := os.LookupEnv(launchFreezeProbeKey); set {
		os.Unsetenv(launchFreezeProbeKey)
		t.Cleanup(func() { os.Setenv(launchFreezeProbeKey, previous) })
	} else {
		t.Cleanup(func() { os.Unsetenv(launchFreezeProbeKey) })
	}
	second := filepath.Join(os.Getenv("ICEBATH_HOME"), "profiles", "b")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevGate, prevBuild, prevConflict, prevServe := startConfigureAuthGate, startBuildUvicornServer, startPortBindConflict, startRunServe
	t.Cleanup(func() {
		startConfigureAuthGate, startBuildUvicornServer, startPortBindConflict, startRunServe = prevGate, prevBuild, prevConflict, prevServe
	})
	startConfigureAuthGate = func(context.Context, *FastAPI, string, bool, *string, *string) error {
		return os.Setenv(launchFreezeProbeKey, "injected-during-boot")
	}
	startBuildUvicornServer = func(context.Context, *FastAPI, string, int, bool) (*UvicornConfig, *UvicornServer) { return nil, nil }
	startPortBindConflict = func(string, int) bool { return false }
	served := false
	startRunServe = func(func() error, gateway.DashboardServerOptions) error {
		served = true
		return nil
	}
	ctx := context.Background()
	err := startServer(ctx, app, gateway.DashboardServerOptions{Host: "127.0.0.1", Port: 9119, Headless: true,
		Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !served {
		t.Fatal("start_server never reached the serve step")
	}
	if !provider.IsMultiplexActive(ctx) {
		t.Error("multi profile hosting was not activated")
	}
	if got := provider.CaptureLaunchEnv()[launchFreezeProbeKey]; got != "injected-during-boot" {
		t.Errorf("the launch env was frozen before boot finished injecting credentials: %q", got)
	}
}

// Runs a headless server start on an ephemeral loopback port that stops once its last ready line is written
func startServerUntilReady(t *testing.T, app *FastAPI, afterBind bool, onSentinel func()) {
	t.Helper()
	prevBuild, prevSentinel, prevConflict := startBuildUvicornServer, startWriteSentinelLine, startPortBindConflict
	t.Cleanup(func() {
		startBuildUvicornServer, startWriteSentinelLine, startPortBindConflict = prevBuild, prevSentinel, prevConflict
	})
	startPortBindConflict = func(string, int) bool { return false }
	var server *UvicornServer
	startBuildUvicornServer = func(ctx context.Context, app *FastAPI, host string, port int, sshIsolated bool) (*UvicornConfig, *UvicornServer) {
		cfg, built := prevBuild(ctx, app, host, port, sshIsolated)
		server = built
		return cfg, built
	}
	written := 0
	startWriteSentinelLine = func(string, io.Writer) {
		onSentinel()
		if written++; written == 2 {
			server.ShouldExit.Store(true)
		}
	}
	err := startServer(context.Background(), app, gateway.DashboardServerOptions{Host: "127.0.0.1", Port: 0, Headless: true,
		StartMCPDiscoveryAfterBind: afterBind, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDesktopServeArmsMcpDiscoveryOnlyAfterReadySentinel(t *testing.T) {
	app := dashboardTestApp(t)
	installProxyEnvRestore(t)
	mcp.ResetDiscoveryStateForTests()
	t.Cleanup(mcp.ResetDiscoveryStateForTests)
	var mu sync.Mutex
	var order []string
	record := func(event string) {
		mu.Lock()
		order = append(order, event)
		mu.Unlock()
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(order)
	}
	t.Cleanup(mcp.ReplaceBackgroundDiscoveryStartForTests(func(_ context.Context, _ *tools.Env, _ *slog.Logger, threadName string) {
		record("discovery:" + threadName)
	}))
	startServerUntilReady(t, app, true, func() { record("sentinel") })
	armed, clocked := mcp.DeferredDiscoveryForTests()
	if got := seen(); !slices.Equal(got, []string{"sentinel", "sentinel"}) || !armed || !clocked {
		t.Fatalf("order = %v, armed = %v, clocked = %v", got, armed, clocked)
	}
	mcp.CancelDeferredDiscoveryTimerForTests()
	immediately := time.Duration(0)
	mcp.WaitForDiscovery(context.Background(), nil, &immediately, false)
	if got := seen(); !slices.Equal(got, []string{"sentinel", "sentinel", "discovery:dashboard-mcp-discovery"}) {
		t.Fatalf("order = %v", got)
	}
	if armed, _ := mcp.DeferredDiscoveryForTests(); armed {
		t.Error("the deferred start stayed armed")
	}

	mu.Lock()
	order = nil
	mu.Unlock()
	mcp.ResetDiscoveryStateForTests()
	startServerUntilReady(t, app, false, func() { record("sentinel") })
	if got := seen(); !slices.Equal(got, []string{"sentinel", "sentinel"}) {
		t.Fatalf("order = %v", got)
	}
	if armed, _ := mcp.DeferredDiscoveryForTests(); armed {
		t.Error("a serve without the flag armed discovery")
	}
}
