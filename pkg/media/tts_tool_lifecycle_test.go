package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the leases and every local model cache around a case
func cleanLifecycle(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	reset := func() {
		ResetTTSLeasesForTests()
		for _, cache := range localTTSModelCaches {
			cache.Clear()
		}
	}
	reset()
	t.Cleanup(reset)
}

// Piper engine double counting voice loads and recording synthesized text
type fakePiperEngine struct {
	mu          sync.Mutex
	loads       int
	synthesized []string
	onLoad      func()
}

func (f *fakePiperEngine) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// Mono 16 bit 16kHz WAV of 160 silent frames
func silentWAV() []byte {
	data := make([]byte, 320)
	header := make([]byte, 44)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(data)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], 16000)
	binary.LittleEndian.PutUint32(header[28:], 32000)
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(data)))
	return append(header, data...)
}

// Worker answering synthesis requests in process, bound to an exited helper so Close waits cleanly
func (f *fakePiperEngine) worker(t *testing.T) (*localTTSWorker, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	reqR, reqW := io.Pipe()
	repR, repW := io.Pipe()
	go func() {
		defer repW.Close()
		lines := bufio.NewScanner(reqR)
		for lines.Scan() {
			var req map[string]any
			if err := json.Unmarshal(lines.Bytes(), &req); err != nil {
				t.Error(err)
				return
			}
			f.mu.Lock()
			f.synthesized = append(f.synthesized, req["text"].(string))
			f.mu.Unlock()
			if err := os.WriteFile(req["wav_path"].(string), silentWAV(), 0o600); err != nil {
				t.Error(err)
			}
			if _, err := repW.Write([]byte("{}\n")); err != nil {
				return
			}
		}
	}()
	return &localTTSWorker{cmd: cmd, stdin: reqW, out: bufio.NewReader(repR)}, nil
}

