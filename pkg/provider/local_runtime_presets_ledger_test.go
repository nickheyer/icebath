package provider

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const lr7GiB = int64(1) << 30

// Writes a GGUF magic followed by zero bytes as a staged model file
func lr7StageFake(t *testing.T, mdir, name string) string {
	t.Helper()
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mdir, name+".gguf")
	if err := os.WriteFile(path, append([]byte("GGUF"), make([]byte, 64)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func lr7TinyProfile(modelID string) ModelProfile {
	return NewModelProfile(modelID, 2*lr7GiB, 0, 131072, lrtLayers(LayerFull, 512, 4))
}

func lr7Generate(t *testing.T, mdir string, budget HardwareBudget, presetPath string, mtp map[string]bool) []*PresetEntry {
	t.Helper()
	entries, err := GeneratePresets(mdir, budget, presetPath, mtp, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func lr7Footprint(t *testing.T, gguf string, budget HardwareBudget, window int) int64 {
	t.Helper()
	need, ok, err := residentFootprint(gguf, budget, window, false)
	if err != nil || !ok {
		t.Fatalf("footprint %v %v", ok, err)
	}
	return need
}

func TestLocalRuntimeResidencyCapPricesASplitAgainstTheCard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
	mdir := filepath.Join(dir, "models")
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	lrtWriteGGUF(t, filepath.Join(mdir, "giant-00001-of-00002.gguf"), lrtSplitArch, nil)
	lrtWriteGGUF(t, filepath.Join(mdir, "giant-00002-of-00002.gguf"), lrtSplitKeys(1, 2),
		[]lrtTensor{{"blk.0.attn_q.weight", 48 << 30 / 4}})
	lrtWriteGGUF(t, filepath.Join(mdir, "utility.gguf"), lrtSplitArch, []lrtTensor{{"blk.0.attn_q.weight", 10 << 30 / 4}})
	budget := NewHardwareBudget(64*lr7GiB, 64*lr7GiB, 64*lr7GiB)
	if got, err := AdmittedResidencyCount(mdir, budget, 4, nil, ""); got != 1 || err != nil {
		t.Fatalf("admitted %d %v", got, err)
	}
}

func TestLocalRuntimeACatalogBuildIsPricedWithoutItsLazyTables(t *testing.T) {
	build := QuantVariant{Quant: "UD-IQ4_XS", LazyBytes: 30 * lr7GiB, Files: []AssetFile{
		{Path: "m-00001-of-00002.gguf", SizeBytes: 10 << 20}, {Path: "m-00002-of-00002.gguf", SizeBytes: 90 * lr7GiB}}}
	if build.SizeBytes() != 90*lr7GiB+(10<<20) || build.WeightsBytes() != build.SizeBytes()-30*lr7GiB {
		t.Fatalf("size %d weights %d", build.SizeBytes(), build.WeightsBytes())
	}
}

func TestLocalRuntimeThePresetReadsTheTableOnDemandWhereverItPricesItThatWay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	gguf := filepath.Join(dir, "flash.gguf")
	lrtWriteGGUF(t, gguf, lrtLazyArch("qwen4exp"), []lrtTensor{{"per_layer_token_embd.weight", lrtOver4GiB}, {"blk.0.attn_q.weight", 16 << 20}})
	unified := func(lazy bool) HardwareBudget {
		return HardwareBudget{UsableVRAMBytes: 64 * lr7GiB, TotalDeviceBytes: 80 * lr7GiB, UMA: true, LazyReads: lazy}
	}
	lazy, eager := lr6Preset(t, gguf, unified(true), 0, nil, ""), lr6Preset(t, gguf, unified(false), 0, nil, "")
	if _, ok := eager.Keys["lazy-mode"]; lazy.Keys["lazy-mode"] != "on" || ok {
		t.Fatalf("lazy %v eager %v", lazy.Keys, eager.Keys)
	}
	window := min(lazy.Window, eager.Window)
	if d := lr7Footprint(t, gguf, unified(false), window) - lr7Footprint(t, gguf, unified(true), window); d != lrtOver4GiB*4 {
		t.Fatalf("difference %d", d)
	}
}

func TestLocalRuntimePresetGenerationForCatalogModelWithMmproj(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	i := slices.IndexFunc(Catalog(), func(e CatalogEntry) bool { return e.MMProj != nil })
	variant := Catalog()[i].Variants[len(Catalog()[i].Variants)-1]
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, variant.ModelID())
	lr6FakeHeaders(t, func(string) ModelProfile { return lr7TinyProfile(variant.ModelID()) })
	entries := lr7Generate(t, mdir, NewHardwareBudget(24*lr7GiB, 24*lr7GiB, 64*lr7GiB), filepath.Join(dir, "p.ini"), nil)
	if len(entries) != 1 || entries[0].Refusal != "" || entries[0].Window <= 0 {
		t.Fatalf("entries %+v", entries)
	}
}

func TestLocalRuntimePresetRestoresGrownWindowCappedAtNative(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, "tiny-dense")
	lr6FakeHeaders(t, func(string) ModelProfile { return lr7TinyProfile("tiny-dense") })
	budget := NewHardwareBudget(24*lr7GiB, 24*lr7GiB, 64*lr7GiB)
	preset := filepath.Join(dir, "presets.ini")
	if w := lr7Generate(t, mdir, budget, preset, nil)[0].Window; w != 131072 {
		t.Fatalf("baseline %d", w)
	}
	if err := SaveWindowOverride("tiny-dense", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if w := lr7Generate(t, mdir, budget, preset, nil)[0].Window; w != 131072 {
		t.Fatalf("capped %d", w)
	}
}

func TestLocalRuntimePresetIgnoresOverrideBelowLaunchWindow(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, "tiny-dense")
	lr6FakeHeaders(t, func(string) ModelProfile { return lr7TinyProfile("tiny-dense") })
	if err := SaveWindowOverride("tiny-dense", 65536); err != nil {
		t.Fatal(err)
	}
	budget := NewHardwareBudget(24*lr7GiB, 24*lr7GiB, 64*lr7GiB)
	if w := lr7Generate(t, mdir, budget, filepath.Join(dir, "p.ini"), nil)[0].Window; w != 131072 {
		t.Fatalf("window %d", w)
	}
}

