package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Minimal llama-server imitation whose answers the test drives through its fields
type lr5Fake struct {
	mu                 sync.Mutex
	models             any
	chatAnswer         string
	reasoning          string
	requestsProcessing int
	slots              any
	slotsError         int
	metricsError       int
	unloaded           []string
	hits               int
	port               int
}

func lr5StartFake(t *testing.T) *lr5Fake {
	t.Helper()
	st := &lr5Fake{chatAnswer: "Paris", slots: []any{}}
	srv := httptest.NewServer(http.HandlerFunc(st.serve))
	t.Cleanup(srv.Close)
	st.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return st
}

func (st *lr5Fake) set(fn func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn()
}

func (st *lr5Fake) send(w http.ResponseWriter, code int, body any) {
	var raw []byte
	if s, ok := body.(string); ok {
		raw = []byte(s)
	} else {
		raw, _ = json.Marshal(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func (st *lr5Fake) serve(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hits++
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/health":
			st.send(w, 200, map[string]any{"status": "ok"})
		case "/models":
			if st.models == nil {
				st.send(w, 404, map[string]any{})
			} else {
				st.send(w, 200, st.models)
			}
		case "/metrics":
			if st.metricsError != 0 {
				st.send(w, st.metricsError, map[string]any{})
			} else {
				st.send(w, 200, "llamacpp:requests_processing "+strconv.Itoa(st.requestsProcessing)+"\n")
			}
		case "/slots":
			if st.slotsError != 0 {
				st.send(w, st.slotsError, map[string]any{})
			} else {
				st.send(w, 200, st.slots)
			}
		default:
			st.send(w, 404, map[string]any{})
		}
		return
	}
	switch r.URL.Path {
	case "/v1/chat/completions":
		msg := map[string]any{"role": "assistant", "content": st.chatAnswer}
		if st.reasoning != "" {
			msg["reasoning_content"] = st.reasoning
		}
		st.send(w, 200, map[string]any{"choices": []any{map[string]any{"message": msg}}})
	case "/models/load":
		st.send(w, 200, map[string]any{"success": true})
	case "/models/unload":
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		st.unloaded = append(st.unloaded, pyStr(body["model"]))
		st.send(w, 200, map[string]any{"success": true})
	default:
		st.send(w, 404, map[string]any{})
	}
}

func (st *lr5Fake) unloadedSorted() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := append([]string{}, st.unloaded...)
	sort.Strings(out)
	return out
}

// Fresh ICEBATH_HOME for the test, returning its path
func lr5Home(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	return h
}