// Installs the Piper double with an on disk voice and returns the engine and the TTS config
func fakePiper(t *testing.T) (*fakePiperEngine, map[string]any) {
	t.Helper()
	cleanLifecycle(t)
	engine := &fakePiperEngine{}
	swap(t, &ttsImportPiper, func(context.Context) error { return nil })
	swap(t, &startLocalTTSWorker, func(_ context.Context, spec map[string]any) (*localTTSWorker, error) {
		engine.mu.Lock()
		engine.loads++
		onLoad := engine.onLoad
		engine.mu.Unlock()
		if onLoad != nil {
			onLoad()
		}
		return engine.worker(t)
	})
	voices := filepath.Join(t.TempDir(), "voices")
	if err := os.MkdirAll(voices, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(voices, "en_US-test-medium.onnx"), []byte("onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(voices, "en_US-test-medium.onnx.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"provider": "piper", "piper": map[string]any{"voice": "en_US-test-medium", "voices_dir": voices}}
	swap(t, &loadTTSConfig, func(context.Context, *tools.Env) map[string]any { return cfg })
	return engine, cfg
}

// Keep warm timer double that records its interval and fires only on demand
type fakeKeepWarmTimer struct {
	interval  time.Duration
	fn        func()
	cancelled bool
}

func (f *fakeKeepWarmTimer) Stop() bool {
	f.cancelled = true
	return true
}

func (f *fakeKeepWarmTimer) fire() { f.fn() }

// Replaces the keep warm timer factory and returns every timer it created
func fakeTimers(t *testing.T) *[]*fakeKeepWarmTimer {
	t.Helper()
	created := &[]*fakeKeepWarmTimer{}
	swap(t, &makeKeepWarmTimer, func(delay time.Duration, fn func()) keepWarmTimerHandle {
		timer := &fakeKeepWarmTimer{interval: delay, fn: fn}
		*created = append(*created, timer)
		return timer
	})
	return created
}

// Timers started and not cancelled
func pendingTimers(timers *[]*fakeKeepWarmTimer) []*fakeKeepWarmTimer {
	var out []*fakeKeepWarmTimer
	for _, timer := range *timers {
		if !timer.cancelled {
			out = append(out, timer)
		}
	}
	return out
}

// Kept KittenTTS model entry under the key
func putKittenModel(t *testing.T, key string) {
	t.Helper()
	if _, err := ttsCacheGetOrLoad(kittenTTSModelCache, key, func() (any, error) { return struct{}{}, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestWarmLoadsPiperIntoSynthesisCache(t *testing.T) {
	engine, cfg := fakePiper(t)
	ctx := context.Background()
	result := WarmTTSProvider(ctx, nil, cfg, "")
	if result["warmed"] != true || result["action"] != "loaded" || result["provider"] != "piper" {
		t.Fatalf("result %v", result)
	}
	if engine.loadCount() != 1 || len(piperVoiceCache.Keys()) != 1 {
		t.Fatalf("loads %d cache %d", engine.loadCount(), len(piperVoiceCache.Keys()))
	}
	out, err := generatePiperTTS(ctx, nil, "hello", filepath.Join(t.TempDir(), "out.wav"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, ".wav") || engine.loadCount() != 1 || !reflect.DeepEqual(engine.synthesized, []string{"hello"}) {
		t.Fatalf("out %q loads %d synthesized %q", out, engine.loadCount(), engine.synthesized)
	}
}

func TestWarmTwiceIsACacheHit(t *testing.T) {
	engine, cfg := fakePiper(t)
	WarmTTSProvider(context.Background(), nil, cfg, "")
	second := WarmTTSProvider(context.Background(), nil, cfg, "")
	if second["action"] != "cached" || engine.loadCount() != 1 {
		t.Fatalf("second %v loads %d", second, engine.loadCount())
	}
}

func TestWarmReadsConfiguredProviderWhenNoneGiven(t *testing.T) {
	fakePiper(t)
	result := WarmTTSProvider(context.Background(), nil, nil, "")
	if result["provider"] != "piper" || result["action"] != "loaded" {
		t.Fatalf("result %v", result)
	}
}

func TestWarmNeverRaisesOnEngineFailure(t *testing.T) {
	cleanLifecycle(t)
	swap(t, &ttsImportPiper, func(context.Context) error {
		return &localEngineError{Class: "ImportError", Msg: "No module named 'piper'"}
	})
	result := WarmTTSProvider(context.Background(), nil, map[string]any{"provider": "piper"}, "")
	if result["warmed"] != false || result["action"] != "error" || !strings.Contains(result["error"].(string), "piper") {
		t.Fatalf("result %v", result)
	}
	if len(piperVoiceCache.Keys()) != 0 {
		t.Fatal("cache filled after a failed warm")
	}
}

func TestWarmIsNoopForCloudProviderWithoutLazySdk(t *testing.T) {
	cleanLifecycle(t)
	result := WarmTTSProvider(context.Background(), nil, map[string]any{"provider": "openai"}, "")
	if !reflect.DeepEqual(result, map[string]any{"provider": "openai", "warmed": false, "action": "noop"}) {
		t.Fatalf("result %v", result)
	}
}

func TestReleaseDropsEveryLocalCache(t *testing.T) {
	_, cfg := fakePiper(t)
	WarmTTSProvider(context.Background(), nil, cfg, "")
	putKittenModel(t, "m")
	if got := ReleaseTTSProvider(context.Background(), nil, ""); !reflect.DeepEqual(got, map[string]any{"released": 2}) {
		t.Fatalf("released %v", got)
	}
	if len(piperVoiceCache.Keys()) != 0 || len(kittenTTSModelCache.Keys()) != 0 {
		t.Fatal("caches kept")
	}
}

func TestReleaseScopedToOneProvider(t *testing.T) {
	_, cfg := fakePiper(t)
	WarmTTSProvider(context.Background(), nil, cfg, "")
	putKittenModel(t, "m")
	if got := ReleaseTTSProvider(context.Background(), nil, "kittentts"); !reflect.DeepEqual(got, map[string]any{"released": 1}) {
		t.Fatalf("released %v", got)
	}
	if len(piperVoiceCache.Keys()) != 1 {
		t.Fatal("piper dropped by a kittentts release")
	}
}

func TestReleaseWithNothingResidentIsZero(t *testing.T) {
	cleanLifecycle(t)
	if got := ReleaseTTSProvider(context.Background(), nil, ""); !reflect.DeepEqual(got, map[string]any{"released": 0}) {
		t.Fatalf("released %v", got)
	}
}

func TestAcquireWarmsAndCounts(t *testing.T) {
	fakePiper(t)
	result := AcquireTTSLease(context.Background(), nil, "desktop:read-aloud", nil)
	if result["leases"] != 1 || result["action"] != "loaded" {
		t.Fatalf("result %v", result)
	}
	if got := TTSLeaseHolders(); !reflect.DeepEqual(got, []string{"desktop:read-aloud"}) {
		t.Fatalf("holders %q", got)
	}
}

func TestLastReleaseUnloadsButEarlierReleaseDoesNot(t *testing.T) {
	fakePiper(t)
	timers := fakeTimers(t)
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "desktop:read-aloud", nil)
	AcquireTTSLease(ctx, nil, "tui:voice-tts", nil)
	if len(piperVoiceCache.Keys()) != 1 {
		t.Fatal("voice not resident")
	}
	if first := ReleaseTTSLease(ctx, nil, "desktop:read-aloud"); !reflect.DeepEqual(first, map[string]any{"leases": 1, "released": 0}) {
		t.Fatalf("first %v", first)
	}
	if len(piperVoiceCache.Keys()) != 1 || len(pendingTimers(timers)) != 0 {
		t.Fatal("an earlier release scheduled an unload")
	}
	if last := ReleaseTTSLease(ctx, nil, "tui:voice-tts"); last["leases"] != 0 {
		t.Fatalf("last %v", last)
	}
	pending := pendingTimers(timers)
	if len(pending) != 1 {
		t.Fatalf("pending timers %d", len(pending))
	}
	pending[0].fire()
	if len(piperVoiceCache.Keys()) != 0 {
		t.Fatal("voice resident after the window")
	}
}

func TestReacquireIsIdempotentAndRehealsCache(t *testing.T) {
	engine, _ := fakePiper(t)
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "cli:voice-tts", nil)
	ReleaseTTSProvider(ctx, nil, "")
	result := AcquireTTSLease(ctx, nil, "cli:voice-tts", nil)
	if result["leases"] != 1 || result["action"] != "loaded" || engine.loadCount() != 2 {
		t.Fatalf("result %v loads %d", result, engine.loadCount())
	}
}

func TestReleaseUnknownLeaseIsNoop(t *testing.T) {
	fakePiper(t)
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "a", nil)
	if got := ReleaseTTSLease(ctx, nil, "never-acquired"); !reflect.DeepEqual(got, map[string]any{"leases": 1, "released": 0}) {
		t.Fatalf("release %v", got)
	}
	if len(piperVoiceCache.Keys()) != 1 {
		t.Fatal("voice dropped")
	}
}

func TestAcquireFailureStillRegistersLease(t *testing.T) {
	cleanLifecycle(t)
	swap(t, &ttsImportPiper, func(context.Context) error { return &localEngineError{Class: "RuntimeError", Msg: "engine missing"} })
	result := AcquireTTSLease(context.Background(), nil, "desktop:conversation", map[string]any{"provider": "piper"})
	if result["action"] != "error" || result["leases"] != 1 {
		t.Fatalf("result %v", result)
	}
	if got := TTSLeaseHolders(); !reflect.DeepEqual(got, []string{"desktop:conversation"}) {
		t.Fatalf("holders %q", got)
	}
}

func TestEveryLocalWarmerHasARegisteredCache(t *testing.T) {
	var warmers, caches []string
	for name := range localTTSWarmers() {
		warmers = append(warmers, name)
	}
	for name := range localTTSModelCaches {
		caches = append(caches, name)
	}
	sort.Strings(warmers)
	sort.Strings(caches)
	if !reflect.DeepEqual(warmers, caches) {
		t.Fatalf("warmers %q caches %q", warmers, caches)
	}
	if localTTSModelCaches["piper"] != piperVoiceCache || localTTSModelCaches["kittentts"] != kittenTTSModelCache {
		t.Fatal("registry does not hold the synthesis caches")
	}
}

// Plugin provider backed by a local server that records its warm and release hooks
type serverBackedTTS struct {
	TTSProviderDefaults
	mu    sync.Mutex
	calls []string
}

func (p *serverBackedTTS) Name() string { return "my-server" }
func (p *serverBackedTTS) Synthesize(_ context.Context, _, outputPath string, _ TTSOptions) (string, error) {
	return outputPath, nil
}
func (p *serverBackedTTS) Warm(context.Context) error    { p.record("warm"); return nil }
func (p *serverBackedTTS) Release(context.Context) error { p.record("release"); return nil }
func (p *serverBackedTTS) record(hook string) {
	p.mu.Lock()
	p.calls = append(p.calls, hook)
	p.mu.Unlock()
}
func (p *serverBackedTTS) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func TestPluginProviderWarmAndReleaseFollowTheLease(t *testing.T) {
	cleanLifecycle(t)
	timers := fakeTimers(t)
	resetTTSRegistry(t)
	p := &serverBackedTTS{}
	p.TTSProviderDefaults = NewTTSProviderDefaults(p)
	if err := RegisterTTSProvider(p, ""); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"provider": "my-server"}
	swap(t, &loadTTSConfig, func(context.Context, *tools.Env) map[string]any { return cfg })
	swap(t, &ttsEnsurePluginsDiscovered, func(context.Context, bool) {})
	ctx := context.Background()
	if got := AcquireTTSLease(ctx, nil, "desktop:read-aloud", cfg)["action"]; got != "warmed" {
		t.Fatalf("action %v", got)
	}
	AcquireTTSLease(ctx, nil, "tui:voice-tts", cfg)
	ReleaseTTSLease(ctx, nil, "desktop:read-aloud")
	if got := p.snapshot(); !reflect.DeepEqual(got, []string{"warm", "warm"}) {
		t.Fatalf("calls %q", got)
	}
	ReleaseTTSLease(ctx, nil, "tui:voice-tts")
	if got := p.snapshot(); !reflect.DeepEqual(got, []string{"warm", "warm"}) {
		t.Fatalf("calls %q", got)
	}
	pendingTimers(timers)[0].fire()
	if got := p.snapshot(); !reflect.DeepEqual(got, []string{"warm", "warm", "release"}) {
		t.Fatalf("calls %q", got)
	}
}

// Command runner double recording each hook command and signalling once per run
func fakeHookRunner(t *testing.T, each func(env *tools.Env, envPassthrough []string)) (*[]string, chan struct{}) {
	t.Helper()
	ran := &[]string{}
	done := make(chan struct{}, 4)
	var mu sync.Mutex
	swap(t, &lifecycleRunCommandProvider, func(_ context.Context, env *tools.Env, command string, _ float64, envPassthrough []string) (*commandCompleted, error) {
		mu.Lock()
		*ran = append(*ran, command)
		mu.Unlock()
		if each != nil {
			each(env, envPassthrough)
		}
		done <- struct{}{}
		return &commandCompleted{}, nil
	})
	return ran, done
}

// Waits up to five seconds for one hook run
func waitHook(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hook command never ran")
	}
}

