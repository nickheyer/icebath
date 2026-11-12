package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// The wake engine gate of the hermes pyproject extras-platforms table
var wakeHermesGates = map[string]string{
	"wake-openwakeword": "(platform_machine != 'ARM64' or sys_platform != 'win32') and (platform_machine != 'x86_64' or sys_platform != 'darwin')",
}

// Installs a gate table evaluated in process like packaging Marker.evaluate
func wakeInProcessGates(t *testing.T, gates map[string]string) {
	t.Helper()
	savedGates := pm.SwapPlatformGates(gates)
	t.Cleanup(func() { pm.SwapPlatformGates(savedGates) })
}

// Points the core tree's venv interpreter at the host Python, whose marker environment gates fall back on
func wakeHostInterpreter(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	host, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	python := pm.VenvPython(filepath.Join(repo, "venv"))
	if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, python); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pm.PatchRepoRoot(repo))
}

// extra_supported bound to a marker environment with no anchor importable
func wakeSupportedOn(system, machine string) func(string) (bool, error) {
	env := map[string]string{"sys_platform": system, "platform_machine": machine}
	return func(extra string) (bool, error) {
		return pm.ExtraSupportedIn(context.Background(), extra, env, func(string) bool { return false })
	}
}

// Pins the STT and TTS probes and empties the platform gates under a private home
func wakeVoiceLoopReady(t *testing.T, stt, tts bool) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swap(t, &wakeSTTReady, func(context.Context) bool { return stt })
	swap(t, &wakeTTSReady, func(context.Context) bool { return tts })
	savedGates := pm.SwapPlatformGates(map[string]string{})
	t.Cleanup(func() { pm.SwapPlatformGates(savedGates) })
}

func wakeAvailable(v bool) func(context.Context, string) (bool, error) {
	return func(context.Context, string) (bool, error) { return v, nil }
}

func wakeLazy(v bool) func(context.Context) bool { return func(context.Context) bool { return v } }