// Supervisor pointed at a fake HTTP port, driving the HTTP logic without a spawn
func lr5Sup(t *testing.T, port int) *LlamaServerSupervisor {
	t.Helper()
	dir := t.TempDir()
	lr5Home(t)
	sup, err := NewLlamaServerSupervisor(filepath.Join(dir, "llama-server"), dir, 4, port, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return sup
}

func lr5Models(entries ...[2]string) map[string]any {
	var data []any
	for _, e := range entries {
		data = append(data, map[string]any{"id": e[0], "status": map[string]any{"value": e[1]}})
	}
	return map[string]any{"data": data}
}

// Restores the bootstrap singletons and seams a test replaces
func lr5ResetBoot(t *testing.T) {
	t.Helper()
	bootMu.Lock()
	prevSup, prevEngine, prevHooked := localSupervisor, localServingEngine, localExitHooked
	localSupervisor, localServingEngine = nil, nil
	bootMu.Unlock()
	prevInstalled, prevStart, prevState := bootInstalledEngine, bootStartSupervisor, bootStateEndpoint
	prevStale, prevMax, prevGen := bootPresetsStaleFn, bootAdmittedModelsMax, bootGeneratePresets
	prevNvidia, prevClass, prevSweepSleep := bootNvidiaQuery, bootGPUClass, bootSweeperSleep
	t.Cleanup(func() {
		bootMu.Lock()
		localSupervisor, localServingEngine, localExitHooked = prevSup, prevEngine, prevHooked
		bootMu.Unlock()
		bootInstalledEngine, bootStartSupervisor, bootStateEndpoint = prevInstalled, prevStart, prevState
		bootPresetsStaleFn, bootAdmittedModelsMax, bootGeneratePresets = prevStale, prevMax, prevGen
		bootNvidiaQuery, bootGPUClass, bootSweeperSleep = prevNvidia, prevClass, prevSweepSleep
	})
}

// Captures log records into a buffer for the rest of the test
func lr5CaptureLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lr5LockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lr5LockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lr5LockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func lr5Touch(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Publishes a modern state record for this process with its parent as the owner
func lr5WriteCurrentProcessState(t *testing.T, baseURL, apiKey string) {
	t.Helper()
	self, err := openPSProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := self.Parent()
	if err != nil || parent == nil {
		t.Fatalf("no parent: %v", err)
	}
	created, _ := self.CreateTime()
	exe, _ := self.Exe()
	parentCreated, _ := parent.CreateTime()
	raw, _ := json.Marshal(map[string]any{"base_url": baseURL, "api_key": apiKey, "pid": self.Pid(), "create_time": created,
		"executable": exe, "owner_pid": parent.Pid(), "owner_create_time": parentCreated})
	lr5Touch(t, localRuntimeStatePath(), string(raw))
}

func TestTouchGenerateIsTheReadinessProof(t *testing.T) {
	st := lr5StartFake(t)
	sup := lr5Sup(t, st.port)
	if !sup.TouchGenerate("m", 300*time.Second) {
		t.Fatal("Paris answer must prove readiness")
	}
	st.set(func() { st.chatAnswer = "I cannot answer that." })
	if sup.TouchGenerate("m", 300*time.Second) {
		t.Fatal("a wrong answer must not prove readiness")
	}
}

func TestTouchGenerateScansReasoningContent(t *testing.T) {
	st := lr5StartFake(t)
	sup := lr5Sup(t, st.port)
	st.set(func() { st.chatAnswer = "" })
	if sup.TouchGenerate("m", 300*time.Second) {
		t.Fatal("empty content with no reasoning field must fail")
	}
	st.set(func() { st.reasoning = "The capital of France is Paris." })
	if !sup.TouchGenerate("m", 300*time.Second) {
		t.Fatal("an answer inside reasoning_content must prove readiness")
	}
}

func TestEnsureModelReadyUnknownModelRaises(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() { st.models = lr5Models([2]string{"present", "unloaded"}) })
	sup := lr5Sup(t, st.port)
	_, err := sup.EnsureModelReady("absent", 600*time.Second)
	var missing *lrModelMissingError
	if !errors.As(err, &missing) || err.Error() != "'model absent not present in models dir'" {
		t.Fatalf("err = %v", err)
	}
}

func TestIsIdleRequiresNoBusySlotsAndZeroProcessing(t *testing.T) {
	st := lr5StartFake(t)
	sup := lr5Sup(t, st.port)
	st.set(func() {
		st.models = lr5Models([2]string{"m", "loaded"})
		st.slots = []any{map[string]any{"id": 0, "is_processing": false}}
	})
	if !sup.IsIdle("") {
		t.Fatal("idle slots and zero processing must be idle")
	}
	st.set(func() { st.slots = []any{map[string]any{"id": 0, "is_processing": true}} })
	if sup.IsIdle("") {
		t.Fatal("a busy slot is not idle")
	}
	st.set(func() {
		st.slots = []any{map[string]any{"id": 0, "is_processing": false}}
		st.requestsProcessing = 2
	})
	if sup.IsIdle("") {
		t.Fatal("processing requests are not idle")
	}
}

func TestBaseURLDialsLoopbackIPNeverLocalhost(t *testing.T) {
	sup := lr5Sup(t, 9999)
	if !strings.Contains(sup.BaseURL(), "127.0.0.1") || strings.Contains(sup.BaseURL(), "localhost") {
		t.Fatal(sup.BaseURL())
	}
}

func lr5Now(v float64) *float64 { return &v }

func TestIdleSweepUnloadsIdleModels(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() { st.models = lr5Models([2]string{"model-a", "loaded"}, [2]string{"model-b", "loaded"}) })
	sup := lr5Sup(t, st.port)
	t0 := 1000.0
	if got := sup.SweepIdle(lr5Now(t0)); len(got) != 0 {
		t.Fatal(got)
	}
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS - 1)); len(got) != 0 {
		t.Fatal(got)
	}
	got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 1))
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"model-a", "model-b"}) || !reflect.DeepEqual(st.unloadedSorted(), got) {
		t.Fatal(got, st.unloadedSorted())
	}
}

