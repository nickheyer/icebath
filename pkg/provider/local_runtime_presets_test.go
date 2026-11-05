package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const lr6GiB = int64(1) << 30
const lr6MiB = int64(1) << 20

// Replaces the preset header reader and profile builder for the rest of the test
func lr6FakeHeaders(t *testing.T, profile func(path string) ModelProfile) {
	t.Helper()
	prevRead, prevProfile, prevEntry := presetReadHeader, presetProfileFromGGUF, presetEntryForModel
	t.Cleanup(func() {
		presetReadHeader, presetProfileFromGGUF, presetEntryForModel = prevRead, prevProfile, prevEntry
	})
	presetReadHeader = func(p string) (*GGUFHeader, error) { return &GGUFHeader{Path: p}, nil }
	presetProfileFromGGUF = func(h *GGUFHeader) (ModelProfile, error) { return profile(h.Path), nil }
}

// Equality of two preset entries the way the Python dataclass compares them
func lr6PresetEq(a, b *PresetEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ModelID == b.ModelID && a.Window == b.Window && a.Spilled == b.Spilled && a.Refusal == b.Refusal &&
		reflect.DeepEqual(a.Keys, b.Keys)
}

func lr6Preset(t *testing.T, gguf string, budget HardwareBudget, requested int, live *HardwareBudget, engineTag string) *PresetEntry {
	t.Helper()
	p, err := PresetForModel(gguf, budget, map[string]bool{}, requested, live, 1, engineTag)
	if err != nil || p == nil {
		t.Fatalf("preset %v %v", p, err)
	}
	return p
}

func lr6Touch(t *testing.T, path string) {
	t.Helper()
	lr5Touch(t, path, "")
}