func TestCommandProviderRunsWarmAndReleaseCommands(t *testing.T) {
	cleanLifecycle(t)
	timers := fakeTimers(t)
	ran, done := fakeHookRunner(t, nil)
	cfg := map[string]any{"provider": "srv", "providers": map[string]any{"srv": map[string]any{
		"command":         "srv say {input_path} {output_path}",
		"warm_command":    "curl -s localhost:5002/load?model={model}",
		"release_command": "curl -s localhost:5002/unload",
		"model":           "kokoro v1",
	}}}
	swap(t, &loadTTSConfig, func(context.Context, *tools.Env) map[string]any { return cfg })
	ctx := context.Background()
	if got := AcquireTTSLease(ctx, nil, "desktop:read-aloud", cfg)["action"]; got != "warmed" {
		t.Fatalf("action %v", got)
	}
	waitHook(t, done)
	ReleaseTTSLease(ctx, nil, "desktop:read-aloud")
	pendingTimers(timers)[0].fire()
	waitHook(t, done)
	quote := "'kokoro v1'"
	if runtime.GOOS == "windows" {
		quote = `"kokoro v1"`
	}
	if want := []string{"curl -s localhost:5002/load?model=" + quote, "curl -s localhost:5002/unload"}; !reflect.DeepEqual(*ran, want) {
		t.Fatalf("ran %q want %q", *ran, want)
	}
}