func TestIdleSweepBusyModelResetsClock(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() { st.models = lr5Models([2]string{"side-m", "loaded"}) })
	sup := lr5Sup(t, st.port)
	t0 := 1000.0
	sup.SweepIdle(lr5Now(t0))
	st.set(func() { st.slots = []any{map[string]any{"is_processing": true}} })
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS)); len(got) != 0 {
		t.Fatal(got)
	}
	st.set(func() { st.slots = []any{} })
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 10)); len(got) != 0 || len(st.unloadedSorted()) != 0 {
		t.Fatal(got)
	}
}

func TestIdleSweepProbeFailureKeepsClock(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() { st.models = lr5Models([2]string{"stuck-m", "loaded"}) })
	sup := lr5Sup(t, st.port)
	t0 := 1000.0
	sup.SweepIdle(lr5Now(t0))
	st.set(func() { st.metricsError = 500 })
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 1)); len(got) != 0 || len(st.unloadedSorted()) != 0 {
		t.Fatal(got)
	}
	st.set(func() { st.metricsError = 0 })
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 2)); !reflect.DeepEqual(got, []string{"stuck-m"}) || !reflect.DeepEqual(st.unloadedSorted(), got) {
		t.Fatal(got)
	}
}

func TestIdleSweepBusyAfterProbeFailureStillResetsClock(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() { st.models = lr5Models([2]string{"m", "loaded"}) })
	sup := lr5Sup(t, st.port)
	t0 := 1000.0
	sup.SweepIdle(lr5Now(t0))
	st.set(func() { st.slotsError = 500 })
	sup.SweepIdle(lr5Now(t0 + 100))
	st.set(func() { st.slotsError = 0; st.slots = []any{map[string]any{"is_processing": true}} })
	sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 5))
	st.set(func() { st.slots = []any{} })
	if got := sup.SweepIdle(lr5Now(t0 + LlamaIdleUnloadS + 6)); len(got) != 0 || len(st.unloadedSorted()) != 0 {
		t.Fatal(got)
	}
}

func TestStagedModelsRequiresEverySplitPart(t *testing.T) {
	lr5Home(t)
	mdir := localModelsDir()
	for _, name := range []string{"Single-Q4_K_M.gguf", "Whole-Q4-00001-of-00002.gguf", "Whole-Q4-00002-of-00002.gguf", "Partial-Q4-00001-of-00003.gguf"} {
		lr5Touch(t, filepath.Join(mdir, name), "")
	}
	if got := stagedModelIDs(); !reflect.DeepEqual(got, []string{"Single-Q4_K_M", "Whole-Q4"}) {
		t.Fatal(got)
	}
}

func TestBootstrapSkipsBootWithNoStagedModels(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	called := false
	bootInstalledEngine = func(context.Context, string) (*Engine, error) {
		called = true
		return nil, errors.New("must not reach install/spawn")
	}
	sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false)
	if sup != nil || err != nil || called {
		t.Fatal(sup, err, called)
	}
}