func TestLocalRuntimePresetRoundtripKeepsRefusalsAndDenseSpill(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	mdir := filepath.Join(dir, "modèles")
	for _, name := range []string{"allowed", "refused"} {
		lr6Touch(t, filepath.Join(mdir, name+".gguf"))
	}
	lr6FakeHeaders(t, func(p string) ModelProfile {
		stem := pyPathStem(p)
		weights := int64(40) << 30
		if stem == "allowed" {
			weights = 4 << 30
		}
		return NewModelProfile(stem, weights, 0, 65536, nil)
	})
	ini := filepath.Join(dir, "presets.ini")
	generated, err := GeneratePresets(mdir, NewHardwareBudget(2<<30, 2<<30, 8<<30), ini, nil, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(ini)
	if strings.HasPrefix(string(raw), "\xef\xbb\xbf") {
		t.Fatal("presets carry a BOM")
	}
	_ = os.WriteFile(ini, append([]byte("\xef\xbb\xbf"), raw...), 0o600)
	reread := readPresetDecisions(ini)
	if len(reread) != len(generated) {
		t.Fatalf("%v %v", reread, generated)
	}
	for _, p := range generated {
		if reread[p.ModelID] == nil {
			t.Fatal(p.ModelID)
		}
	}
	allowed := reread["allowed"]
	if reread["refused"].Refusal == "" || !allowed.Spilled || allowed.Keys["model"] != filepath.Join(mdir, "allowed.gguf") {
		t.Fatalf("%+v %+v", reread["refused"], allowed)
	}
	if _, ok := allowed.Keys["override-tensor"]; ok {
		t.Fatal("dense spill has no tensor pattern override")
	}
	_ = os.WriteFile(ini, []byte("\xef\xbb\xbfnot an INI section"), 0o600)
	if got := readPresetDecisions(ini); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestLocalRuntimeOptionalDraftIsEnabledOnlyWithRoomAtTheSelectedWindow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	var entry *CatalogEntry
	for _, e := range Catalog() {
		if e.Draft != nil && e.DraftSpecType == "draft-dspark" {
			entry = &e
			break
		}
	}
	if entry == nil {
		t.Fatal("no DSpark catalog entry")
	}
	main := filepath.Join(dir, entry.Variants[0].ModelID()+".gguf")
	draft := filepath.Join(localAssetsDir(), entry.Draft.LocalName())
	lr6Touch(t, draft)
	mainProfile := NewModelProfile("main", 12<<30, 0, 65536, nil)
	mainProfile.MoE = true
	draftProfile := NewModelProfile("draft", 1<<30, 0, 65536, nil)
	lr6FakeHeaders(t, func(p string) ModelProfile {
		if p == draft {
			return draftProfile
		}
		return mainProfile
	})
	tight := NewHardwareBudget(8<<30, 8<<30, 6<<30)
	result := lr6Preset(t, main, tight, 0, nil, "")
	if result.Window != 65536 || !result.Spilled || result.Keys["model-draft"] != "" {
		t.Fatalf("%+v", result)
	}
	roomy := tight
	roomy.RAMAvailableBytes = 16 << 30
	withDraft := lr6Preset(t, main, roomy, 0, nil, "")
	if withDraft.Window != result.Window || withDraft.Keys["model-draft"] != draft || withDraft.Keys["spec-type"] != "draft-dspark" {
		t.Fatalf("%+v", withDraft)
	}
	draftProfile.NCtxTrain, draftProfile.NVocab = 32768, 32768
	draftProfile.Layers = lrtLayers(LayerFull, 4096, 4)
	draftCost := draftProfile.WeightsBytes + CtxBytes(draftProfile, result.Window, false) + RuntimeOverheadBytes +
		UbLogitsBytes(draftProfile.NVocab, false, false)
	boundary := RuntimeOverheadBytes + draftCost
	exact := roomy
	exact.UsableVRAMBytes, exact.TotalDeviceBytes = boundary, boundary
	if _, ok := lr6Preset(t, main, exact, 0, nil, "").Keys["model-draft"]; !ok {
		t.Fatal("the draft must fit at the exact device boundary")
	}
	below := exact
	below.UsableVRAMBytes = boundary - 1
	if _, ok := lr6Preset(t, main, below, 0, nil, "").Keys["model-draft"]; ok {
		t.Fatal("one byte short must omit the draft")
	}
	draftProfile.WeightsBytes = 9 << 30
	if _, ok := lr6Preset(t, main, roomy, 0, nil, "").Keys["model-draft"]; ok {
		t.Fatal("a draft larger than the room must be omitted")
	}
}

func TestLocalRuntimePresetPassPropagatesHeaderErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	gguf := filepath.Join(dir, "m.gguf")
	lr6Touch(t, gguf)
	keyErr := &GGUFError{"KeyError", "general.architecture"}
	lr6FakeHeaders(t, func(string) ModelProfile { return NewModelProfile("m", lr6GiB, 0, 65536, nil) })
	presetReadHeader = func(string) (*GGUFHeader, error) { return nil, keyErr }
	budget := NewHardwareBudget(48*lr6GiB, 48*lr6GiB, 64*lr6GiB)
	if p, err := PresetForModel(gguf, budget, nil, 0, nil, 1, ""); p != nil || err != keyErr {
		t.Fatalf("preset %v %v", p, err)
	}
	if _, ok, err := residentFootprint(gguf, budget, 65536, false); ok || err != keyErr {
		t.Fatalf("footprint %v %v", ok, err)
	}
	if _, err := AdmittedResidencyCount(dir, budget, 4, nil, ""); err != keyErr {
		t.Fatalf("admission %v", err)
	}
	if _, err := PlanPresets(dir, budget, nil, nil, 1, ""); err != keyErr {
		t.Fatalf("plan %v", err)
	}
	presetReadHeader = func(string) (*GGUFHeader, error) { return nil, &GGUFError{"ValueError", "truncated"} }
	if p, err := PresetForModel(gguf, budget, nil, 0, nil, 1, ""); p != nil || err != nil {
		t.Fatalf("skippable %v %v", p, err)
	}
	if got, err := AdmittedResidencyCount(dir, budget, 4, nil, ""); got != 4 || err != nil {
		t.Fatalf("skippable admission %d %v", got, err)
	}
	lr5ResetBoot(t)
	lr5Home(t)
	logs := lr5CaptureLogs(t)
	presetReadHeader = func(string) (*GGUFHeader, error) { return nil, keyErr }
	lr6Touch(t, filepath.Join(localModelsDir(), "m.gguf"))
	bootProbeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
	t.Cleanup(func() { bootProbeBudget = ProbeBudget })
	if got := admittedModelsMax(localModelsDir(), 4, ""); got != 4 || !strings.Contains(logs(), "residency cap probe failed") {
		t.Fatalf("boot cap %d %s", got, logs())
	}
	var entry CatalogEntry
	for _, e := range Catalog() {
		if e.Draft != nil && e.DraftSpecType == "draft-dspark" {
			entry = e
			break
		}
	}
	main := filepath.Join(dir, entry.Variants[0].ModelID()+".gguf")
	draft := filepath.Join(localAssetsDir(), entry.Draft.LocalName())
	lr6Touch(t, draft)
	presetReadHeader = func(p string) (*GGUFHeader, error) {
		if p == draft {
			return nil, keyErr
		}
		return &GGUFHeader{Path: p}, nil
	}
	tight := NewHardwareBudget(8<<30, 8<<30, 16<<30)
	presetProfileFromGGUF = func(*GGUFHeader) (ModelProfile, error) {
		p := NewModelProfile("main", 12<<30, 0, 65536, nil)
		p.MoE = true
		return p, nil
	}
	if p, err := PresetForModel(main, tight, nil, 0, nil, 1, ""); p != nil || err != keyErr {
		t.Fatalf("draft header %v %v", p, err)
	}
}