func TestCommandHookThreadResolvesPassthroughUnderTheCallersScope(t *testing.T) {
	cleanLifecycle(t)
	fakeTimers(t)
	var seen []any
	var mu sync.Mutex
	_, done := fakeHookRunner(t, func(env *tools.Env, envPassthrough []string) {
		mu.Lock()
		defer mu.Unlock()
		for _, k := range envPassthrough {
			v, err := tools.ResolvePassthroughValue(env, k, nil)
			switch {
			case err != nil:
				seen = append(seen, err)
			case v == nil:
				seen = append(seen, nil)
			default:
				seen = append(seen, *v)
			}
		}
	})
	cfg := map[string]any{"provider": "srv", "providers": map[string]any{"srv": map[string]any{
		"command": "srv say {input_path} {output_path}", "warm_command": "curl -s localhost:5002/load",
		"env_passthrough": []any{"MY_TTS_TOKEN"}}}}
	swap(t, &loadTTSConfig, func(context.Context, *tools.Env) map[string]any { return cfg })
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, ".env"), []byte("MY_TTS_TOKEN=tok-work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: profile, Secrets: config.NewSecrets(profile)}
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
	if got := AcquireTTSLease(context.Background(), env, "desktop:read-aloud", cfg)["action"]; got != "warmed" {
		t.Fatalf("action %v", got)
	}
	waitHook(t, done)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []any{"tok-work"}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestLastReleaseKeepsModelWarmForConfiguredWindow(t *testing.T) {
	_, cfg := fakePiper(t)
	timers := fakeTimers(t)
	cfg["keep_warm_seconds"] = 30
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "desktop:conversation", nil)
	if got := ReleaseTTSLease(ctx, nil, "desktop:conversation"); !reflect.DeepEqual(got, map[string]any{"leases": 0, "released": 0}) {
		t.Fatalf("release %v", got)
	}
	if len(piperVoiceCache.Keys()) != 1 {
		t.Fatal("voice unloaded inside the window")
	}
	pending := pendingTimers(timers)
	if len(pending) != 1 || pending[0].interval != 30*time.Second {
		t.Fatalf("pending %v", pending)
	}
	pending[0].fire()
	if len(piperVoiceCache.Keys()) != 0 {
		t.Fatal("voice kept after the window")
	}
}

