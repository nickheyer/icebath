package gatewayrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Relay adapter recording dormancy calls, refusing the going idle ack
type dormancyRelayAdapter struct {
	*gateway.BasePlatformAdapter
	mu    sync.Mutex
	calls []string
}

func (a *dormancyRelayAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *dormancyRelayAdapter) Disconnect(context.Context) error            { return nil }
func (a *dormancyRelayAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *dormancyRelayAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *dormancyRelayAdapter) HoldRedial() bool    { return true }
func (a *dormancyRelayAdapter) ReleaseRedial() bool { return true }
func (a *dormancyRelayAdapter) GoDormant(context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "go_dormant")
	return false, nil
}

// Dormancy calls recorded so far
func (a *dormancyRelayAdapter) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// Enabled relay adapter of the given platform
func newDormancyRelayAdapter(platform gateway.Platform) *dormancyRelayAdapter {
	a := &dormancyRelayAdapter{}
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, platform, a)
	return a
}

// Gateway config with only an enabled relay platform
func relayOnlyConfig() *gateway.GatewayConfig {
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled = true
	return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformRelay: cfg}}
}

func TestScaleToZeroGateAccountsForSecondaryProfileDirectAdapter(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(gateway.ScaleToZeroEnv, "1")
	t.Setenv("GATEWAY_RELAY_WAKE_URL", "https://wake.example.test/instance")
	r := allocGatewayRunner()
	r.config = relayOnlyConfig()
	r.adapters = adapterDict(gateway.PlatformRelay, newDormancyRelayAdapter(gateway.PlatformRelay))
	r.profileAdapters = profileAdapterDict("imessage", adapterDict(gateway.Platform("photon"), newDormancyRelayAdapter(gateway.Platform("photon"))))
	if r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("a secondary profile's direct adapter must keep the gateway awake")
	}
	r.profileAdapters = profileAdapterDict()
	r.profileFailedPlatforms = map[string]map[gateway.Platform]*gatewayTask{"imessage": {gateway.Platform("photon"): nil}}
	if r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("a secondary platform mid reconnect must keep the gateway awake")
	}
}

func TestWatcherReasksGateBeforeDormantSequence(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(gateway.SleepURLEnv, "https://sleep.example.test/instance")
	newRunner := func() (*GatewayRunner, *dormancyRelayAdapter) {
		relay := newDormancyRelayAdapter(gateway.PlatformRelay)
		r := allocGatewayRunner()
		r.config = relayOnlyConfig()
		r.adapters = adapterDict(gateway.PlatformRelay, relay)
		r.profileAdapters = profileAdapterDict()
		r.running.Store(true)
		r.backgroundTasks = map[*gatewayTask]struct{}{}
		r.lastInboundAt = wallClock() - 3600
		return r, relay
	}
	oneTick := func(r *GatewayRunner) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = r.scaleToZeroWatcher(ctx, 0.0)
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
	}
	hotAdded, hotRelay := newRunner()
	hotAdded.profileAdapters = profileAdapterDict("imessage", adapterDict(gateway.Platform("photon"), newDormancyRelayAdapter(gateway.Platform("photon"))))
	oneTick(hotAdded)
	if calls := hotRelay.recorded(); len(calls) != 0 {
		t.Fatalf("hot added direct adapter still went dormant: %v", calls)
	}
	relayOnly, relay := newRunner()
	oneTick(relayOnly)
	if calls := relay.recorded(); len(calls) == 0 || calls[0] != "go_dormant" {
		t.Fatalf("relay only tick calls = %v", calls)
	}
}

// Restart runner whose process home is a fresh temp dir
func runnerWithHome(t *testing.T) (*GatewayRunner, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(home.EnvHome, dir)
	r, _ := makeRestartRunner(t)
	return r, dir
}

// Decoded restart failure counters file
func readStuckCounts(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]any{}
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestIncrementCreatesFile(t *testing.T) {
	r, dir := runnerWithHome(t)
	r.incrementRestartFailureCounts(context.Background(), []string{"session:a", "session:b"})
	path := filepath.Join(dir, stuckLoopFile)
	counts := readStuckCounts(t, path)
	if counts["session:a"] != float64(1) || counts["session:b"] != float64(1) {
		t.Fatalf("counts = %v", counts)
	}
}