func TestLocalRuntimeProbeBudgetReturnsTheEscapedProbeError(t *testing.T) {
	lr2Isolate(t)
	probeErr := &HardwareProbeError{Class: "TimeoutExpired", Msg: "Command '['getconf']' timed out after 5 seconds"}
	hwRAMBytes = func() (int64, int64, error) { return 0, 0, probeErr }
	if _, err := ProbeBudget(true); err != probeErr {
		t.Fatalf("probe %v", err)
	}
	lr5ResetBoot(t)
	lr5Home(t)
	logs := lr5CaptureLogs(t)
	if got := admittedModelsMax(localModelsDir(), 4, ""); got != 4 || !strings.Contains(logs(), "residency cap probe failed") {
		t.Fatalf("cap %d", got)
	}
	if got := generateBootPresets(localModelsDir(), filepath.Join(RuntimesRoot(), "presets.ini"), 1, ""); got != "" {
		t.Fatalf("presets %q", got)
	}
}

func TestLocalRuntimeEndpointWrappersReadErrorsAsNoEndpointSilently(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	prevState := lrStatePath
	t.Cleanup(func() { lrStatePath = prevState })
	lrStatePath = func() string { return filepath.Join(dir, "runtimes", "llamacpp", "server.json") }
	logs := lr5CaptureLogs(t)
	bad := map[string]any{"local_runtime": map[string]any{"detect_ports": []any{"x"}}}
	if _, err := ResolveLlamacppEndpoint(context.Background(), bad, dir, 0); err == nil {
		t.Fatal("a bad detect port must raise")
	}
	if base, key, ok := resolveLlamacppEndpointWithin(context.Background(), bad, dir, 0); ok || base != "" || key != "" {
		t.Fatal("an escaped error must read as no endpoint")
	}
	if logs() != "" {
		t.Fatalf("callers swallow the error without logging %s", logs())
	}
}

func TestLocalRuntimeBootKickLogsTheEnsureError(t *testing.T) {
	logs := lr5CaptureLogs(t)
	prev := ensureLocalRuntimeFn
	t.Cleanup(func() { ensureLocalRuntimeFn = prev })
	ensureLocalRuntimeFn = func(context.Context, map[string]any) (*LlamaServerSupervisor, error) {
		return nil, errors.New("'str' object has no attribute 'get'")
	}
	kickManagedBoot(context.Background(), map[string]any{}, t.TempDir())
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs(), "on-demand managed-server boot failed: 'str' object has no attribute 'get'") {
		if time.Now().After(deadline) {
			t.Fatalf("no boot failure log %s", logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	kickManagedBootMu.Lock()
	kickManagedBootMu.Unlock()
}

// Stages names under the machine models dir with a GGUF magic header
func lr6Stage(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		lr5Touch(t, filepath.Join(localModelsDir(), name+".gguf"), "GGUF"+strings.Repeat("\x00", 32))
	}
}

// Writes a preset INI covering model ids at the machine runtimes root
func lr6WritePresets(t *testing.T, ids ...string) string {
	t.Helper()
	var body []string
	for _, m := range ids {
		body = append(body, "["+m+"]\nmodel = "+filepath.Join(localModelsDir(), m+".gguf")+"\nctx-size = 65536\n")
	}
	ini := filepath.Join(RuntimesRoot(), "presets.ini")
	lr5Touch(t, ini, strings.Join(body, "\n"))
	return ini
}

func TestLocalRuntimePresetsStaleWhenAStagedModelHasNoSection(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "model-a", "model-b")
	lr6WritePresets(t, "model-a")
	if !presetsStale() {
		t.Fatal("a staged model without a section is stale")
	}
}

