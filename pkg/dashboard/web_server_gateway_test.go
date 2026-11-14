package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestWindows11BuildDisplaysAsWindows11(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	info := displaySystemPlatform("Windows", "10", "10.0.26200", "Windows-10-10.0.26200-SP0")
	if info.Vals["os"] != "Windows" || info.Vals["os_release"] != "11" || info.Vals["os_version"] != "10.0.26200" ||
		info.Vals["platform"] != "Windows-11-10.0.26200-SP0" {
		t.Fatalf("info %v", info.Vals)
	}
}

func TestNonWindowsPlatformUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	info := displaySystemPlatform("Linux", "6.8.0", "#1 SMP", "Linux-6.8.0-x86_64-with-glibc2.39")
	want := pyjson.ObjectOf("os", "Linux", "os_release", "6.8.0", "os_version", "#1 SMP", "platform", "Linux-6.8.0-x86_64-with-glibc2.39")
	if !pyjson.Equal(info, want) {
		t.Fatalf("info %v", info.Vals)
	}
}

func TestNoRuntimePlatformsReturnsEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	if got := profilePlatformPorts(context.Background(), dir, nil); got.Len() != 0 {
		t.Fatalf("got %v", got.Vals)
	}
	if got := profilePlatformPorts(context.Background(), dir, pyjson.ObjectOf("platforms", pyjson.NewObject())); got.Len() != 0 {
		t.Fatalf("got %v", got.Vals)
	}
}

