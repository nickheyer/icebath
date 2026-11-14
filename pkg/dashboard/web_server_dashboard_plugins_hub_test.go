package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session client over a home whose enabled user plugin demo provides demo_tool, the tool gated by check
func hubDemoClient(t *testing.T, check func(*tools.Env) bool) sessionClient {
	t.Helper()
	c := newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	writeTestFile(t, filepath.Join(h, "plugins", "demo", "plugin.yaml"),
		"name: demo\nversion: 1.0.0\ndescription: demo plugin\nprovides_tools:\n- demo_tool\n")
	writeTestFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - demo\ndashboard:\n  hidden_plugins: []\n")
	resetDashboardPluginCache(t)
	tools.Default.Register(tools.Def{Name: "demo_tool", Toolset: "demo",
		Schema:  map[string]any{"name": "demo_tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		Handler: func(context.Context, *tools.Call) string { return "ok" }, Check: check})
	t.Cleanup(func() {
		if err := tools.Default.Deregister("demo_tool", "", ""); err != nil {
			t.Errorf("deregister demo_tool: %v", err)
		}
		tools.InvalidateCheckCache()
		invalidatePluginsHubCache()
	})
	tools.InvalidateCheckCache()
	invalidatePluginsHubCache()
	return c
}

// Hub row of the named plugin, failing the case when the payload lacks it
func hubRow(t *testing.T, payload *pyjson.Object, name string) *pyjson.Object {
	t.Helper()
	rows, _ := payload.GetOr("plugins", nil).([]any)
	for _, r := range rows {
		if row := r.(*pyjson.Object); row.GetOr("name", nil) == name {
			return row
		}
	}
	t.Fatalf("hub payload has no plugin %s", name)
	return nil
}

// Freshly built hub payload of the launch home
func rebuiltHub(t *testing.T) *pyjson.Object {
	t.Helper()
	payload, err := mergedPluginsHub(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// Identity of the calling goroutine as the runtime prints it
func goroutineID() string {
	buf := make([]byte, 64)
	fields := strings.Fields(string(buf[:runtime.Stack(buf, false)]))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func TestPluginsHubDoesNotProbeColdCheckFns(t *testing.T) {
	var mu sync.Mutex
	probedOn := map[string]bool{}
	hubDemoClient(t, func(*tools.Env) bool {
		mu.Lock()
		probedOn[goroutineID()] = true
		mu.Unlock()
		return false
	})
	probes := trackProbes(t)
	row := hubRow(t, rebuiltHub(t), "demo")
	if row.GetOr("auth_required", nil) != false || row.GetOr("auth_command", nil) != "" {
		t.Fatalf("row %v", pyjson.Plain(row))
	}
	waitProbes(t, probes)
	mu.Lock()
	defer mu.Unlock()
	if probedOn[goroutineID()] {
		t.Fatal("the request path ran the probe")
	}
}

// Records the completion channel of every scheduled probe
func trackProbes(t *testing.T) *[]<-chan struct{} {
	t.Helper()
	scheduled := &[]<-chan struct{}{}
	real := hubScheduleCheckFnProbe
	hubScheduleCheckFnProbe = func(fn func(*tools.Env) bool) <-chan struct{} {
		ch := real(fn)
		*scheduled = append(*scheduled, ch)
		return ch
	}
	t.Cleanup(func() { hubScheduleCheckFnProbe = real })
	return scheduled
}

// Waits for every scheduled probe to finish
func waitProbes(t *testing.T, scheduled *[]<-chan struct{}) {
	t.Helper()
	for _, ch := range *scheduled {
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("probe did not finish")
		}
	}
}

func TestPluginsHubColdCacheSchedulesBackgroundProbe(t *testing.T) {
	probeRan := make(chan struct{}, 8)
	hubDemoClient(t, func(*tools.Env) bool {
		probeRan <- struct{}{}
		return false
	})
	scheduled := trackProbes(t)
	payload := rebuiltHub(t)
	if hubRow(t, payload, "demo").GetOr("auth_required", nil) != false {
		t.Fatalf("row %v", pyjson.Plain(hubRow(t, payload, "demo")))
	}
	if len(*scheduled) == 0 || (*scheduled)[0] == nil {
		t.Fatalf("no probe scheduled: %v", *scheduled)
	}
	waitProbes(t, scheduled)
	select {
	case <-probeRan:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never ran")
	}
	refreshed := hubRow(t, rebuiltHub(t), "demo")
	if refreshed.GetOr("auth_required", nil) != true || refreshed.GetOr("auth_command", nil) != "icebath auth demo" {
		t.Fatalf("row %v", pyjson.Plain(refreshed))
	}
}

func TestPluginsHubUsesCachedFailedCheckFnVerdict(t *testing.T) {
	check := func(*tools.Env) bool { return false }
	hubDemoClient(t, check)
	if tools.Default.CheckCached(context.Background(), &tools.Env{}, check) {
		t.Fatal("check answered true")
	}
	row := hubRow(t, rebuiltHub(t), "demo")
	if row.GetOr("auth_required", nil) != true || row.GetOr("auth_command", nil) != "icebath auth demo" {
		t.Fatalf("row %v", pyjson.Plain(row))
	}
}

func TestPluginsHubRouteBuildsCatalogAnnotationsOffEventLoop(t *testing.T) {
	c := hubDemoClient(t, func(*tools.Env) bool { return true })
	dir := filepath.Join(t.TempDir(), "catalog")
	writeTestFile(t, filepath.Join(dir, "removed.yaml"), "removed:\n- name: demo\n  repo: https://github.com/example/demo\n"+
		"  reason: withdrawn by catalog\n")
	t.Cleanup(plugins.SwapCatalogDir(dir))
	resp := c.get("/api/dashboard/plugins/hub")
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	for _, r := range jsonObject(t, resp)["plugins"].([]any) {
		if row := r.(map[string]any); row["name"] == "demo" {
			if row["removed_reason"] != "withdrawn by catalog" {
				t.Fatalf("row %v", row)
			}
			return
		}
	}
	t.Fatalf("hub lists no demo plugin: %s", resp.Body)
}

func TestPluginInstallEndpointInvalidatesHubCache(t *testing.T) {
	c := hubDemoClient(t, func(*tools.Env) bool { return true })
	primed := rebuiltHub(t)
	cached, err := mergedPluginsHub(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cached != primed {
		t.Fatal("a plain fetch rebuilt the hub")
	}
	real := uiDashboardInstallPlugin
	uiDashboardInstallPlugin = func(context.Context, plugins.DashboardInstallOptions) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}
	t.Cleanup(func() { uiDashboardInstallPlugin = real })
	if resp := c.send("POST", "/api/dashboard/agent-plugins/install", map[string]any{"identifier": "demo"}); resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	rebuilt, err := mergedPluginsHub(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt == primed {
		t.Fatal("the install left the memoized hub in place")
	}
}

// Transport of a dead catalog host that counts every attempt
type unreachableCatalog struct{ attempts atomic.Int64 }

func (u *unreachableCatalog) RoundTrip(*http.Request) (*http.Response, error) {
	u.attempts.Add(1)
	return nil, errors.New("catalog host unreachable")
}

func TestHubRebuildAndPluginsListResolveTheKillListOnce(t *testing.T) {
	newSessionClient(t)
	h := os.Getenv("ICEBATH_HOME")
	for name, version := range map[string]string{"demo": "1.0.0", "second": "0.2.0", "third": "0.3.0"} {
		writeTestFile(t, filepath.Join(h, "plugins", name, "plugin.yaml"), "name: "+name+"\nversion: "+version+"\ndescription: "+name+" plugin\n")
	}
	writeTestFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - demo\ndashboard:\n  hidden_plugins: []\n")
	resetDashboardPluginCache(t)
	unreachable := &unreachableCatalog{}
	t.Cleanup(plugins.SwapLiveCatalogClient(&http.Client{Transport: unreachable}))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "removed.yaml"), "removed:\n- name: demo\n  reason: exfiltrated env vars\n")
	t.Cleanup(plugins.SwapCatalogDir(dir))
	tools.InvalidateCheckCache()
	invalidatePluginsHubCache()
	t.Cleanup(invalidatePluginsHubCache)
	want := map[string]any{"demo": "exfiltrated env vars", "second": nil, "third": nil}
	byName := map[string]any{}
	for _, r := range rebuiltHub(t).GetOr("plugins", nil).([]any) {
		row := r.(*pyjson.Object)
		name, _ := row.GetOr("name", nil).(string)
		if _, listed := want[name]; listed {
			byName[name] = row.GetOr("removed_reason", nil)
		} else if row.GetOr("removed_reason", nil) != nil {
			t.Fatalf("row %s carries a removal", name)
		}
	}
	if !reflect.DeepEqual(byName, want) {
		t.Fatalf("hub removals %v", byName)
	}
	if got := unreachable.attempts.Load(); got != 1 {
		t.Fatalf("hub rebuild made %d network attempts", got)
	}
	plugins.ForgetLiveCatalogFailure()
	var out bytes.Buffer
	if err := plugins.CmdList(context.Background(), plugins.ListOptions{JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	listed, err := pyjson.LoadsBytes(out.Bytes())
	if err != nil {
		t.Fatalf("list output %q: %v", out.String(), err)
	}
	rows := map[string]any{}
	for _, r := range pyjson.Plain(listed).([]any) {
		row := r.(map[string]any)
		if name, _ := row["name"].(string); name == "demo" || name == "second" || name == "third" {
			rows[name] = row["removed"]
		}
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("list removals %v", rows)
	}
	if got := unreachable.attempts.Load(); got != 2 {
		t.Fatalf("listing made %d network attempts in all", got)
	}
}

const scopedprovInit = `
from agent.memory_provider import MemoryProvider
from agent.secret_scope import get_secret


class ScopedProvMemoryProvider(MemoryProvider):
    @property
    def name(self):
        return "scopedprov"

    def is_available(self):
        # A credentialed provider: availability IS a scoped secret read.
        return bool(get_secret("SCOPEDPROV_API_KEY"))

    def initialize(self, session_id, **kwargs):
        pass

    def get_tool_schemas(self):
        return []
`