func TestLocalRuntimePresetsCurrentWhenEveryStagedModelIsCovered(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "model-a")
	lr6WritePresets(t, "model-a")
	if presetsStale() {
		t.Fatal("covered models are current")
	}
}

func TestLocalRuntimeLegacyPresetsWithoutModelPathsAreRegenerated(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "model-a")
	ini := lr6WritePresets(t, "model-a")
	lr5Touch(t, ini, "[model-a]\nctx-size = 65536\n")
	if !presetsStale() {
		t.Fatal("a section without a model path is stale")
	}
}

func TestLocalRuntimeNoModelsIsNeverStale(t *testing.T) {
	lr5Home(t)
	lr6WritePresets(t, "model-a")
	if presetsStale() {
		t.Fatal("no staged models is never stale")
	}
}

func TestLocalRuntimeBootReplacesIncumbentWithStalePresets(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	lr6Stage(t, "model-a", "model-b")
	lr6WritePresets(t, "model-a")
	prevStop := bootStopStateServer
	t.Cleanup(func() { bootStopStateServer = prevStop })
	stopped := false
	bootStopStateServer = func() error { stopped = true; return nil }
	bootStateEndpoint = func(context.Context) (bool, error) { return true, nil }
	bootInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, errors.New("boot reached") }
	sup, err := EnsureLocalRuntime(context.Background(), map[string]any{"local_runtime": map[string]any{"enabled": true}}, false)
	if !stopped {
		t.Fatal("stale incumbent was not stopped")
	}
	if sup != nil || err != nil {
		t.Fatal(sup, err)
	}
}

func TestLocalRuntimeRefreshBouncesAnAdoptedServer(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	prevStop, prevEnsure := bootStopStateServer, bootEnsureLocalRuntime
	t.Cleanup(func() { bootStopStateServer, bootEnsureLocalRuntime = prevStop, prevEnsure })
	stopped := false
	bootStopStateServer = func() error { stopped = true; return nil }
	bootStateEndpoint = func(context.Context) (bool, error) { return true, nil }
	var forced []bool
	bootEnsureLocalRuntime = func(_ context.Context, _ map[string]any, force bool) (*LlamaServerSupervisor, error) {
		forced = append(forced, force)
		return &LlamaServerSupervisor{}, nil
	}
	if !refreshLocalRuntime() {
		t.Fatal("refresh must report the bounce")
	}
	if !stopped {
		t.Fatal("adopted server was not stopped")
	}
	if !reflect.DeepEqual(forced, []bool{true}) {
		t.Fatalf("fresh boot did not follow the stop %v", forced)
	}
}

func TestLocalRuntimeRefreshNoServerAnywhereIsANoop(t *testing.T) {
	lr5Home(t)
	lr5ResetBoot(t)
	bootStateEndpoint = func(context.Context) (bool, error) { return false, nil }
	if refreshLocalRuntime() {
		t.Fatal("nothing to refresh")
	}
}

// Stages weights by stem in a models dir priced through the fake header reader
func lr6Staged(t *testing.T, mdir string, weights map[string]int64) {
	t.Helper()
	for stem := range weights {
		lr6Touch(t, filepath.Join(mdir, stem+".gguf"))
	}
	lr6FakeHeaders(t, func(p string) ModelProfile {
		stem := pyPathStem(p)
		return NewModelProfile(stem, weights[stem], 0, 65536, nil)
	})
}

func TestLocalRuntimeResidencyCapIsPricedAgainstTheCard(t *testing.T) {
	tight := NewHardwareBudget(6*lr6GiB, 8*lr6GiB, 8*lr6GiB)
	roomy := NewHardwareBudget(48*lr6GiB, 48*lr6GiB, 64*lr6GiB)
	noDevice := NewHardwareBudget(0, 0, 8*lr6GiB)
	gib := lr6GiB
	cases := []struct {
		weights    map[string]int64
		budget     HardwareBudget
		configured int
		want       int
	}{
		{map[string]int64{"nine-b": 6 * lr6GiB, "gemma-e4b": int64(3.9 * float64(gib))}, tight, 4, 1},
		{map[string]int64{"a": 6 * lr6GiB, "b": 6 * lr6GiB}, roomy, 4, 4},
		{map[string]int64{"a": 1 * lr6GiB}, roomy, 1, 1},
		{map[string]int64{"a": 6 * lr6GiB}, noDevice, 4, 4},
		{map[string]int64{"tiny": 2 * lr6GiB, "huge": 200 * lr6GiB}, roomy, 4, 4},
	}
	for i, c := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
			mdir := filepath.Join(dir, "models")
			lr6Staged(t, mdir, c.weights)
			if got, err := AdmittedResidencyCount(mdir, c.budget, c.configured, nil, ""); got != c.want || err != nil {
				t.Fatalf("got %d %v want %d", got, err, c.want)
			}
		})
	}
}