func TestEndpointIdentityStableAcrossSupervisorInstances(t *testing.T) {
	h := lr5Home(t)
	port, err := lrFreePort()
	if err != nil {
		t.Fatal(err)
	}
	prev := lrDefaultPort
	lrDefaultPort = port
	t.Cleanup(func() { lrDefaultPort = prev })
	dir := t.TempDir()
	first, err1 := NewLlamaServerSupervisor(filepath.Join(dir, "install"), filepath.Join(dir, "models"), 4, 0, nil, "", "")
	second, err2 := NewLlamaServerSupervisor(filepath.Join(dir, "install"), filepath.Join(dir, "models"), 4, 0, nil, "", "")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if first.APIKey != second.APIKey || len(first.APIKey) < 16 || first.Port != port || second.Port != port {
		t.Fatal(first.APIKey, second.APIKey, first.Port, second.Port)
	}
	raw, err := os.ReadFile(filepath.Join(h, "runtimes", "llamacpp", ".api_key"))
	if err != nil || strings.TrimSpace(string(raw)) != first.APIKey {
		t.Fatal(string(raw), err)
	}
}

func TestBootstrapDisabledIsNoop(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	for _, cfg := range []map[string]any{{"local_runtime": map[string]any{"enabled": false}}, {}, nil} {
		if sup, err := EnsureLocalRuntime(context.Background(), cfg, false); sup != nil || err != nil {
			t.Fatal(cfg, sup, err)
		}
	}
}

func TestBootstrapReusesRunningServer(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	st := lr5StartFake(t)
	raw, _ := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:" + strconv.Itoa(st.port) + "/v1", "api_key": "k", "pid": os.Getpid()})
	lr5Touch(t, localRuntimeStatePath(), string(raw))
	called := false
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { called = true; return nil, nil }
	if sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); sup != nil || err != nil || called {
		t.Fatal(sup, err, called)
	}
}

func TestBootstrapFailureNeverRaises(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	bootNvidiaQuery = func() (*NvidiaGPUQuery, error) { return nil, nil }
	bootGPUClass = func() string { return "none" }
	lr5Touch(t, filepath.Join(localModelsDir(), "test.gguf"), "")
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, errors.New("no network") }
	if sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); sup != nil || err != nil {
		t.Fatal(sup, err)
	}
}

func TestEnsureLocalRuntimeSerializesRacingCallers(t *testing.T) {
	dir := t.TempDir()
	lr5Home(t)
	lr5ResetBoot(t)
	lr5Touch(t, filepath.Join(localModelsDir(), "tiny-Q4_K_M.gguf"), "")
	bootGeneratePresets = func(string, string, int, string) string { return "" }
	bootPresetsStaleFn = func() bool { return false }
	bootAdmittedModelsMax = func(_ string, configured int, _ string) int { return configured }
	bootInstalledEngine = func(context.Context, string) (*Engine, error) {
		return &Engine{Backend: "cpu", Tag: "b1", Binary: filepath.Join(dir, "llama-server")}, nil
	}
	var mu sync.Mutex
	spawns := 0
	bootStartSupervisor = func(s *LlamaServerSupervisor) error {
		mu.Lock()
		spawns++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		lr5WriteCurrentProcessState(t, s.BaseURL(), s.APIKey)
		return nil
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false)
		}()
	}
	close(start)
	wg.Wait()
	if spawns != 1 {
		t.Fatalf("both racing callers spawned a router instead of the second adopting the first's published state: %d", spawns)
	}
}

func TestEnsureLocalRuntimeProceedsWhenBootLockIsUnwritable(t *testing.T) {
	h := lr5Home(t)
	lr5ResetBoot(t)
	lr5Touch(t, filepath.Join(localModelsDir(), "tiny-Q4_K_M.gguf"), "")
	lr5Touch(t, filepath.Join(h, "runtimes"), "")
	bootStateEndpoint = func(context.Context) (bool, error) { return false, nil }
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	logs := lr5CaptureLogs(t)
	if sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); sup != nil || err != nil {
		t.Fatal(sup, err)
	}
	if !strings.Contains(logs(), "boot lock unavailable") {
		t.Fatal(logs())
	}
}