func mustProvider(t *testing.T, cfg map[string]any, supported func(string) (bool, error)) string {
	t.Helper()
	p, err := wakeProvider(context.Background(), cfg, supported)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRequirements(t *testing.T, cfg map[string]any, supported func(string) (bool, error)) WakeRequirements {
	t.Helper()
	r, err := CheckWakeWordRequirements(context.Background(), cfg, supported)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestConfigDefaultsAndClamping(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if a, b := mustProvider(t, map[string]any{}, nil), mustProvider(t, LoadWakeWordConfig(ctx), nil); a != b {
		t.Fatalf("provider of {} %q != provider of the loaded config %q", a, b)
	}
	if p := mustProvider(t, map[string]any{"provider": "Porcupine"}, nil); p != "porcupine" {
		t.Fatalf("provider = %q", p)
	}
	for _, c := range []struct {
		cfg  map[string]any
		want any
	}{
		{map[string]any{}, nil},
		{map[string]any{"input_device": 7}, 7},
		{map[string]any{"input_device": " Microphone Array "}, "Microphone Array"},
		{map[string]any{"input_device": ""}, nil},
		{map[string]any{"input_device": false}, nil},
	} {
		if got := wakeInputDevice(c.cfg); got != c.want {
			t.Errorf("input device of %v = %#v, want %#v", c.cfg, got, c.want)
		}
	}
	for _, c := range []struct {
		cfg  map[string]any
		want float64
	}{
		{map[string]any{"sensitivity": 5}, 1.0},
		{map[string]any{"sensitivity": -1}, 0.0},
		{map[string]any{"sensitivity": "nope"}, wakeDefaults["sensitivity"].(float64)},
		{map[string]any{}, wakeDefaults["sensitivity"].(float64)},
	} {
		got, err := wakeSensitivity(c.cfg)
		if err != nil || got != c.want {
			t.Errorf("sensitivity of %v = %v, %v; want %v", c.cfg, got, err, c.want)
		}
	}
	if got := WakePhrase(ctx, map[string]any{"phrase": "hey hermes"}); got != "hey hermes" {
		t.Errorf("phrase = %q", got)
	}
	if got := WakePhrase(ctx, map[string]any{}); got != "hey hermes" {
		t.Errorf("default phrase = %q", got)
	}
}

func TestWakeSurfaceEnabledGate(t *testing.T) {
	ctx := context.Background()
	if WakeSurfaceEnabled(ctx, "cli", map[string]any{"enabled": false, "surface": "cli"}) {
		t.Fatal("disabled config enabled a surface")
	}
	for _, s := range []string{"cli", "tui", "gui"} {
		if !WakeSurfaceEnabled(ctx, s, map[string]any{"enabled": true, "surface": "auto"}) {
			t.Fatalf("auto surface refused %s", s)
		}
	}
	cfg := map[string]any{"enabled": true, "surface": "tui"}
	if !WakeSurfaceEnabled(ctx, "tui", cfg) || WakeSurfaceEnabled(ctx, "cli", cfg) || WakeSurfaceEnabled(ctx, "gui", cfg) {
		t.Fatal("pinned surface must admit only tui")
	}
	if !WakeSurfaceEnabled(ctx, "gui", map[string]any{"enabled": true}) {
		t.Fatal("a missing surface must default to auto")
	}
}

func TestLooksLikePath(t *testing.T) {
	if !looksLikePath("models/hey_hermes.onnx") || !looksLikePath("custom.ppn") || looksLikePath("hey_jarvis") {
		t.Fatal("path heuristic mismatch")
	}
}

func TestLoadedWakeDefaultsResolveSupportedProvider(t *testing.T) {
	platforms := []struct{ system, machine, expected string }{
		{"win32", "ARM64", "sherpa"},
		{"win32", "AMD64", "openwakeword"},
		{"darwin", "x86_64", "sherpa"},
		{"darwin", "arm64", "openwakeword"},
		{"linux", "x86_64", "openwakeword"},
		{"linux", "aarch64", "openwakeword"},
	}
	saves := []*string{nil, ptr("wake_word: {}\n"), ptr("wake_word:\n  provider: auto\n")}
	for _, p := range platforms {
		for _, saved := range saves {
			t.Run(p.system+"-"+p.machine, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("ICEBATH_HOME", dir)
				wakeInProcessGates(t, wakeHermesGates)
				path := filepath.Join(dir, "config.yaml")
				if saved != nil {
					if err := os.WriteFile(path, []byte(*saved), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				cfg := LoadWakeWordConfig(context.Background())
				if cfg["provider"] != "auto" {
					t.Fatalf("loaded provider = %v", cfg["provider"])
				}
				supported := wakeSupportedOn(p.system, p.machine)
				if got := mustProvider(t, cfg, supported); got != p.expected {
					t.Fatalf("provider of loaded config = %q, want %q", got, p.expected)
				}
				if got := mustProvider(t, map[string]any{}, supported); got != p.expected {
					t.Fatalf("provider of {} = %q, want %q", got, p.expected)
				}
				if ok, err := supported(WakeProviders[p.expected].Extra); err != nil || !ok {
					t.Fatalf("expected provider unsupported: %v %v", ok, err)
				}
				if WakeSurfaceEnabled(context.Background(), "gui", cfg) {
					t.Fatal("default config enabled the gui surface")
				}
				if cfg["provider"] != "auto" {
					t.Fatal("probing mutated the config")
				}
				if saved != nil {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != *saved {
						t.Fatalf("config rewritten to %q, %v", data, err)
					}
				}
			})
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestLoadWakeWordConfigGuardsNonDict(t *testing.T) {
	swap(t, &wakeLoadConfig, func(context.Context) (config.Tree, error) { return config.Tree{"wake_word": "oops"}, nil })
	if got := LoadWakeWordConfig(context.Background()); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// Engine stand in that a dispatch test identifies by label
type labelWakeEngine struct {
	wakeEngineBase
	label string
}

func (e *labelWakeEngine) Process([]int16) (bool, error) { return false, nil }

func swapEngineClass(t *testing.T, name, label string) {
	t.Helper()
	saved := wakeEngineClasses[name]
	wakeEngineClasses[name] = func(context.Context, map[string]any) (WakeEngine, error) {
		return &labelWakeEngine{label: label}, nil
	}
	t.Cleanup(func() { wakeEngineClasses[name] = saved })
}

func TestBuildEngineDispatch(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	swapEngineClass(t, "_OpenWakeWordEngine", "oww")
	swapEngineClass(t, "_PorcupineEngine", "pv")
	swapEngineClass(t, "_SherpaKwsEngine", "sherpa")
	expected := map[string]string{"openwakeword": "oww", "porcupine": "pv", "sherpa": "sherpa"}[mustProvider(t, map[string]any{}, nil)]
	label := func(cfg map[string]any) string {
		t.Helper()
		e, err := wakeBuildEngine(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return e.(*labelWakeEngine).label
	}
	if got := label(LoadWakeWordConfig(ctx)); got != expected {
		t.Fatalf("loaded config built %q, want %q", got, expected)
	}
	if got := label(map[string]any{"provider": "auto"}); got != expected {
		t.Fatalf("auto built %q, want %q", got, expected)
	}
	if label(map[string]any{"provider": "openwakeword"}) != "oww" || label(map[string]any{"provider": "porcupine"}) != "pv" {
		t.Fatal("explicit providers built the wrong engine")
	}
	_, err := wakeBuildEngine(ctx, map[string]any{"provider": "bogus"})
	var we *WakeWordError
	if !asError(err, &we) || we.Class != "ValueError" || we.Msg != "Unknown wake_word provider: 'bogus'" {
		t.Fatalf("bogus provider error = %v", err)
	}
}

func asError[T error](err error, target *T) bool {
	if err == nil {
		return false
	}
	v, ok := err.(T)
	if ok {
		*target = v
	}
	return ok
}

func TestEngineClassesAreOwnedByTheExtractedModule(t *testing.T) {
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(wakeEngineClasses["_OpenWakeWordEngine"], newOpenWakeWordEngine) ||
		!same(wakeEngineClasses["_SherpaKwsEngine"], newSherpaKwsEngine) ||
		!same(wakeEngineClasses["_PorcupineEngine"], newPorcupineEngine) {
		t.Fatal("the dispatch table must name the engine constructors themselves")
	}
	for alias, entry := range WakeProviders {
		if _, ok := wakeEngineClasses[entry.Engine]; !ok {
			t.Fatalf("provider %s names an unknown engine class %s", alias, entry.Engine)
		}
	}
}

// pyopen_wakeword stand in recording the model path and yielding nothing
type fakeOWWModule struct{ paths *[]string }

type fakeOWWFeatures struct{}

func (fakeOWWFeatures) ProcessStreaming([]byte, func([]float32) bool) error { return nil }
func (fakeOWWFeatures) Reset()                                              {}
func (fakeOWWFeatures) Close()                                              {}

type fakeOWWModel struct{ id string }

func (m fakeOWWModel) ID() string                                         { return m.id }
func (fakeOWWModel) ProcessStreaming([]float32, func(float32) bool) error { return nil }
func (fakeOWWModel) Reset()                                               {}
func (fakeOWWModel) Close()                                               {}

func (f fakeOWWModule) FeaturesFromBuiltin() (owwFeatures, error) { return fakeOWWFeatures{}, nil }

func (f fakeOWWModule) ModelFromModel(path string) (owwModel, error) {
	if f.paths != nil {
		*f.paths = append(*f.paths, path)
	}
	base := filepath.Base(path)
	return fakeOWWModel{id: strings.TrimSuffix(base, filepath.Ext(base))}, nil
}

func TestEngineConstructionEnsuresAudioIOOnlyForLocalCapture(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	var ensured []string
	swap(t, &wakeEnsureImport, func(_ context.Context, feature string) error { ensured = append(ensured, feature); return nil })
	swap(t, &wakePMAvailable, func(_ context.Context, feature string) (bool, error) { return slices.Contains(ensured, feature), nil })
	swap(t, &wakeImportPyOpenWakeWord, func(context.Context) (pyOpenWakeWordModule, error) { return fakeOWWModule{}, nil })
	cfg := map[string]any{"provider": "openwakeword"}
	with := func(capture string) map[string]any {
		return map[string]any{"provider": "openwakeword", "capture": capture}
	}
	if _, err := newOpenWakeWordEngine(ctx, with("client")); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ensured, "wake-openwakeword") || slices.Contains(ensured, "audio-io") {
		t.Fatalf("client capture ensured %v", ensured)
	}
	ensured = nil
	if _, err := newOpenWakeWordEngine(ctx, with("local")); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ensured, "wake-openwakeword") || !slices.Contains(ensured, "audio-io") {
		t.Fatalf("local capture ensured %v", ensured)
	}
	ensured = nil
	swap(t, &wakeLockPath, func() string { return filepath.Join(t.TempDir(), "wake.lock") })
	owner := new(int)
	defer StopListening(owner)
	if _, err := StartListening(ctx, func() error { return nil }, owner, cfg, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ensured, "wake-openwakeword") || slices.Contains(ensured, "audio-io") {
		t.Fatalf("client listener ensured %v", ensured)
	}
}

func TestLoadedProviderRequirementsPreserveChoicesAndRequireKeys(t *testing.T) {
	providers := append([]string{"auto"}, WakeProviderNames...)
	for _, plat := range []struct{ system, machine string }{{"win32", "ARM64"}, {"darwin", "x86_64"}} {
		for _, provider := range providers {
			t.Run(plat.system+"-"+provider, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("ICEBATH_HOME", dir)
				t.Setenv("PORCUPINE_ACCESS_KEY", "")
				os.Unsetenv("PORCUPINE_ACCESS_KEY")
				wakeInProcessGates(t, wakeHermesGates)
				saved := "wake_word:\n  provider: " + provider + "\n  capture: client\n"
				path := filepath.Join(dir, "config.yaml")
				if err := os.WriteFile(path, []byte(saved), 0o644); err != nil {
					t.Fatal(err)
				}
				cfg := LoadWakeWordConfig(context.Background())
				supported := wakeSupportedOn(plat.system, plat.machine)
				swap(t, &wakePMAvailable, wakeAvailable(false))
				swap(t, &wakeLazyInstallsAllowed, wakeLazy(true))
				swap(t, &wakeSTTReady, func(context.Context) bool { return true })
				swap(t, &wakeTTSReady, func(context.Context) bool { return true })
				result := mustRequirements(t, cfg, supported)
				selected := mustProvider(t, cfg, supported)
				if result.Provider != selected {
					t.Fatalf("result provider %q, selected %q", result.Provider, selected)
				}
				if provider != "auto" && selected != provider {
					t.Fatalf("explicit provider %q resolved to %q", provider, selected)
				}
				ok, err := supported(WakeProviders[selected].Extra)
				switch {
				case err != nil:
					t.Fatal(err)
				case !ok:
					if result.Available || !strings.Contains(result.Hint, "not supported on this platform") {
						t.Fatalf("unsupported engine result %+v", result)
					}
				case selected == "porcupine":
					if result.Available || result.AccessKeySet || !strings.Contains(result.Hint, "PORCUPINE_ACCESS_KEY") {
						t.Fatalf("keyless porcupine result %+v", result)
					}
					t.Setenv("PORCUPINE_ACCESS_KEY", "test-key")
					if !mustRequirements(t, cfg, supported).Available {
						t.Fatal("porcupine with a key must be available")
					}
				default:
					if !result.Available {
						t.Fatalf("supported engine unavailable %+v", result)
					}
				}
				if WakeSurfaceEnabled(context.Background(), "gui", cfg) || cfg["provider"] != provider {
					t.Fatal("probe changed the configured choice")
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != saved {
					t.Fatalf("config rewritten to %q, %v", data, err)
				}
			})
		}
	}
}

func TestRequirementsOpenwakewordAvailable(t *testing.T) {
	wakeVoiceLoopReady(t, true, true)
	swap(t, &wakeAudioAvailable, func() (bool, error) { return true, nil })
	swap(t, &wakePMAvailable, wakeAvailable(true))
	r := mustRequirements(t, map[string]any{"provider": "openwakeword", "phrase": "hey hermes"}, nil)
	if !r.Available || r.Provider != "openwakeword" || r.Phrase != "hey hermes" {
		t.Fatalf("got %+v", r)
	}
}

func TestTTSReadyIsAProbeNeverAnInstaller(t *testing.T) {
	ctx := context.Background()
	checks := 0
	checkOK := false
	swap(t, &wakeGetTTSProvider, func(map[string]any) (string, error) { return "edge", nil })
	swap(t, &wakeLoadTTSConfig, func(context.Context) map[string]any { return map[string]any{} })
	swap(t, &wakeCheckTTS, func(context.Context) (bool, error) { checks++; return checkOK, nil })
	swap(t, &wakePMAvailable, wakeAvailable(false))
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(true))
	if !ttsReady(ctx) || checks != 0 {
		t.Fatalf("missing lazily installable deps must be ready without a check, checks=%d", checks)
	}
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(false))
	if ttsReady(ctx) || checks != 0 {
		t.Fatalf("missing deps with lazy installs off must be unready without a check, checks=%d", checks)
	}
	checkOK = true
	swap(t, &wakePMAvailable, wakeAvailable(true))
	if !ttsReady(ctx) || checks != 1 {
		t.Fatalf("present deps must defer to the requirements check, checks=%d", checks)
	}
}

func TestRequirementsFreshInstallLazyAllowed(t *testing.T) {
	wakeVoiceLoopReady(t, true, true)
	probed := false
	swap(t, &wakeAudioAvailable, func() (bool, error) { probed = true; return false, nil })
	swap(t, &wakePMAvailable, wakeAvailable(false))
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(true))
	r := mustRequirements(t, map[string]any{"provider": "openwakeword"}, nil)
	if probed {
		t.Fatal("audio probe must not run while deps are missing")
	}
	if !r.Available || r.DepsAvailable || r.Hint != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestRequirementsRejectUnsupportedEngineWithoutAttemptingInstall(t *testing.T) {
	for _, capture := range []string{"local", "client"} {
		for _, provider := range []string{"openwakeword", "oww", "local"} {
			t.Run(capture+"-"+provider, func(t *testing.T) {
				wakeVoiceLoopReady(t, true, true)
				wakeHostInterpreter(t)
				wakeInProcessGates(t, map[string]string{"wake-openwakeword": "python_version < '0'"})
				savedImportable := pm.SwapAnchorImportable(func(context.Context, string) bool { return false })
				t.Cleanup(func() { pm.SwapAnchorImportable(savedImportable) })
				swap(t, &wakeLazyInstallsAllowed, wakeLazy(true))
				t.Setenv("PORCUPINE_ACCESS_KEY", "test-key")
				swap(t, &wakeEnsureImport, func(context.Context, string) error {
					t.Error("a requirements probe must not install dependencies")
					return nil
				})
				r := mustRequirements(t, map[string]any{"provider": provider, "capture": capture}, nil)
				if r.Available || !strings.Contains(r.Hint, "not supported") || !strings.Contains(r.Hint, "wake_word.provider") {
					t.Fatalf("got %+v", r)
				}
				if !strings.Contains(r.Hint, "sherpa") || !strings.Contains(r.Hint, "porcupine") || strings.Contains(r.Hint, "uv sync") {
					t.Fatalf("hint %q", r.Hint)
				}
				for _, alternative := range []string{"sherpa", "porcupine"} {
					if !mustRequirements(t, map[string]any{"provider": alternative, "capture": capture}, nil).Available {
						t.Fatalf("%s must be available", alternative)
					}
				}
			})
		}
	}
}

func TestRequirementsLazyDisabledReturnsRemedyNotNameerror(t *testing.T) {
	wakeVoiceLoopReady(t, true, true)
	swap(t, &wakeAudioAvailable, func() (bool, error) { return true, nil })
	swap(t, &wakePMAvailable, wakeAvailable(false))
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(false))
	r := mustRequirements(t, map[string]any{"provider": "openwakeword"}, nil)
	if r.Available || r.DepsAvailable || !strings.Contains(r.Hint, "icebath pm install --extra wake-openwakeword") {
		t.Fatalf("got %+v", r)
	}
}

func TestRequirementsDepsPresentButNoAudioHint(t *testing.T) {
	wakeVoiceLoopReady(t, true, true)
	swap(t, &wakeAudioAvailable, func() (bool, error) { return false, nil })
	swap(t, &wakeLocalInputReady, func() bool { return false })
	swap(t, &wakePMAvailable, wakeAvailable(true))
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(true))
	r := mustRequirements(t, map[string]any{"provider": "openwakeword", "capture": "local"}, nil)
	if r.Available || !(strings.Contains(r.Hint, "audio device") || strings.Contains(strings.ToLower(r.Hint), "microphone")) {
		t.Fatalf("got %+v", r)
	}
}

func TestOpenwakewordCustomModelPathUsed(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var paths []string
	swap(t, &wakeImportPyOpenWakeWord, func(context.Context) (pyOpenWakeWordModule, error) { return fakeOWWModule{paths: &paths}, nil })
	swap(t, &wakeEnsureImport, func(context.Context, string) error { return nil })
	eng, err := newOpenWakeWordEngine(context.Background(), map[string]any{"provider": "openwakeword", "openwakeword": map[string]any{"model": "/models/hey_hermes.tflite"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/models/hey_hermes.tflite" {
		t.Fatalf("model paths %v", paths)
	}
	if labels := eng.(*openWakeWordEngine).labels; !slices.Equal(labels, []string{"hey_hermes"}) {
		t.Fatalf("labels %v", labels)
	}
}

func TestBundledHeyHermesModelShipsOnDisk(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	path, err := bundledWakewordPath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() <= 1024 {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestResolveCaptureModeAutoAndPreferClient(t *testing.T) {
	ctx := context.Background()
	ready := false
	swap(t, &wakeLocalInputReady, func() bool { return ready })
	mode := func(capture string, preferClient, forceLocal bool) string {
		return ResolveCaptureMode(ctx, map[string]any{"capture": capture}, preferClient, forceLocal)
	}
	if mode("auto", false, false) != "local" || mode("auto", true, false) != "client" || mode("local", true, false) != "local" ||
		mode("client", false, false) != "client" || mode("auto", false, true) != "local" {
		t.Fatal("capture mode without a backend mic")
	}
	ready = true
	if mode("auto", false, false) != "local" || mode("auto", true, false) != "local" || mode("client", true, false) != "client" {
		t.Fatal("capture mode with a backend mic")
	}
}

func TestRequirementsClientCaptureWithoutLocalMic(t *testing.T) {
	swap(t, &wakeAudioAvailable, func() (bool, error) { return false, nil })
	swap(t, &wakeLocalInputReady, func() bool { return false })
	swap(t, &wakeSTTReady, func(context.Context) bool { return true })
	swap(t, &wakeTTSReady, func(context.Context) bool { return true })
	swap(t, &wakePMAvailable, wakeAvailable(true))
	swap(t, &wakeLazyInstallsAllowed, wakeLazy(false))
	r := mustRequirements(t, map[string]any{"capture": "client", "provider": "openwakeword"}, nil)
	if !r.Available || r.Capture != "client" {
		t.Fatalf("got %+v", r)
	}
}