func TestNoSuspendBelowThreshold(t *testing.T) {
	r, _ := runnerWithHome(t)
	r.incrementRestartFailureCounts(context.Background(), []string{"session:a"})
	r.incrementRestartFailureCounts(context.Background(), []string{"session:a"})
	if n := r.suspendStuckLoopSessions(context.Background()); n != 0 {
		t.Fatalf("suspended %d", n)
	}
}

func TestConcurrentClearsDoNotResurrectEachOthersCounter(t *testing.T) {
	r, dir := runnerWithHome(t)
	path := filepath.Join(dir, stuckLoopFile)
	if err := os.WriteFile(path, []byte(`{"session:a": 2, "session:b": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, key := range []string{"session:a", "session:b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.clearRestartFailureCount(context.Background(), key)
		}()
	}
	wg.Wait()
	remaining := map[string]any{}
	if _, err := os.Stat(path); err == nil {
		remaining = readStuckCounts(t, path)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v", remaining)
	}
}

// Bare running runner whose last inbound was at lastInboundAt, its home a fresh temp dir
func idleRunner(t *testing.T, lastInboundAt float64) *GatewayRunner {
	t.Helper()
	r := allocGatewayRunner()
	r.running.Store(true)
	r.lastInboundAt = lastInboundAt
	r.backgroundTasks = map[*gatewayTask]struct{}{}
	r.adapters = adapterDict()
	return r
}

// Touches the dashboard marker and moves its mtime ago seconds into the past
func touchMarkerAgo(t *testing.T, ago float64) {
	t.Helper()
	gateway.TouchDashboardClientHeartbeat("")
	if ago == 0 {
		return
	}
	at := time.Now().Add(-time.Duration(ago * float64(time.Second)))
	if err := os.Chtimes(gateway.DashboardClientHeartbeatPath(""), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestIdleWithoutDashboardClientUnchanged(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	if !idleRunner(t, wallClock()-600).scaleToZeroIsIdle(context.Background()) {
		t.Fatal("no client and old inbound must be idle")
	}
}

func TestAttachedDashboardClientBlocksIdle(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 0)
	if r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("an attached client must block idle")
	}
}

func TestClientGetsTheSameIdleGraceAsAMessage(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 100)
	if r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("a frame 100s ago is inside the 120s window")
	}
}

func TestClientGoneLongerThanIdleTimeoutIsIdle(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 121)
	if !r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("a client gone past the timeout must be idle")
	}
}

func TestMarkerPredatingGatewayInboundDoesNotMatter(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 7200)
	if !r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("an ancient marker must not keep the gateway awake")
	}
}

func TestDashboardClientSeenRecentlyExtendsInboundClock(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 30)
	if r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("a marker 30s old moves the inbound clock inside the window")
	}
}

func TestNewerGatewayInboundWinsOverOlderMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(home.EnvHome, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("gateway:\n  scale_to_zero:\n    idle_timeout_minutes: 0.16666666666666666\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := idleRunner(t, wallClock()-5)
	touchMarkerAgo(t, 40)
	if r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("the newer chat message must win over the older marker")
	}
	if wallClock()-r.lastInboundAt >= 10 {
		t.Fatal("the read must not move the inbound clock")
	}
}

func TestUnreadableMarkerKeepsGatewayAwake(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root stats through denied directories")
	}
	t.Setenv(home.EnvHome, t.TempDir())
	r := idleRunner(t, wallClock()-600)
	touchMarkerAgo(t, 0)
	dir := filepath.Dir(gateway.DashboardClientHeartbeatPath(""))
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("an unreadable marker must keep the gateway awake")
	}
}

// Runner whose config holds the given platforms with their enabled flags, scale to zero opted in with a wake URL
func armRunner(t *testing.T, states map[gateway.Platform]bool) *GatewayRunner {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(gateway.ScaleToZeroEnv, "1")
	t.Setenv("GATEWAY_RELAY_WAKE_URL", "https://wake.example")
	r := allocGatewayRunner()
	r.config = &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
	for platform, enabled := range states {
		cfg := gateway.NewPlatformConfig()
		cfg.Enabled = enabled
		r.config.Platforms[platform] = cfg
	}
	return r
}

func TestArmTrueForRelayOnlyWithDisabledPlaceholders(t *testing.T) {
	r := armRunner(t, map[gateway.Platform]bool{gateway.PlatformTelegram: false, gateway.PlatformDiscord: false,
		gateway.PlatformSlack: false, gateway.PlatformMatrix: false, gateway.PlatformRelay: true})
	if !r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("disabled placeholders must not disarm")
	}
}

func TestNoArmWhenADirectPlatformIsActuallyEnabled(t *testing.T) {
	r := armRunner(t, map[gateway.Platform]bool{gateway.PlatformDiscord: true, gateway.PlatformRelay: true})
	if r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("an enabled direct platform must disarm")
	}
}

func TestArmTrueWithAPIServerEnabled(t *testing.T) {
	r := armRunner(t, map[gateway.Platform]bool{gateway.PlatformRelay: true, gateway.PlatformAPIServer: true, gateway.PlatformTelegram: false})
	if !r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("the api server is not a messaging socket")
	}
}

func TestArmTrueWithAllNonMessagingSurfacesEnabled(t *testing.T) {
	r := armRunner(t, map[gateway.Platform]bool{gateway.PlatformRelay: true, gateway.PlatformAPIServer: true,
		gateway.PlatformWebhook: true, gateway.PlatformLocal: true})
	if !r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("non messaging surfaces must not disarm")
	}
}

func TestDirectPlatformStillDisarmsAlongsideAPIServer(t *testing.T) {
	r := armRunner(t, map[gateway.Platform]bool{gateway.PlatformRelay: true, gateway.PlatformAPIServer: true, gateway.PlatformDiscord: true})
	if r.scaleToZeroShouldArm(context.Background()) {
		t.Fatal("a direct platform must still disarm next to the api server")
	}
}

func TestSupervisedWatchersDoNotBlockIdle(t *testing.T) {
	r := allocGatewayRunner()
	r.running.Store(true)
	r.backgroundTasks = map[*gatewayTask]struct{}{}
	forever := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	var tasks []*gatewayTask
	for _, name := range []string{"session_expiry", "kanban", "scale_to_zero_watcher"} {
		tasks = append(tasks, r.spawnSupervised(forever, name, false, 0, supervisedHooks{}))
	}
	defer func() {
		for _, task := range tasks {
			task.Cancel()
		}
	}()
	if r.scaleToZeroHasLiveBackgroundWork() {
		t.Fatal("supervised watchers must not count as background work")
	}
}

func TestTransientBackgroundTaskStillBlocksIdle(t *testing.T) {
	r := allocGatewayRunner()
	r.running.Store(true)
	task := spawnGatewayTask(context.Background(), "work", func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer task.Cancel()
	r.backgroundTasks = map[*gatewayTask]struct{}{task: {}}
	if !r.scaleToZeroHasLiveBackgroundWork() {
		t.Fatal("a plain task must count as background work")
	}
}

func TestDoneSupervisedWatcherIsIgnoredEitherWay(t *testing.T) {
	r := allocGatewayRunner()
	r.running.Store(true)
	task := spawnGatewayTask(context.Background(), "quick", func(context.Context) (any, error) { return nil, nil })
	_, _ = task.wait(context.Background())
	r.backgroundTasks = map[*gatewayTask]struct{}{task: {}}
	if r.scaleToZeroHasLiveBackgroundWork() {
		t.Fatal("a finished task is not background work")
	}
}

// API server adapter reporting a fixed live agent work count, or panicking when wedged
type workCountAPIAdapter struct {
	*dormancyRelayAdapter
	count  int
	wedged bool
}

func (a *workCountAPIAdapter) ActiveAgentWorkCount() int {
	if a.wedged {
		panic("adapter wedged")
	}
	return a.count
}

// Running runner quiet for hours with an api server adapter reporting apiRuns
func workCountRunner(t *testing.T, apiRuns int, wedged bool) *GatewayRunner {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	r := allocGatewayRunner()
	r.running.Store(true)
	r.backgroundTasks = map[*gatewayTask]struct{}{}
	r.lastInboundAt = 0.0
	r.adapters = adapterDict(gateway.PlatformAPIServer, &workCountAPIAdapter{dormancyRelayAdapter: newDormancyRelayAdapter(gateway.PlatformAPIServer), count: apiRuns, wedged: wedged})
	return r
}

func TestActiveAPIRunBlocksIdle(t *testing.T) {
	if workCountRunner(t, 1, false).scaleToZeroIsIdle(context.Background()) {
		t.Fatal("an api run is work")
	}
}

func TestIdleTrueWhenAllWorkSourcesQuiet(t *testing.T) {
	if !workCountRunner(t, 0, false).scaleToZeroIsIdle(context.Background()) {
		t.Fatal("all sources quiet must be idle")
	}
}

func TestUnreadableAPISourceFailsAwake(t *testing.T) {
	if workCountRunner(t, 0, true).scaleToZeroIsIdle(context.Background()) {
		t.Fatal("an unreadable api source must keep the gateway awake")
	}
}

func TestMissingAPIAdapterIsNotWork(t *testing.T) {
	r := workCountRunner(t, 0, false)
	r.adapters = adapterDict()
	if !r.scaleToZeroIsIdle(context.Background()) {
		t.Fatal("no api adapter is not work")
	}
}

func TestDrainSuppressSkipsHomeChannelKeepsSessionPing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(home.EnvHome, dir)
	r, adapter := makeRestartRunner(t)
	r.config.Platforms[gateway.PlatformTelegram].HomeChannel = &gateway.HomeChannel{Platform: gateway.PlatformTelegram, ChatID: "home-42", Name: "Ops Home"}
	r.sessionState("agent:main:telegram:dm:999").turn.agent = &agent.AIAgent{}
	if _, err := gateway.WriteDrainRequest(context.Background(), "nas", true, dir); err != nil {
		t.Fatal(err)
	}
	r.notifyActiveSessionsOfShutdown(context.Background())
	calls := adapter.calls()
	if len(calls) != 1 {
		t.Fatalf("sent %d notices: %v", len(calls), calls)
	}
	if calls[0].chatID != "999" {
		t.Fatalf("sent to %s", calls[0].chatID)
	}
	if !strings.Contains(calls[0].content, "shutting down") {
		t.Fatalf("content = %q", calls[0].content)
	}
}

// Agent whose last activity is now
func touchedAgent() *agent.AIAgent {
	a := &agent.AIAgent{}
	a.TouchActivity("turn")
	return a
}

// Agent whose activity clock reads idle for the given seconds
func idleAgent(idleSeconds float64) *agent.AIAgent {
	a := &agent.AIAgent{}
	a.SetLastActivityTS(float64(time.Now().UnixNano())/1e9 - idleSeconds)
	return a
}

func TestWedgedAgentCountDisabledTimeoutCountsNothing(t *testing.T) {
	t.Setenv("ICEBATH_AGENT_TIMEOUT", "0")
	r, _ := makeRestartRunner(t)
	r.sessionState("agent:main:telegram:dm:1").turn.agent = touchedAgent()
	if n := r.wedgedAgentCount(context.Background()); n != 0 {
		t.Fatalf("wedged = %d", n)
	}
}

func TestWedgedAgentCountIgnoresSentinelsAndBadSummaries(t *testing.T) {
	t.Setenv("ICEBATH_AGENT_TIMEOUT", "")
	os.Unsetenv("ICEBATH_AGENT_TIMEOUT")
	r, _ := makeRestartRunner(t)
	r.sessionState("pending").turn.agent = agentPendingSentinel
	r.sessionState("no_summary").turn.agent = &agent.AIAgent{}
	r.sessionState("wedged").turn.agent = idleAgent(4000)
	r.sessionState("live").turn.agent = idleAgent(1)
	if n := r.wedgedAgentCount(context.Background()); n != 1 {
		t.Fatalf("wedged = %d", n)
	}
}