func TestLocalRuntimePresetRestoresGrownWindowMidladder(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	profile := NewModelProfile("big-dense", 20*lr7GiB, 0, 262144, lrtLayers(LayerFull, 4096, 60))
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, "big-dense")
	lr6FakeHeaders(t, func(string) ModelProfile { return profile })
	budget := NewHardwareBudget(28*lr7GiB, 32*lr7GiB, 128*lr7GiB)
	baseline := lr7Generate(t, mdir, budget, filepath.Join(dir, "a.ini"), nil)[0]
	if baseline.Window >= 262144 {
		t.Fatalf("sanity: launch below native %d", baseline.Window)
	}
	grown := baseline.Window * 2
	if err := SaveWindowOverride("big-dense", grown); err != nil {
		t.Fatal(err)
	}
	if restored := lr7Generate(t, mdir, budget, filepath.Join(dir, "b.ini"), nil)[0]; restored.Window < grown {
		t.Fatalf("override must lift the launch window %d < %d", restored.Window, grown)
	}
}

func TestLocalRuntimeMTPPlanMatchesCostThroughGeneratedPresets(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	profile := NewModelProfile("mtp-fit", 16*lr7GiB, 0, 262144, lrtLayers(LayerFull, 4096, 32))
	profile.MoE, profile.NVocab = true, 151936
	priced := profile
	priced.KVScale = 1.2
	lean := RuntimeOverheadBytes + UbLogitsBytes(profile.NVocab, true, false)
	stacked := RuntimeOverheadBytes + UbLogitsBytes(profile.NVocab, true, true)
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, profile.Name)
	lr6FakeHeaders(t, func(string) ModelProfile { return profile })
	generate := func(device, ram int64, override int) *PresetEntry {
		t.Helper()
		if err := SaveWindowOverride(profile.Name, override); err != nil {
			t.Fatal(err)
		}
		return lr7Generate(t, mdir, NewHardwareBudget(device, device, ram), filepath.Join(dir, "presets.ini"),
			map[string]bool{profile.Name: true})[0]
	}
	has := func(e *PresetEntry, key string) bool { _, ok := e.Keys[key]; return ok }
	floorNeed := profile.WeightsBytes + CtxBytes(priced, ContextFloor, true)
	initial := generate(floorNeed+lean, 8*lr7GiB, 0)
	if initial.Window != ContextFloor || initial.Spilled || has(initial, "ubatch-size") || initial.Keys["spec-type"] != "draft-mtp" {
		t.Fatalf("initial %+v", initial)
	}
	for _, override := range []int{0, ContextFloor, 73728} {
		if boot := generate(16*lr7GiB, 64*lr7GiB, override); !boot.Spilled || has(boot, "ubatch-size") {
			t.Fatalf("spilled boot %d %+v", override, boot)
		}
	}
	device := floorNeed + stacked
	control := generate(device, 8*lr7GiB, 0)
	if control.Keys["ubatch-size"] != "2048" {
		t.Fatalf("control %+v", control)
	}
	for _, ram := range []int64{8 * lr7GiB, 0} {
		grown := generate(device, ram, 73728)
		if grown.Window != 73728 || grown.Spilled || has(grown, "ubatch-size") || has(grown, "override-tensor") ||
			grown.Keys["spec-type"] != "draft-mtp" || profile.WeightsBytes+CtxBytes(priced, grown.Window, true)+lean > device {
			t.Fatalf("grown ram %d %+v", ram, grown)
		}
	}
	both := generate(device, 64*lr7GiB, 147456)
	if both.Window != 147456 || !both.Spilled || both.Keys["ubatch-size"] != "2048" || !has(both, "override-tensor") {
		t.Fatalf("both spill %+v", both)
	}
	if smaller := generate(floorNeed+lean, 0, 73728); smaller.Window != ContextFloor || smaller.Spilled {
		t.Fatalf("smaller boot %+v", smaller)
	}
	if profile.WeightsBytes+CtxBytes(priced, control.Window, true)+stacked > device {
		t.Fatal("control over the device")
	}
}