func TestReacquireWithinWindowReusesTheLoadedVoice(t *testing.T) {
	engine, _ := fakePiper(t)
	timers := fakeTimers(t)
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "desktop:conversation", nil)
	ReleaseTTSLease(ctx, nil, "desktop:conversation")
	pending := pendingTimers(timers)
	if len(pending) != 1 {
		t.Fatalf("pending %d", len(pending))
	}
	stale := pending[0]
	again := AcquireTTSLease(ctx, nil, "desktop:conversation", nil)
	if again["action"] != "cached" || engine.loadCount() != 1 || len(pendingTimers(timers)) != 0 {
		t.Fatalf("again %v loads %d pending %d", again, engine.loadCount(), len(pendingTimers(timers)))
	}
	stale.fire()
	if len(piperVoiceCache.Keys()) != 1 {
		t.Fatal("a superseded timer unloaded the voice")
	}
}

func TestReleaseDuringWarmUpStillUnloadsAfterTheWindow(t *testing.T) {
	engine, _ := fakePiper(t)
	timers := fakeTimers(t)
	ctx := context.Background()
	engine.onLoad = func() { ReleaseTTSLease(ctx, nil, "tui:voice-tts") }
	AcquireTTSLease(ctx, nil, "tui:voice-tts", nil)
	if len(TTSLeaseHolders()) != 0 || len(piperVoiceCache.Keys()) != 1 {
		t.Fatalf("holders %q cache %d", TTSLeaseHolders(), len(piperVoiceCache.Keys()))
	}
	pending := pendingTimers(timers)
	if len(pending) != 1 {
		t.Fatalf("pending %d", len(pending))
	}
	pending[0].fire()
	if len(piperVoiceCache.Keys()) != 0 {
		t.Fatal("voice resident forever")
	}
}