func TestLocalRuntimeBootHandsTheRouterTheDerivedCap(t *testing.T) {
	tight := NewHardwareBudget(6*lr6GiB, 8*lr6GiB, 8*lr6GiB)
	boom := errors.New("nvidia-smi vanished mid-session")
	cases := []struct {
		configured int
		probeErr   error
		want       int
	}{{4, nil, 1}, {1, nil, 1}, {4, boom, 4}}
	for i, c := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
			lr5ResetBoot(t)
			lr6Staged(t, localModelsDir(), map[string]int64{"nine-b": 6 * lr6GiB})
			prevProbe := bootProbeBudget
			t.Cleanup(func() { bootProbeBudget = prevProbe })
			bootProbeBudget = func(bool) (HardwareBudget, error) { return tight, c.probeErr }
			bootGeneratePresets = func(string, string, int, string) string { return "" }
			bootNvidiaQuery = func() (*NvidiaGPUQuery, error) { return nil, nil }
			bootGPUClass = func() string { return "" }
			bootInstalledEngine = func(context.Context, string) (*Engine, error) {
				return &Engine{Backend: "cpu", Tag: "build-1", Binary: filepath.Join(dir, "install", "llama-server")}, nil
			}
			bootStateEndpoint = func(context.Context) (bool, error) { return false, nil }
			captured := -1
			bootStartSupervisor = func(s *LlamaServerSupervisor) error { captured = s.ModelsMax; return nil }
			cfg := map[string]any{"local_runtime": map[string]any{"enabled": true, "models_max": c.configured}}
			sup, err := EnsureLocalRuntime(context.Background(), cfg, false)
			if sup == nil || err != nil {
				t.Fatal(sup, err)
			}
			if captured != c.want {
				t.Fatalf("models_max %d want %d", captured, c.want)
			}
		})
	}
}

func TestLocalRuntimeQwenMediumDefaultReachesOnlyItsModelPreset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	configPath := filepath.Join(dir, "config.yaml")
	config := "agent:\n  reasoning_effort: off\n"
	lr5Touch(t, configPath, config)
	entry := CatalogByID()["qwen3.8-27b"]
	modelID := entry.Variants[0].ModelID()
	control := CatalogByID()["qwen3.8-flash-next"].Variants[0].ModelID()
	mdir := localModelsDir()
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{modelID, control, "off-catalog-model"} {
		lrtWriteGGUFTyped(t, filepath.Join(mdir, name+".gguf"), []lrtKV{
			{"general.architecture", 8, "llama"}, {"llama.context_length", 4, 65536},
			{"general.sampling.temperature", 6, 0.42}}, nil, 0)
	}
	projector := filepath.Join(localAssetsDir(), entry.MMProj.LocalName())
	lr6Touch(t, projector)
	ini := filepath.Join(dir, "presets.ini")
	generated, err := GeneratePresets(mdir, NewHardwareBudget(64<<30, 64<<30, 64<<30), ini, nil, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	reread := readPresetDecisions(ini)
	if len(reread) != len(generated) {
		t.Fatalf("%d sections for %d presets", len(reread), len(generated))
	}
	kwargs := func(keys map[string]string) map[string]any {
		raw := keys["chat-template-kwargs"]
		if raw == "" {
			raw = "{}"
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	target := reread[modelID].Keys
	if !reflect.DeepEqual(kwargs(target), map[string]any{"reasoning_effort": "medium"}) {
		t.Fatalf("%v", target)
	}
	for _, name := range []string{control, "off-catalog-model"} {
		if _, ok := kwargs(reread[name].Keys)["reasoning_effort"]; ok {
			t.Fatal(name)
		}
	}
	if target["mmproj"] != projector || target["spec-draft-n-max"] != strconv.Itoa(entry.MTPDraftDepth) || target["temp"] != "0.42" {
		t.Fatalf("%v", target)
	}
	if raw, _ := os.ReadFile(configPath); string(raw) != config {
		t.Fatal("the preset pass rewrote config")
	}
}

func TestLocalRuntimeNoStagedModelsNoRow(t *testing.T) {
	lr5Home(t)
	ctx := context.Background()
	pc, err := loadPickerContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row := localRuntimeRow(ctx, pc); row != nil {
		t.Fatal(row)
	}
}

func TestLocalRuntimeStagedModelsMakeASelectableRow(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "Qwen3-4B-Instruct-2507-UD-Q8_K_XL", "Some-Other-Model")
	ctx := context.Background()
	pc, _ := loadPickerContext(ctx)
	row := localRuntimeRow(ctx, pc)
	if row == nil || row["slug"] != "llamacpp" || row["authenticated"] != true || row["total_models"] != 2 {
		t.Fatal(row)
	}
	found := false
	for _, m := range row["models"].([]any) {
		found = found || m == "Qwen3-4B-Instruct-2507-UD-Q8_K_XL"
	}
	if !found {
		t.Fatal(row)
	}
}

func TestLocalRuntimeRowMarksCurrentWhenConfigPointsAtLlamacpp(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "M")
	ctx := context.Background()
	pc, _ := loadPickerContext(ctx)
	if row := localRuntimeRow(ctx, pc.WithOverrides("llamacpp", "", "")); row == nil || row["is_current"] != true {
		t.Fatal(row)
	}
}