func TestLocalRuntimeGrowthRequiresAnAdmissibleMaterializedPresetFromTheCatalog(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	i := slices.IndexFunc(Catalog(), func(e CatalogEntry) bool { return e.MTP && e.MMProj != nil })
	entry := Catalog()[i]
	modelID := entry.Variants[len(entry.Variants)-1].ModelID()
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, modelID)
	profile := entry.Profile(entry.Variants[len(entry.Variants)-1])
	profile.KVScale = 1.0
	saved := localGrowthDeps
	t.Cleanup(func() { localGrowthDeps = saved })
	localGrowthDeps.stagedModels = func() []string { m, _ := filepath.Glob(filepath.Join(mdir, "*.gguf")); return m }
	localGrowthDeps.getSupervisor = func() *LlamaServerSupervisor { return &LlamaServerSupervisor{} }
	localGrowthDeps.isIdle = func(*LlamaServerSupervisor, string) bool { return true }
	localGrowthDeps.isManagedEndpoint = func(string) bool { return true }
	localGrowthDeps.readHeader = func(string) (*GGUFHeader, error) { return &GGUFHeader{}, nil }
	localGrowthDeps.profileFromHeader = func(*GGUFHeader) (ModelProfile, error) { return profile, nil }
	lr6FakeHeaders(t, func(string) ModelProfile { return profile })
	asset := filepath.Join(localAssetsDir(), entry.MMProj.LocalName())
	lr6Touch(t, asset)
	overhead := RuntimeOverheadBytes + entry.MMProj.SizeBytes + UbLogitsBytes(profile.NVocab, true, false)
	priced := profile
	priced.KVScale = 1.2
	next := ContextFloor * 3 / 2
	floorNeed := profile.WeightsBytes + CtxBytes(priced, ContextFloor, true) + overhead
	nextNeed := profile.WeightsBytes + CtxBytes(priced, next, true) + overhead
	budget := NewHardwareBudget(floorNeed, floorNeed, 0)
	budget.UMA = true
	localGrowthDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
	presetPath := filepath.Join(RuntimesRoot(), "presets.ini")
	calls := 0
	localGrowthDeps.refreshLocalServer = func() bool {
		calls++
		if _, err := GeneratePresets(mdir, budget, presetPath, nil, nil, 1, ""); err != nil {
			t.Fatal(err)
		}
		return true
	}
	grow := func(current int) (int, bool) {
		t.Helper()
		w, ok, err := MaybeGrowWindow(modelID, "http://127.0.0.1:1/v1", ContextFloor, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		return w, ok
	}
	if _, ok := grow(ContextFloor); ok || calls != 0 || len(LoadWindowOverrides()) != 0 {
		t.Fatalf("a refused rung grew or touched the server %v %d", ok, calls)
	}
	budget.UsableVRAMBytes, budget.TotalDeviceBytes = nextNeed, nextNeed
	if w, ok := grow(ContextFloor); !ok || w != next {
		t.Fatalf("grow %d %v", w, ok)
	}
	if d := readPresetDecisions(presetPath)[modelID]; d == nil || d.Window != next || LoadWindowOverrides()[modelID] != next {
		t.Fatalf("decision %+v overrides %v", d, LoadWindowOverrides())
	}
	localGrowthDeps.refreshLocalServer = func() bool { return true }
	budget.UsableVRAMBytes, budget.TotalDeviceBytes = 64*lr7GiB, 64*lr7GiB
	if w, ok := grow(next); ok {
		t.Fatalf("unmaterialized grant reported %d", w)
	}
}