func TestBootPricesTheEngineItServes(t *testing.T) {
	dir := t.TempDir()
	lr5Home(t)
	lr5ResetBoot(t)
	lr5Touch(t, filepath.Join(localModelsDir(), "test.gguf"), "")
	vulkan := &Engine{Backend: "vulkan", Tag: "b1", Binary: filepath.Join(dir, "llama-server")}
	bootInstalledEngine = func(_ context.Context, backend string) (*Engine, error) {
		if backend == "vulkan" {
			return vulkan, nil
		}
		return nil, nil
	}
	type priced struct {
		engine *Engine
		tag    string
	}
	var pricedList []priced
	bootGeneratePresets = func(_ string, path string, _ int, engineTag string) string {
		engine, err := configuredEngine()
		if err != nil {
			t.Fatal(err)
		}
		pricedList = append(pricedList, priced{engine, engineTag})
		return path
	}
	var capped []string
	bootAdmittedModelsMax = func(_ string, configured int, engineTag string) int {
		capped = append(capped, engineTag)
		return configured
	}
	bootStartSupervisor = func(*LlamaServerSupervisor) error { return errors.New("health timeout") }
	sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true, "backend": "vulkan"}}, false)
	if sup != nil || err != nil {
		t.Fatal(sup, err)
	}
	if len(pricedList) != 1 || pricedList[0].engine != vulkan || pricedList[0].tag != "b1" || !reflect.DeepEqual(capped, []string{"b1"}) {
		t.Fatal(pricedList, capped)
	}
	if servingEngine() != nil {
		t.Fatal("a failed boot serves nothing")
	}
}

func TestAutoVendorDetectionUsesHostGPUClass(t *testing.T) {
	lr5ResetBoot(t)
	bootNvidiaQuery = func() (*NvidiaGPUQuery, error) { return nil, nil }
	for _, tc := range []struct{ class, want string }{{"intel", "intel"}, {"amd", "amd"}, {"none", ""}, {"unknown", ""}} {
		bootGPUClass = func() string { return tc.class }
		if got, err := detectGPUVendor(); got != tc.want || err != nil {
			t.Fatal(tc.class, got, err)
		}
	}
	bootNvidiaQuery = func() (*NvidiaGPUQuery, error) { return &NvidiaGPUQuery{GPUName: "RTX 4090"}, nil }
	if got, _ := detectGPUVendor(); got != "nvidia RTX 4090" {
		t.Fatal(got)
	}
}

func TestLocalRuntimeStartupReusesWithoutActivityProbe(t *testing.T) {
	for _, failure := range []string{"arrival", "truncated"} {
		t.Run(failure, func(t *testing.T) {
			lr5Home(t)
			lr5ResetBoot(t)
			st := lr5StartFake(t)
			lr5Touch(t, filepath.Join(localModelsDir(), "model.gguf"), "")
			bootPresetsStaleFn = func() bool { return false }
			bootStateEndpoint = func(context.Context) (bool, error) { return true, nil }
			raw, _ := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:" + strconv.Itoa(st.port) + "/v1", "api_key": "test-only"})
			lr5Touch(t, localRuntimeStatePath(), string(raw))
			for i := 0; i < 2; i++ {
				if sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); sup != nil || err != nil {
					t.Fatal(sup, err)
				}
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if st.hits != 0 {
				t.Fatalf("startup probed the running server %d times", st.hits)
			}
		})
	}
}

// Process handle double recording every signal and wait
type lr5FakeProc struct {
	pid                    int
	running                bool
	waitErr                error
	terminated, killed     int
	children               []localRuntimeProc
	childrenErr            error
	stopRunningAfterChildr bool
}

func (f *lr5FakeProc) Pid() int                          { return f.pid }
func (f *lr5FakeProc) CreateTime() (float64, error)      { return 1, nil }
func (f *lr5FakeProc) IsRunning() bool                   { return f.running }
func (f *lr5FakeProc) Exe() (string, error)              { return "", nil }
func (f *lr5FakeProc) PPID() (int, error)                { return 1, nil }
func (f *lr5FakeProc) Parent() (localRuntimeProc, error) { return nil, nil }
func (f *lr5FakeProc) Cmdline() ([]string, error)        { return nil, nil }
func (f *lr5FakeProc) Children(bool) ([]localRuntimeProc, error) {
	if f.stopRunningAfterChildr {
		f.running = false
	}
	return f.children, f.childrenErr
}
func (f *lr5FakeProc) Terminate() error         { f.terminated++; return nil }
func (f *lr5FakeProc) Kill() error              { f.killed++; return nil }
func (f *lr5FakeProc) Wait(time.Duration) error { return f.waitErr }