func TestZeroKeepWarmUnloadsInline(t *testing.T) {
	_, cfg := fakePiper(t)
	timers := fakeTimers(t)
	cfg["keep_warm_seconds"] = 0
	ctx := context.Background()
	AcquireTTSLease(ctx, nil, "cli:voice-tts", nil)
	if got := ReleaseTTSLease(ctx, nil, "cli:voice-tts"); !reflect.DeepEqual(got, map[string]any{"leases": 0, "released": 1}) {
		t.Fatalf("release %v", got)
	}
	if len(piperVoiceCache.Keys()) != 0 || len(pendingTimers(timers)) != 0 {
		t.Fatal("inline unload left state")
	}
}

func TestWarmSdkUsesPmAvailabilityAndRefusesInactiveGeneration(t *testing.T) {
	for _, installed := range []bool{true, false} {
		t.Run(map[bool]string{true: "True", false: "False"}[installed], func(t *testing.T) {
			cleanLifecycle(t)
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pm.PatchRepoRoot(root))
			prevAnchor := pm.SwapAnchorImportable(func(context.Context, string) bool { return installed })
			t.Cleanup(func() { pm.SwapAnchorImportable(prevAnchor) })
			candidate := filepath.Join(filepath.Dir(pm.RuntimeFactsPath(ctx)), "environments", "new", "venv")
			if err := os.MkdirAll(candidate, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(candidate, "pyvenv.cfg"), []byte("home = fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var calls [][]string
			prevSync := pm.SwapClientSyncVenv(func(ctx context.Context, opts pm.SyncVenvOptions) error {
				calls = append(calls, opts.Extras)
				facts, err := pm.NewFacts(pm.RuntimeFactsPath(ctx), false)
				if err != nil {
					return err
				}
				return facts.RecordState("venv", "new", opts.Extras, candidate, "")
			})
			t.Cleanup(func() { pm.SwapClientSyncVenv(prevSync) })
			result := WarmTTSProvider(ctx, nil, map[string]any{"provider": "edge"}, "")
			wantAction, wantCalls := "cached", [][]string(nil)
			if !installed {
				wantAction, wantCalls = "error", [][]string{{"edge-tts"}}
			}
			if result["warmed"] != installed || result["action"] != wantAction || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("result %v calls %q", result, calls)
			}
			if !installed && !strings.Contains(result["error"].(string), "restart") {
				t.Fatalf("error %q", result["error"])
			}
		})
	}
}