func TestLocalRuntimeSamplingLadderFileBeatsCatalogBeatsNothing(t *testing.T) {
	lrtGrowthHome(t)
	dir := t.TempDir()
	i := slices.IndexFunc(Catalog(), func(e CatalogEntry) bool { return len(e.Sampling) > 0 })
	entry := Catalog()[i]
	variant := entry.Variants[len(entry.Variants)-1]
	mdir := filepath.Join(dir, "models")
	lr7StageFake(t, mdir, variant.ModelID())
	lr7StageFake(t, mdir, "off-catalog-model")
	lr6FakeHeaders(t, func(string) ModelProfile { return lr7TinyProfile("x") })
	presetReadHeader = func(p string) (*GGUFHeader, error) {
		if strings.Contains(p, variant.ModelID()) {
			return &GGUFHeader{Path: p, Metadata: map[string]any{"general.sampling.temp": 0.42}}, nil
		}
		return &GGUFHeader{Path: p}, nil
	}
	out := filepath.Join(dir, "presets.ini")
	lr7Generate(t, mdir, NewHardwareBudget(64*lr7GiB, 64*lr7GiB, 64*lr7GiB), out, nil)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	ini, err := parseAWSINI(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	sec := ini.flat[variant.ModelID()]
	if sec["temp"] != "0.42" {
		t.Fatalf("file's own sampling must win per key %v", sec)
	}
	for _, s := range entry.Sampling {
		if s.Key != "temp" && sec[s.Key] != pyStr(s.Value) {
			t.Fatalf("catalog must fill the silent key %s %v", s.Key, sec)
		}
	}
	if _, ok := sec["ctx-size"]; !ok {
		t.Fatal("policy keys survive the ladder")
	}
	off := ini.flat["off-catalog-model"]
	if _, temp := off["temp"]; temp || off["top-p"] != "" {
		t.Fatalf("no file keys and no catalog entry keep llama.cpp defaults %v", off)
	}
}

func TestLocalRuntimeReadPresetDecisionsDefaultsToTheSharedRuntimesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profile)
	shared := filepath.Join(root, "runtimes", "llamacpp")
	lr5Touch(t, filepath.Join(shared, "presets.ini"), "[m1]\nctx-size = 65536\n")
	if _, ok := readPresetDecisions("")["m1"]; !ok {
		t.Fatalf("decisions %v", readPresetDecisions(""))
	}
}

