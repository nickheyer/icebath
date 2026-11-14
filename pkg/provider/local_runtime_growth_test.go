package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func lrtGrowthHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestLocalRuntimeOverridesRoundtripAndClear(t *testing.T) {
	lrtGrowthHome(t)
	if got := LoadWindowOverrides(); len(got) != 0 {
		t.Fatalf("initial %v", got)
	}
	for _, kv := range []struct {
		id string
		w  int
	}{{"model-a", 98304}, {"model-b", 262144}} {
		if err := SaveWindowOverride(kv.id, kv.w); err != nil {
			t.Fatal(err)
		}
	}
	if got := LoadWindowOverrides(); !reflect.DeepEqual(got, map[string]int{"model-a": 98304, "model-b": 262144}) {
		t.Fatalf("saved %v", got)
	}
	raw, _ := os.ReadFile(WindowOverridesPath())
	if string(raw) != "{\n \"model-a\": 98304,\n \"model-b\": 262144\n}" {
		t.Fatalf("file %q", raw)
	}
	if err := ClearWindowOverride("model-a"); err != nil {
		t.Fatal(err)
	}
	if got := LoadWindowOverrides(); !reflect.DeepEqual(got, map[string]int{"model-b": 262144}) {
		t.Fatalf("cleared %v", got)
	}
	if err := ClearWindowOverride("never-existed"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalRuntimeCorruptOverridesReadAsEmpty(t *testing.T) {
	lrtGrowthHome(t)
	path := WindowOverridesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"{not json", "[1, 2]", `{"a": null}`, `{"a": NaN}`, "\xff{}"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := LoadWindowOverrides(); len(got) != 0 {
			t.Fatalf("%q read as %v", text, got)
		}
	}
	if err := os.WriteFile(path, []byte("\ufeff{\"b\": 1.9, \"a\": \"7\", \"b\": 2, \"c\": true}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadWindowOverrides(); !reflect.DeepEqual(got, map[string]int{"a": 7, "b": 2, "c": 1}) {
		t.Fatalf("coerced %v", got)
	}
	if err := SaveWindowOverride("é", 5); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{\n \"b\": 2,\n \"a\": 7,\n \"c\": 1,\n \"\\u00e9\": 5\n}" {
		t.Fatalf("rewritten %q", raw)
	}
}

func TestLocalRuntimeWindowOverridesFollowTheSharedRuntimesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	if got := WindowOverridesPath(); got != filepath.Join(root, "runtimes", "llamacpp", "window_overrides.json") {
		t.Fatalf("path %s", got)
	}
}

func TestLocalRuntimeGrowthDeclinesForeignEndpoints(t *testing.T) {
	lrtGrowthHome(t)
	grown, ok, err := MaybeGrowWindow("some-model", "http://127.0.0.1:9999/v1", 100_000, 65536, nil)
	if grown != 0 || ok || err != nil {
		t.Fatalf("grown %d %v %v", grown, ok, err)
	}
}

func TestLocalRuntimeOccupancyConfirmedSkipsGateOne(t *testing.T) {
	gib := int64(1) << 30
	layers := append(lrtLayers(LayerFull, 4096, 16), lrtLayers(LayerRecurrent, 0, 48)...)
	p := NewModelProfile("m", 2*gib, 0, 262144, layers)
	b := NewHardwareBudget(26*gib, 32*gib, 64*gib)
	if d := DecideGrowth(p, b, 98304, 78_000, nil, true, true, false); d.Action != GrowthActionHold {
		t.Fatalf("ungated %+v", d)
	}
	d := DecideGrowth(p, b, 98304, 78_000, nil, true, true, true)
	if d.Action != GrowthActionGrow || d.NextWindow <= 98304 {
		t.Fatalf("confirmed %+v", d)
	}
}

// Fake runtime whose preset grants follow the budget, recording what growth asked of it
type lrtFakeRuntime struct {
	budget       HardwareBudget
	running      *PresetEntry
	freed        []int64
	refreshes    int
	refreshGrant bool
	need         func(window int) int64
}

func lrtInstallGrowthFakes(t *testing.T, gguf string, profile ModelProfile, rt *lrtFakeRuntime) {
	saved := localGrowthDeps
	t.Cleanup(func() { localGrowthDeps = saved })
	localGrowthDeps.getSupervisor = func() *LlamaServerSupervisor { return &LlamaServerSupervisor{} }
	localGrowthDeps.isIdle = func(*LlamaServerSupervisor, string) bool { return true }
	localGrowthDeps.isManagedEndpoint = func(string) bool { return true }
	localGrowthDeps.stagedModels = func() []string { return []string{gguf} }
	localGrowthDeps.readHeader = func(string) (*GGUFHeader, error) { return &GGUFHeader{}, nil }
	localGrowthDeps.profileFromHeader = func(*GGUFHeader) (ModelProfile, error) { return profile, nil }
	localGrowthDeps.probeBudget = func(bool) (HardwareBudget, error) { return rt.budget, nil }
	localGrowthDeps.readPresets = func(string) map[string]*PresetEntry {
		return map[string]*PresetEntry{"m": rt.running}
	}
	localGrowthDeps.residentFootprint = func(_ string, _ HardwareBudget, window int, drafting bool) (int64, bool, error) {
		if drafting {
			return rt.need(window) + 1, true, nil
		}
		return rt.need(window), true, nil
	}
	localGrowthDeps.launchBudget = func(b HardwareBudget, own int64) *HardwareBudget {
		rt.freed = append(rt.freed, own)
		return &b
	}
	localGrowthDeps.presetForModel = func(_ string, b HardwareBudget, _ map[string]bool, requested int, live *HardwareBudget, _ int, _ string) (*PresetEntry, error) {
		window := ContextFloor
		if requested != 0 && rt.need(requested) <= live.UsableVRAMBytes {
			window = requested
		}
		return &PresetEntry{ModelID: "m", Window: window}, nil
	}
	localGrowthDeps.refreshLocalServer = func() bool {
		rt.refreshes++
		if rt.refreshGrant {
			if w, ok := LoadWindowOverrides()["m"]; ok {
				rt.running = &PresetEntry{ModelID: "m", Window: w}
			}
		}
		return true
	}
}

func TestLocalRuntimeGrowthRequiresAnAdmissibleMaterializedPreset(t *testing.T) {
	lrtGrowthHome(t)
	profile := NewModelProfile("m", 2*lrtGiB, 0, 131072, lrtLayers(LayerFull, 512, 4))
	next := ContextFloor * 3 / 2
	need := func(window int) int64 { return FootprintBytes(profile, window, true, RuntimeOverheadBytes) }
	rt := &lrtFakeRuntime{budget: NewHardwareBudget(need(ContextFloor), need(ContextFloor), 0),
		running: &PresetEntry{ModelID: "m", Window: ContextFloor, Keys: map[string]string{"spec-type": "draft-dflash"}},
		need:    need, refreshGrant: true}
	rt.budget.UMA = true
	lrtInstallGrowthFakes(t, filepath.Join(t.TempDir(), "m.gguf"), profile, rt)

	if w, ok, err := MaybeGrowWindow("m", "http://127.0.0.1:1/v1", ContextFloor, ContextFloor, nil); ok || err != nil || w != 0 {
		t.Fatalf("grew on a full card %d %v %v", w, ok, err)
	}
	if rt.refreshes != 0 || len(LoadWindowOverrides()) != 0 {
		t.Fatal("a refused rung touched the server or the overrides")
	}
	if !reflect.DeepEqual(rt.freed, []int64{need(ContextFloor) + 1}) {
		t.Fatalf("freed %v", rt.freed)
	}
	rt.budget.UsableVRAMBytes, rt.budget.TotalDeviceBytes = need(next), need(next)
	if w, ok, err := MaybeGrowWindow("m", "http://127.0.0.1:1/v1", ContextFloor, ContextFloor, nil); !ok || err != nil || w != next {
		t.Fatalf("grow %d %v %v", w, ok, err)
	}
	if LoadWindowOverrides()["m"] != next || rt.running.Window != next {
		t.Fatal("grant not recorded")
	}
	rt.refreshGrant = false
	rt.budget.UsableVRAMBytes, rt.budget.TotalDeviceBytes = 64*lrtGiB, 64*lrtGiB
	if w, ok, err := MaybeGrowWindow("m", "http://127.0.0.1:1/v1", next, next, nil); ok || err != nil || w != 0 {
		t.Fatalf("unmaterialized grant reported %d %v %v", w, ok, err)
	}
}

func TestLocalRuntimeGrowthSkipsUnreadableHeadersAndRaisesOthers(t *testing.T) {
	lrtGrowthHome(t)
	profile := NewModelProfile("m", 2*lrtGiB, 0, 131072, lrtLayers(LayerFull, 512, 4))
	rt := &lrtFakeRuntime{budget: NewHardwareBudget(64*lrtGiB, 64*lrtGiB, 0), need: func(int) int64 { return 0 }}
	lrtInstallGrowthFakes(t, filepath.Join(t.TempDir(), "m-00001-of-00002.gguf"), profile, rt)
	localGrowthDeps.readHeader = func(string) (*GGUFHeader, error) {
		return nil, &GGUFError{"ValueError", "truncated GGUF header"}
	}
	if _, ok, err := MaybeGrowWindow("m", "", ContextFloor, ContextFloor, nil); ok || err != nil {
		t.Fatalf("value error %v %v", ok, err)
	}
	localGrowthDeps.readHeader = func(string) (*GGUFHeader, error) { return nil, &GGUFError{"KeyError", "13"} }
	if _, ok, err := MaybeGrowWindow("m", "", ContextFloor, ContextFloor, nil); ok || err == nil {
		t.Fatalf("key error %v %v", ok, err)
	}
	if _, ok, err := MaybeGrowWindow("other", "", ContextFloor, ContextFloor, nil); ok || err != nil {
		t.Fatalf("unstaged model %v %v", ok, err)
	}
}

func TestLocalRuntimeGrowthPropagatesProbeAndPresetErrors(t *testing.T) {
	lrtGrowthHome(t)
	profile := NewModelProfile("m", 2*lrtGiB, 0, 131072, lrtLayers(LayerFull, 512, 4))
	need := func(window int) int64 { return FootprintBytes(profile, window, true, RuntimeOverheadBytes) }
	rt := &lrtFakeRuntime{budget: NewHardwareBudget(64*lrtGiB, 64*lrtGiB, 0), need: need, refreshGrant: true,
		running: &PresetEntry{ModelID: "m", Window: ContextFloor}}
	rt.budget.UMA = true
	lrtInstallGrowthFakes(t, filepath.Join(t.TempDir(), "m.gguf"), profile, rt)
	probeErr := &HardwareProbeError{Class: "TimeoutExpired", Msg: "getconf timed out"}
	localGrowthDeps.probeBudget = func(bool) (HardwareBudget, error) { return HardwareBudget{}, probeErr }
	if _, ok, err := MaybeGrowWindow("m", "", ContextFloor, ContextFloor, nil); ok || err != probeErr {
		t.Fatalf("probe error %v %v", ok, err)
	}
	localGrowthDeps.probeBudget = func(bool) (HardwareBudget, error) { return rt.budget, nil }
	presetErr := &GGUFError{"KeyError", "general.architecture"}
	localGrowthDeps.presetForModel = func(string, HardwareBudget, map[string]bool, int, *HardwareBudget, int, string) (*PresetEntry, error) {
		return nil, presetErr
	}
	if _, ok, err := MaybeGrowWindow("m", "", ContextFloor, ContextFloor, nil); ok || err != presetErr {
		t.Fatalf("preset error %v %v", ok, err)
	}
	if rt.refreshes != 0 || len(LoadWindowOverrides()) != 0 {
		t.Fatal("a raising preset pass touched the server or the overrides")
	}
}