func TestTopLevelPlatformsWinsOverGatewayBlock(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("gateway:\n  platforms:\n    webhook:\n      port: 1111\n"+
		"platforms:\n  webhook:\n    port: 2222\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := pyjson.ObjectOf("platforms", pyjson.ObjectOf("webhook", pyjson.ObjectOf("state", "connected")))
	if got := profilePlatformPorts(context.Background(), dir, runtime); !pyjson.Equal(got, pyjson.ObjectOf("webhook", 2222)) {
		t.Fatalf("got %v", got.Vals)
	}
}

func TestDeadPlatformStatesExcluded(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	runtime := pyjson.ObjectOf("platforms", pyjson.ObjectOf(
		"webhook", pyjson.ObjectOf("state", "fatal"),
		"api_server", pyjson.ObjectOf("state", "disconnected"),
		"msgraph_webhook", pyjson.ObjectOf("state", "connected")))
	if got := profilePlatformPorts(context.Background(), t.TempDir(), runtime); !pyjson.Equal(got, pyjson.ObjectOf("msgraph_webhook", 8646)) {
		t.Fatalf("got %v", got.Vals)
	}
}

func TestDashboardStandaloneRefusalResolvesOnceAndPreservesInvalidProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hermes")
	coder := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(coder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("model:\n  default: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coder, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", coder)
	savedResolve, savedMux := gatewayResolveProfileDir, profileIsMultiplexed
	t.Cleanup(func() { gatewayResolveProfileDir, profileIsMultiplexed = savedResolve, savedMux })
	var resolved []string
	gatewayResolveProfileDir = func(name string) (string, error) {
		resolved = append(resolved, name)
		if name != "coder" {
			return "", NewHTTPException(404, "Unknown profile")
		}
		return coder, nil
	}
	ctx := context.Background()
	profileIsMultiplexed = func(string) bool { return true }
	refusal, err := multiplexedProfileRefusal(ctx, "coder", "start")
	if err != nil || !strings.Contains(refusal, "rescan-profiles") {
		t.Fatalf("refusal %q err %v", refusal, err)
	}
	if !slices.Equal(resolved, []string{"coder"}) {
		t.Fatalf("resolved %v", resolved)
	}
	profileIsMultiplexed = func(string) bool { return false }
	if refusal, err := multiplexedProfileRefusal(ctx, "coder", "start"); err != nil || refusal != "" {
		t.Fatalf("refusal %q err %v", refusal, err)
	}
	_, err = multiplexedProfileRefusal(ctx, "missing", "stop")
	var he *HTTPException
	if !errors.As(err, &he) || he.StatusCode != 404 {
		t.Fatalf("err %v", err)
	}
}

// Empties the topology cache
func resetTopologyCache() {
	topologyCacheMu.Lock()
	topologyCache.data, topologyCache.fn = nil, 0
	topologyCache.ts = time.Time{}
	topologyCacheMu.Unlock()
}

// Collector counting its scans after an optional delay
func fakeTopology(calls *atomic.Int32, delay time.Duration) func(context.Context) *pyjson.Object {
	return func(context.Context) *pyjson.Object {
		if delay > 0 {
			time.Sleep(delay)
		}
		calls.Add(1)
		return pyjson.ObjectOf("profiles", []any{"default"}, "gateway_mode", "single", "gateways", []any{})
	}
}

// Swaps the collector for the case and clears the cache around it
func swapTopologyCollector(t *testing.T, fn func(context.Context) *pyjson.Object) {
	saved := collectProfileGatewayTopology
	collectProfileGatewayTopology = fn
	resetTopologyCache()
	t.Cleanup(func() {
		collectProfileGatewayTopology = saved
		resetTopologyCache()
	})
}

func TestTopologyCacheReturnsCachedResultWithinTtl(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls atomic.Int32
	swapTopologyCollector(t, fakeTopology(&calls, 0))
	first := collectProfileGatewayTopologyCached(context.Background())
	second := collectProfileGatewayTopologyCached(context.Background())
	if calls.Load() != 1 || first != second {
		t.Fatalf("calls %d same %v", calls.Load(), first == second)
	}
}

func TestTopologyCacheRescansAfterTtl(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls atomic.Int32
	swapTopologyCollector(t, fakeTopology(&calls, 0))
	collectProfileGatewayTopologyCached(context.Background())
	topologyCacheMu.Lock()
	topologyCache.ts = topologyCache.ts.Add(-(topologyCacheTTL + time.Second))
	topologyCacheMu.Unlock()
	collectProfileGatewayTopologyCached(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestTopologyCacheCollapsesConcurrentScans(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var calls atomic.Int32
	swapTopologyCollector(t, fakeTopology(&calls, 50*time.Millisecond))
	var wg sync.WaitGroup
	results := make([]*pyjson.Object, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = collectProfileGatewayTopologyCached(context.Background())
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	for _, r := range results {
		if !pyjson.Equal(r, results[0]) {
			t.Fatalf("results differ %v", results)
		}
	}
}

type topoHome struct{ name, home string }

// Patches the topology collaborators with fixed homes, live set and runtime records
func patchTopology(t *testing.T, homes []topoHome, running map[string]bool, runtimes map[string]*pyjson.Object) {
	t.Helper()
	savedServe, savedCheck, savedRead := topologyProfilesToServe, checkGatewayRunning, readRuntimeStatus
	t.Cleanup(func() {
		topologyProfilesToServe, checkGatewayRunning, readRuntimeStatus = savedServe, savedCheck, savedRead
	})
	topologyProfilesToServe = func(context.Context, bool, bool, bool) ([]profiles.ServedProfile, error) {
		out := []profiles.ServedProfile{}
		for _, h := range homes {
			out = append(out, profiles.ServedProfile{Name: h.name, Home: h.home})
		}
		return out, nil
	}
	checkGatewayRunning = func(home string) bool {
		for _, h := range homes {
			if h.home == home {
				return running[h.name]
			}
		}
		return false
	}
	byPath := map[string]*pyjson.Object{}
	for _, h := range homes {
		byPath[filepath.Join(h.home, "gateway_state.json")] = runtimes[h.name]
	}
	readRuntimeStatus = func(path string) *pyjson.Object { return byPath[path] }
}

// Swaps the writer identity lookup for the case
func patchWriterIdentity(t *testing.T, fn func(home string, rt *pyjson.Object) (int, any, bool)) {
	saved := gatewayWriterIdentityOfHome
	gatewayWriterIdentityOfHome = fn
	t.Cleanup(func() { gatewayWriterIdentityOfHome = saved })
}

func TestRunningStandaloneProfileIsInTopology(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	solo := filepath.Join(root, "profiles", "solo")
	if err := os.MkdirAll(solo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(solo, "config.yaml"), []byte("gateway:\n  standalone: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	saved := checkGatewayRunning
	t.Cleanup(func() { checkGatewayRunning = saved })
	checkGatewayRunning = func(home string) bool { return home == solo }
	topo := collectProfileGatewayTopologyNow(context.Background())
	names := topo.Vals["profiles"].([]any)
	if !slices.Contains(names, any("solo")) {
		t.Fatalf("profiles %v", names)
	}
	var gws []any
	for _, g := range topo.Vals["gateways"].([]any) {
		gws = append(gws, g.(*pyjson.Object).Vals["profile"])
	}
	if !slices.Equal(gws, []any{"solo"}) {
		t.Fatalf("gateways %v", gws)
	}
}

func TestNoGatewaysRunning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	patchTopology(t, []topoHome{{"default", filepath.Join(tmp, "d")}, {"coder", filepath.Join(tmp, "c")}}, map[string]bool{}, nil)
	topo := collectProfileGatewayTopologyNow(context.Background())
	if !pyjson.Equal(topo.Vals["profiles"], []any{"default", "coder"}) || topo.Vals["gateway_mode"] != "none" ||
		len(topo.Vals["gateways"].([]any)) != 0 || topo.Vals["profile_platforms"].(*pyjson.Object).Len() != 0 {
		t.Fatalf("topo %v", topo.Vals)
	}
}

func TestCollectsPerProfilePlatformMaps(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	d, c := filepath.Join(tmp, "d"), filepath.Join(tmp, "c")
	telegram := pyjson.ObjectOf("state", "connected", "writer_pid", 100, "writer_start_time", 111)
	discord := pyjson.ObjectOf("state", "fatal", "error_code", "duplicate_credential", "writer_pid", 200, "writer_start_time", 222)
	patchTopology(t, []topoHome{{"default", d}, {"coder", c}}, map[string]bool{"default": true, "coder": true},
		map[string]*pyjson.Object{
			"default": pyjson.ObjectOf("platforms", pyjson.ObjectOf("telegram", telegram)),
			"coder":   pyjson.ObjectOf("platforms", pyjson.ObjectOf("discord", discord)),
		})
	identities := map[string][2]int{d: {100, 111}, c: {200, 222}}
	patchWriterIdentity(t, func(home string, _ *pyjson.Object) (int, any, bool) {
		id, ok := identities[home]
		return id[0], id[1], ok
	})
	topo := collectProfileGatewayTopologyNow(context.Background())
	want := pyjson.ObjectOf("default", pyjson.ObjectOf("telegram", telegram), "coder", pyjson.ObjectOf("discord", discord))
	if topo.Vals["gateway_mode"] != "multiple" || !pyjson.Equal(topo.Vals["profile_platforms"], want) {
		t.Fatalf("topo %v", topo.Vals)
	}
}

func TestStalePlatformEntriesAreNotAggregated(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	patchTopology(t, []topoHome{{"default", filepath.Join(tmp, "d")}, {"coder", filepath.Join(tmp, "c")}}, map[string]bool{"coder": true},
		map[string]*pyjson.Object{"coder": pyjson.ObjectOf("platforms", pyjson.ObjectOf(
			"telegram", pyjson.ObjectOf("state", "fatal", "error_code", "duplicate_credential",
				"updated_at", "2026-08-05T10:00:00.900000+00:00", "writer_pid", 199, "writer_start_time", 110),
			"discord", pyjson.ObjectOf("state", "fatal"),
			"slack", pyjson.ObjectOf("state", "fatal", "writer_pid", 200, "writer_start_time", 110),
			"signal", pyjson.ObjectOf("state", "fatal", "error_code", "duplicate_credential", "writer_pid", 200, "writer_start_time", 222)))})
	patchWriterIdentity(t, func(string, *pyjson.Object) (int, any, bool) { return 200, 222, true })
	topo := collectProfileGatewayTopologyNow(context.Background())
	coder, _ := topo.Vals["profile_platforms"].(*pyjson.Object).GetOr("coder", pyjson.NewObject()).(*pyjson.Object)
	if !slices.Equal(coder.Keys, []string{"signal"}) {
		t.Fatalf("coder platforms %v", coder.Keys)
	}
}

func TestAStalePlatformEntryReportsNoPort(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	patchTopology(t, []topoHome{{"default", filepath.Join(tmp, "d")}}, map[string]bool{"default": true},
		map[string]*pyjson.Object{"default": pyjson.ObjectOf("platforms", pyjson.ObjectOf(
			"api_server", pyjson.ObjectOf("state", "connected", "writer_pid", 200, "writer_start_time", 222),
			"feishu", pyjson.ObjectOf("state", "connected", "writer_pid", 34105, "writer_start_time", int64(178945520374))))})
	patchWriterIdentity(t, func(string, *pyjson.Object) (int, any, bool) { return 200, 222, true })
	topo := collectProfileGatewayTopologyNow(context.Background())
	ports := topo.Vals["gateways"].([]any)[0].(*pyjson.Object).Vals["ports"].(*pyjson.Object)
	if !slices.Equal(ports.Keys, []string{"api_server"}) {
		t.Fatalf("ports %v", ports.Keys)
	}
}

func TestNoLiveProcessMeansNoAggregation(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	patchTopology(t, []topoHome{{"coder", filepath.Join(tmp, "c")}}, map[string]bool{"coder": true},
		map[string]*pyjson.Object{"coder": pyjson.ObjectOf("platforms", pyjson.ObjectOf(
			"telegram", pyjson.ObjectOf("state", "fatal", "writer_pid", 200, "writer_start_time", 222)))})
	patchWriterIdentity(t, func(string, *pyjson.Object) (int, any, bool) { return 0, nil, false })
	topo := collectProfileGatewayTopologyNow(context.Background())
	if topo.Vals["profile_platforms"].(*pyjson.Object).Len() != 0 {
		t.Fatalf("platforms %v", topo.Vals["profile_platforms"])
	}
}

func TestEnumerationFailureDegradesGracefully(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := topologyProfilesToServe
	t.Cleanup(func() { topologyProfilesToServe = saved })
	topologyProfilesToServe = func(context.Context, bool, bool, bool) ([]profiles.ServedProfile, error) {
		return nil, errors.New("no profiles root")
	}
	topo := collectProfileGatewayTopologyNow(context.Background())
	want := pyjson.ObjectOf("profiles", []any{}, "gateway_mode", "unknown", "gateways", []any{}, "profile_platforms", pyjson.NewObject())
	if !pyjson.Equal(topo, want) {
		t.Fatalf("topo %v", topo.Vals)
	}
}

func TestUnnamedLifecycleVerbsFromDefaultHomeAlwaysNameDefault(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "profiles", "coder", "config.yaml"), []byte("{}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "config.yaml"), []byte("model: {default: x}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "active_profile"), []byte("coder"), 0o644)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	for _, v := range []string{"ICEBATH_SUPERVISED_CHILD", "ICEBATH_S6_SUPERVISED_CHILD", "INVOCATION_ID",
		"ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "ICEBATH_UPDATE_POST_SWAP"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	savedSecrets := tools.ProfileSecrets
	tools.ProfileSecrets = provider.ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = savedSecrets })
	ctx := context.Background()
	for _, verb := range []string{"start", "stop", "restart"} {
		argv, err := gatewaySubcommand(ctx, "", verb)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(argv, []string{"-p", "default", "gateway", verb}) || namedProfileFromAction(argv) != "default" {
			t.Fatalf("%s: argv %v", verb, argv)
		}
	}
	argv, err := gatewaySubcommand(ctx, "", "restart")
	if err != nil {
		t.Fatal(err)
	}
	env, err := profileActionEnvironment(ctx, argv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["ICEBATH_HOME"] != root {
		t.Fatalf("ICEBATH_HOME %q", env["ICEBATH_HOME"])
	}
}