func lr7Spark() HardwareBudget {
	g := lr2F(lr2GiB)
	return HardwareBudget{UsableVRAMBytes: int64(121.7 * g * 0.8), TotalDeviceBytes: int64(121.7 * g), UMA: true, LazyReads: true}
}

func TestLocalRuntimeFlashNextOnA128GBSparkRuns256KAtTheLeanMicrobatch(t *testing.T) {
	mib := lr2F(1 << 20)
	measured := (61222.13 + 644.14 + 3291.18 + 644.14 + 865.5 + 248.1 + 3264 + 816 + 512 + 128 +
		5111.55 + 798.45 + 5053.94 + 808.15) * mib
	entry := CatalogByID()["qwen3.8-flash-next"]
	plan := entry.LaunchPlan(entry.Variants[0], lr7Spark())
	decision, ok := plan.Decision.(*WindowDecision)
	if !ok {
		t.Fatalf("decision %#v", plan.Decision)
	}
	args := LaunchArgs(*plan.Profile, decision, true, true, 3, true, plan.MTPPrefill)
	priced := lr2F(FootprintBytes(*plan.Profile, decision.Window, true, plan.OverheadBytes))
	if decision.Window != 262144 || decision.Spilled() || slices.Contains(args, "-ub") {
		t.Fatalf("window %d args %v", decision.Window, args)
	}
	if priced < measured || priced > measured*1.1 {
		t.Fatalf("priced %.0f measured %.0f", priced, measured)
	}
}

func TestLocalRuntimeThePrefillMicrobatchPaysFourTimesTheWindowComputeOfTheLeanOne(t *testing.T) {
	entry := CatalogByID()["qwen3.8-flash-next"]
	profile := entry.Profile(entry.Variants[0])
	lean, stacked := PostureProfile(profile, true, false), PostureProfile(profile, true, true)
	if stacked.WindowComputePerToken != 4*lean.WindowComputePerToken || lean.WindowComputePerToken != 46*512*2 {
		t.Fatalf("lean %d stacked %d", lean.WindowComputePerToken, stacked.WindowComputePerToken)
	}
}

func TestLocalRuntimeRouterHoldsOnlyTheModelsFreeCommitFits(t *testing.T) {
	dir := t.TempDir()
	prevStaged, prevNeed := presetStagedIn, presetLaunchFootprintOf
	t.Cleanup(func() { presetStagedIn, presetLaunchFootprintOf = prevStaged, prevNeed })
	presetStagedIn = func(string) []string { return []string{filepath.Join(dir, "a.gguf"), filepath.Join(dir, "b.gguf")} }
	presetLaunchFootprintOf = func(string, HardwareBudget, string) (int64, bool, error) { return 12 * lr7GiB, true, nil }
	capacity := NewHardwareBudget(30*lr7GiB, 50*lr7GiB, 0)
	capacity.UMA = true
	live := NewHardwareBudget(16*lr7GiB, 50*lr7GiB, 0)
	live.UMA, live.CommitRoomBytes = true, ptrInt64(20*lr7GiB)
	if got, err := AdmittedResidencyCount(dir, capacity, 4, nil, ""); got != 2 || err != nil {
		t.Fatalf("capacity %d %v", got, err)
	}
	if got, err := AdmittedResidencyCount(dir, capacity, 4, &live, ""); got != 1 || err != nil {
		t.Fatalf("live %d %v", got, err)
	}
}