func TestLocalRuntimeTerminateTreeEscalatesAndAlwaysCleansChildren(t *testing.T) {
	prev := supOpenProcess
	t.Cleanup(func() { supOpenProcess = prev })
	for _, kind := range []string{"psutil", "subprocess", "missing-psutil", "wait-error"} {
		child := &lr5FakeProc{pid: 456, running: true}
		proc := &lr5FakeProc{pid: 123, running: true}
		switch kind {
		case "wait-error":
			proc.waitErr = errors.New("wait failed")
		case "subprocess":
			proc.waitErr = &psError{Class: "TimeoutExpired", Pid: 123, Msg: "Command 'router' timed out after 15 seconds"}
		default:
			proc.waitErr = psTimeout(123, 15*time.Second)
		}
		if kind == "missing-psutil" {
			supOpenProcess = func(int) (localRuntimeProc, error) { return nil, errors.New("No module named 'psutil'") }
		} else {
			supOpenProcess = func(int) (localRuntimeProc, error) { return &lr5FakeProc{children: []localRuntimeProc{child}}, nil }
		}
		err := terminateLlamaServerTree(proc, false)
		if kind == "wait-error" {
			if err == nil || !strings.Contains(err.Error(), "wait failed") {
				t.Fatal(kind, err)
			}
		} else if err != nil || proc.killed != 1 {
			t.Fatal(kind, err, proc.killed)
		}
		if kind != "missing-psutil" && (child.terminated != 1 || child.killed != 1) {
			t.Fatal(kind, child.terminated, child.killed)
		}
	}
}

func TestLocalRuntimeTerminateTreeVerifiedRootRefusesAReusedRoot(t *testing.T) {
	child := &lr5FakeProc{pid: 456, running: true}
	proc := &lr5FakeProc{pid: 123, running: true, children: []localRuntimeProc{child}, stopRunningAfterChildr: true}
	err := terminateLlamaServerTree(proc, true)
	if !psIsNoSuchProcess(err) || proc.terminated != 0 || child.terminated != 0 {
		t.Fatal(err, proc.terminated, child.terminated)
	}
}