func TestLocalRuntimeFullPayloadIncludesLocalRow(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "Local-Model-X")
	ctx := context.Background()
	pc, _ := loadPickerContext(ctx)
	payload, err := buildModelsPayload(ctx, pc, ModelsPayloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range payload["providers"].([]any) {
		p := raw.(map[string]any)
		if p["slug"] == "llamacpp" {
			if !reflect.DeepEqual(p["models"], []any{"Local-Model-X"}) {
				t.Fatal(p["models"])
			}
			return
		}
	}
	t.Fatal("no llamacpp row")
}

func TestLocalRuntimeExplicitOnlyFilterKeepsLocalRowOnAnyProfile(t *testing.T) {
	lr5Home(t)
	lr6Stage(t, "Qwen3.8-27B-UD-Q5_K_XL")
	ctx := context.Background()
	pc, _ := loadPickerContext(ctx)
	row := localRuntimeRow(ctx, pc)
	if row == nil {
		t.Fatal("no row")
	}
	pc.CurrentProvider = "anthropic"
	kept := filterExplicitProviderRows(ctx, []map[string]any{row}, pc)
	if len(kept) == 0 || kept[0]["slug"] != "llamacpp" {
		t.Fatal("explicit-only filter dropped the local-runtime row")
	}
}

func TestLocalRuntimeCatalogPresetPricesIdenticalModelFacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	for _, entry := range Catalog() {
		variant := entry.Variants[0]
		profile := entry.Profile(variant)
		path := filepath.Join(dir, variant.ModelID()+".gguf")
		lr6FakeHeaders(t, func(string) ModelProfile { return profile })
		for _, companion := range []*AssetFile{entry.MMProj, entry.MTPHead} {
			if companion != nil {
				lr6Touch(t, filepath.Join(localAssetsDir(), companion.LocalName()))
			}
		}
		for _, vram := range []int64{16, 24, 32, 48} {
			for _, uma := range []bool{false, true} {
				ram := 32 * lr6GiB
				if uma {
					ram = 0
				}
				machine := NewHardwareBudget(int64(float64(vram*lr6GiB)*0.8), vram*lr6GiB, ram)
				machine.UMA = uma
				preset := lr6Preset(t, path, machine, 0, nil, "")
				if preset.Refusal != "" {
					continue
				}
				mtp := entry.MTPCapable()
				prefill := preset.Keys["ubatch-size"] == "2048" && mtp
				overhead := RuntimeOverheadBytes + entry.CompanionBytes() + UbLogitsBytes(profile.NVocab, mtp, prefill)
				posture := PostureProfile(profile, mtp, prefill)
				need := profile.WeightsBytes + CtxBytes(posture, preset.Window, true) + overhead
				if preset.Spilled != (need > machine.UsableVRAMBytes) || need > machine.UsableVRAMBytes+machine.RAMAvailableBytes {
					t.Fatalf("%s %d %v spilled=%v need=%d", entry.ID, vram, uma, preset.Spilled, need)
				}
			}
		}
	}
}

func TestLocalRuntimeDefaultProfilePathsUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	if localModelsDir() != filepath.Join(root, "models") || RuntimesRoot() != filepath.Join(root, "runtimes", "llamacpp") {
		t.Fatal(localModelsDir(), RuntimesRoot())
	}
}
