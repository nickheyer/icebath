package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bundled"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pluginservices"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Plugin collaborators of a gateway process, the registries the bundled platform and provider plugins load into
func gatewayPluginServices() *plugins.Services {
	s := &plugins.Services{
		Platforms:          gateway.PlatformRegistryService(),
		ProviderRegistries: pluginservices.ProviderRegistries(),
		BoundSessionKey:    func(ctx context.Context) string { return tools.BoundSessionKey(tools.ToolEnvFrom(ctx)) },
	}
	gateway.InstallPluginServices(s)
	return s
}

// Stores settings in the home of the running case through the settings writer, the route the settings screen takes
func setSettings(t *testing.T, values map[string]any) {
	t.Helper()
	for key, value := range values {
		if _, err := settings.Set(context.Background(), key, value, settings.Global); err != nil {
			t.Fatalf("setting %s: %v", key, err)
		}
	}
}

// Profile id of an authority over a store, the resolved home that store lives in, as a served profile is reserved
func storeHome(t *testing.T, db *state.SessionDB) string {
	t.Helper()
	if db == nil {
		t.Fatal("the session store of the case did not open")
	}
	return filepath.Dir(resolveHome(db.Path()))
}

// Gateway tasks started so far and not yet joined, in start order
var startedTasks struct {
	sync.Mutex
	tasks []*gatewayTask
}

// Cancels and joins the gateway tasks a case started, as a closing event loop does for its tasks
func joinGatewayTasks(t *testing.T) func() {
	startedTasks.Lock()
	mark := len(startedTasks.tasks)
	startedTasks.Unlock()
	return func() {
		startedTasks.Lock()
		tasks := append([]*gatewayTask(nil), startedTasks.tasks[mark:]...)
		startedTasks.tasks = startedTasks.tasks[:mark]
		startedTasks.Unlock()
		for _, task := range tasks {
			task.Cancel()
		}
		deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, task := range tasks {
			task.wait(deadline)
			if !task.isDone() {
				t.Errorf("gateway task %q was still running 30s after the test cancelled it", task.name)
			}
		}
	}
}

// Clears the process wide state a gateway test leaves behind, the per test resets of the test setup
func processState(services *plugins.Services) hermetic.Fixture {
	return func(t *testing.T) func() {
		return func() {
			if !gateway.FlushRuntimeStatus(30 * time.Second) {
				t.Error("gateway runtime status was still being written 30s after the test")
			}
			provider.SetMultiplexActive(false)
			home.PinProcess("", true)
			provider.ResetLaunchEnvSnapshotForTests()
			tools.ResetForegroundExitFenceForTests()
			gateway.SetGatewayRunnerRef(nil)
			plugins.InstallServices(services)
			plugins.ResetPluginManagersForTests()
			logging.ResetQueuedHandlers()
			state.CloseLeakedTestInstances()
			state.CloseAll()
		}
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == migrateGatewayStandInEntry {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	if dir := os.Getenv("ICEBATH_FAULTHANDLER_CHILD"); dir != "" {
		faulthandlerChild(dir)
		os.Exit(0)
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	dir, err := os.MkdirTemp("", "gatewayrun-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	os.Setenv("ICEBATH_TEST_ISOLATION", dir)
	services := gatewayPluginServices()
	plugins.InstallServices(services)
	gatewayTaskStarted = func(task *gatewayTask) {
		startedTasks.Lock()
		startedTasks.tasks = append(startedTasks.tasks, task)
		startedTasks.Unlock()
	}
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, processState(services), joinGatewayTasks)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