func TestLocalRuntimeStoppedStateIsRetainedWithoutUnlinkRace(t *testing.T) {
	sup := lr5Sup(t, 59998)
	path := localRuntimeStatePath()
	lr5Touch(t, path, `{"pid": 101}`)
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"pid": 101}` {
		t.Fatal(string(raw))
	}
	replacement := `{"pid": 1, "base_url": "http://127.0.0.1:59997/v1", "api_key": "test-only"}`
	lr5Touch(t, path, replacement)
	if err := sup.Stop(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != replacement {
		t.Fatal(string(raw))
	}
}

func TestLocalRuntimeSupervisorModelsKeepsListingOrderAndDefaults(t *testing.T) {
	st := lr5StartFake(t)
	st.set(func() {
		st.models = map[string]any{"data": []any{map[string]any{"id": "b"}, map[string]any{"id": "a", "status": map[string]any{"value": "loaded"}}}}
	})
	sup := lr5Sup(t, st.port)
	got, err := sup.Models(30 * time.Second)
	if err != nil || !reflect.DeepEqual(got.IDs, []string{"b", "a"}) || got.Status["b"] != "unknown" || got.Status["a"] != "loaded" {
		t.Fatal(got, err)
	}
	if lrRequestBody("model", "m") == nil || string(lrRequestBody("model", "m", "n", 0)) != `{"model": "m", "n": 0}` {
		t.Fatal(string(lrRequestBody("model", "m", "n", 0)))
	}
}

func TestLocalRuntimePythonFloatOfMetricValues(t *testing.T) {
	for text, want := range map[string]float64{"0": 0, "2": 2, "1_0.5": 10.5, "1e3": 1000, "٢": 2, ".5": 0.5} {
		if got, ok := lrPyFloat(text); !ok || got != want {
			t.Fatal(text, got, ok)
		}
	}
	for _, bad := range []string{"0x10", "1__0", "_1", "", "llamacpp:requests_processing", "²"} {
		if _, ok := lrPyFloat(bad); ok {
			t.Fatal(bad)
		}
	}
}

// Profile home under root with an identity marker unless identity is false
func lr5Profile(t *testing.T, root, name string, identity bool) string {
	t.Helper()
	p := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(filepath.Join(p, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if identity {
		lr5Touch(t, filepath.Join(p, "config.yaml"), "")
	}
	return p
}

func TestLocalRuntimeLegacyModelsMoveIntoTheMachineDirWithoutClobbering(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	newDir := localModelsDir()
	lr5Touch(t, filepath.Join(newDir, "shared.gguf"), "current")
	work := lr5Profile(t, root, "work", true)
	split := []string{"big-00001-of-00002.gguf", "big-00002-of-00002.gguf"}
	for _, name := range append(append([]string{}, split...), "shared.gguf") {
		lr5Touch(t, filepath.Join(work, "models", name), "legacy "+name)
	}
	lr5Touch(t, filepath.Join(work, "models", "assets", "mmproj.gguf"), "proj")
	play := lr5Profile(t, root, "play", true)
	lr5Touch(t, filepath.Join(play, "models", "small.gguf"), "small")
	ghost := lr5Profile(t, root, "ghost", false)
	lr5Touch(t, filepath.Join(ghost, "models", "ghost.gguf"), "")
	if _, err := adoptLegacyModels(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range stagedModels() {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"big-00001-of-00002.gguf", "shared.gguf", "small.gguf"}) {
		t.Fatal(names)
	}
	read := func(p string) string { raw, _ := os.ReadFile(p); return string(raw) }
	for _, name := range split {
		if read(filepath.Join(newDir, name)) != "legacy "+name {
			t.Fatal(name)
		}
	}
	if read(filepath.Join(localAssetsDir(), "mmproj.gguf")) != "proj" || read(filepath.Join(newDir, "shared.gguf")) != "current" ||
		read(filepath.Join(work, "models", "shared.gguf")) != "legacy shared.gguf" {
		t.Fatal("legacy adoption clobbered or lost a file")
	}
	if lrPathExists(filepath.Join(play, "models")) || !lrPathExists(filepath.Join(ghost, "models", "ghost.gguf")) {
		t.Fatal("emptied legacy dir kept or a marker-less shell adopted")
	}
	if moved, err := adoptLegacyModels(); len(moved) != 0 || err != nil {
		t.Fatal(moved, err)
	}
}

func TestLocalRuntimeBootAdoptsLegacyModels(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	lr5ResetBoot(t)
	work := lr5Profile(t, root, "work", true)
	lr5Touch(t, filepath.Join(work, "models", "m.gguf"), "x")
	bootStateEndpoint = func(context.Context) (bool, error) { return false, nil }
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	if sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false); sup != nil || err != nil {
		t.Fatal(sup, err)
	}
	if !lrPathExists(filepath.Join(localModelsDir(), "m.gguf")) {
		t.Fatal("boot did not adopt the legacy model")
	}
}

func TestLocalRuntimeModelsAndRuntimesResolveToTheSharedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	if localModelsDir() != filepath.Join(root, "models") || RuntimesRoot() != filepath.Join(root, "runtimes", "llamacpp") {
		t.Fatal(localModelsDir(), RuntimesRoot())
	}
	if localRuntimeStatePath() != filepath.Join(root, "runtimes", "llamacpp", "server.json") || localAssetsDir() != filepath.Join(root, "models", "assets") {
		t.Fatal(localRuntimeStatePath(), localAssetsDir())
	}
}

func TestLocalRuntimeStatePathIsServerJSONUnderRuntimesRoot(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got, want := LocalRuntimeStatePath(), filepath.Join(RuntimesRoot(), "server.json"); got != want {
		t.Fatalf("state path %q, want %q", got, want)
	}
}