func TestLocalRuntimeGrowthCountsTheOutgoingModelAtItsCommitCharge(t *testing.T) {
	stat := lr2Spark(t)
	capacity := lr2Probe(t, true)
	*stat = lr2Stat(30.2, 61.2, 48.4-25.0)
	live, err := LaunchBudget(capacity, 20*lr2GiB)
	if err != nil || live == nil || live.CommitRoomBytes == nil ||
		!lr2Near(lr2F(*live.CommitRoomBytes), (48.4-2.718)*lr2F(lr2GiB), 1e-3) {
		t.Fatalf("live %+v %v", live, err)
	}
}

// Stages one model whose header and profile are an 8 GiB model with no layers
func lr7CommitStaged(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	gguf := filepath.Join(dir, "model.gguf")
	lr6Touch(t, gguf)
	lr6FakeHeaders(t, func(string) ModelProfile { return NewModelProfile("model", 8*lr7GiB, 0, 65536, nil) })
	return gguf
}

func lr7UMA(usable, total int64, room *int64) HardwareBudget {
	b := NewHardwareBudget(usable, total, 0)
	b.UMA, b.CommitRoomBytes = true, room
	return b
}

func TestLocalRuntimePresetWritesTheCacheSizeFromLiveCommit(t *testing.T) {
	gguf := lr7CommitStaged(t)
	capacity, live := lr7UMA(16*lr7GiB, 50*lr7GiB, nil), lr7UMA(16*lr7GiB, 50*lr7GiB, ptrInt64(20*lr7GiB))
	cache, err := strconv.Atoi(lr6Preset(t, gguf, capacity, 0, &live, "").Keys["cache-ram"])
	if err != nil || cache <= 0 || cache > 8192 {
		t.Fatalf("cache-ram %d %v", cache, err)
	}
	if _, ok := lr6Preset(t, gguf, capacity, 0, nil, "").Keys["cache-ram"]; ok {
		t.Fatal("cache-ram without a live reading")
	}
}

func TestLocalRuntimeABusyDesktopNarrowsButNeverRefuses(t *testing.T) {
	gguf := lr7CommitStaged(t)
	capacity := lr7UMA(16*lr7GiB, 50*lr7GiB, nil)
	live := lr7UMA(1*lr7GiB, 50*lr7GiB, ptrInt64(int64(1.25*lr2F(lr2GiB))))
	entry := lr6Preset(t, gguf, capacity, 0, &live, "")
	if entry.Refusal != "" || entry.Window <= 0 || entry.Keys["cache-ram"] != "0" {
		t.Fatalf("entry %+v", entry)
	}
}

func TestLocalRuntimeManagedVisionReadsTheStagedCatalogAndProjector(t *testing.T) {
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), ".icebath"))
	i := slices.IndexFunc(Catalog(), func(e CatalogEntry) bool { return e.MMProj != nil })
	entry := Catalog()[i]
	variant := entry.Variants[len(entry.Variants)-1]
	ctx := t.Context()
	if ManagedModelSupportsVision(ctx, "gpt-4o") != nil {
		t.Fatal("not ours")
	}
	lr5Touch(t, filepath.Join(localModelsDir(), variant.ModelID()+".gguf"), "GGUF"+strings.Repeat("\x00", 32))
	if got := ManagedModelSupportsVision(ctx, variant.ModelID()); got == nil || *got {
		t.Fatalf("missing projector must be blind %v", got)
	}
	lr5Touch(t, filepath.Join(localAssetsDir(), entry.MMProj.LocalName()), "GGUF mmproj")
	if got := ManagedModelSupportsVision(ctx, variant.ModelID()); got == nil || !*got {
		t.Fatalf("projector on disk %v", got)
	}
}
