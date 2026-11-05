package provider

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

const lr6Native = 262144

var lr6Build = QuantVariant{Quant: "Q4_K_M", Files: []AssetFile{{Path: "Hybrid-Q4_K_M.gguf", SizeBytes: 16 * lr6GiB}}}

func lr6HybridEntry() CatalogEntry {
	return CatalogEntry{ID: "hybrid", DisplayName: "Hybrid", Repo: "org/Hybrid-GGUF", Variants: []QuantVariant{lr6Build},
		NCtxTrain: lr6Native, FullLayers: 16, RecurrentLayers: 48, PerLayerF16: 4096, MTP: true, NVocab: 248320, MTPDraftDepth: 2,
		Draft:         &AssetFile{Path: "Hybrid-DFlash.gguf", SizeBytes: lr6GiB, Repo: "lab/Hybrid-DFlash-GGUF", Revision: "0123abc"},
		DraftSpecType: "draft-dflash", DraftDepth: 7, DraftMinEngine: "b11370"}
}

// Hybrid model whose catalog entry and recurrent state the test may swap, returning its path and draft path
type lr6DraftModel struct {
	gguf, draft string
	entry       CatalogEntry
	state       int64
}

func lr6Model(t *testing.T) *lr6DraftModel {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
	m := &lr6DraftModel{gguf: filepath.Join(dir, lr6Build.ModelID()+".gguf"), entry: lr6HybridEntry(), state: 150 * lr6MiB}
	lr6FakeHeaders(t, func(string) ModelProfile {
		p := m.entry.Profile(lr6Build)
		p.KVScale, p.RecurrentStateBytes = 1.0, m.state
		return p
	})
	presetEntryForModel = func(modelID string) *CatalogEntry {
		if modelID == lr6Build.ModelID() {
			e := m.entry
			return &e
		}
		return nil
	}
	m.draft = filepath.Join(localAssetsDir(), m.entry.Draft.LocalName())
	lr6Touch(t, m.draft)
	return m
}

func lr6Roomy() HardwareBudget {
	total := 128 * lr6GiB
	b := NewHardwareBudget(int64(float64(total)*0.8), total, 0)
	b.UMA = true
	return b
}

func lr6Discrete(gib float64) HardwareBudget {
	return NewHardwareBudget(int64(gib*float64(lr6GiB)), int64(gib*float64(lr6GiB)), 64*lr6GiB)
}

func lr6Drafts(p *PresetEntry) bool { return p.Keys["spec-type"] == "draft-dflash" }

func lr6Footprint(t *testing.T, gguf string, budget HardwareBudget, window int, drafting bool) int64 {
	t.Helper()
	n, ok, err := residentFootprint(gguf, budget, window, drafting)
	if !ok || err != nil {
		t.Fatalf("footprint %v %v", ok, err)
	}
	return n
}

func lr6LaunchFootprint(t *testing.T, gguf string, budget HardwareBudget, tag string) int64 {
	t.Helper()
	n, ok, err := presetLaunchFootprint(gguf, budget, tag)
	if !ok || err != nil {
		t.Fatalf("launch footprint %v %v", ok, err)
	}
	return n
}

func TestLocalRuntimeTheDraftReplacesMTPOnOneSlotWhenItFits(t *testing.T) {
	m := lr6Model(t)
	p := lr6Preset(t, m.gguf, lr6Roomy(), 0, nil, "b11671")
	if p.Window != lr6Native || p.Spilled || p.Keys["spec-type"] != "draft-dflash" || p.Keys["spec-draft-n-max"] != "7" ||
		p.Keys["model-draft"] != m.draft || p.Keys["parallel"] != "1" {
		t.Fatalf("%+v", p)
	}
}

func TestLocalRuntimeAMissingDraftOrAnOlderEngineKeepsMTP(t *testing.T) {
	m := lr6Model(t)
	for _, tag := range []string{"", "b11000"} {
		p := lr6Preset(t, m.gguf, lr6Roomy(), 0, nil, tag)
		if _, parallel := p.Keys["parallel"]; p.Keys["spec-type"] != "draft-mtp" || parallel {
			t.Fatalf("%q %+v", tag, p)
		}
	}
	_ = os.Remove(m.draft)
	p := lr6Preset(t, m.gguf, lr6Roomy(), 0, nil, "b11671")
	if _, drafted := p.Keys["model-draft"]; p.Keys["spec-type"] != "draft-mtp" || drafted {
		t.Fatalf("%+v", p)
	}
}

func TestLocalRuntimeTheDraftMayShrinkTheWindowToHalfNativeAndNoFurther(t *testing.T) {
	m := lr6Model(t)
	shrank, declinedWithRoom := 0, 0
	for tenths := 160; tenths < 400; tenths += 2 {
		budget := lr6Discrete(float64(tenths) / 10)
		mtp := lr6Preset(t, m.gguf, budget, 0, nil, "")
		p := lr6Preset(t, m.gguf, budget, 0, nil, "b11671")
		if mtp.Refusal != "" {
			continue
		}
		if lr6Drafts(p) {
			if p.Spilled || p.Window < min(mtp.Window, lr6Native/2) || p.Keys["parallel"] != "1" {
				t.Fatalf("%d %+v", tenths, p)
			}
			if p.Window < mtp.Window {
				shrank++
			}
		} else {
			if !lr6PresetEq(p, mtp) {
				t.Fatalf("%d %+v %+v", tenths, p, mtp)
			}
			if !mtp.Spilled {
				declinedWithRoom++
			}
		}
	}
	if shrank == 0 || declinedWithRoom == 0 {
		t.Fatal(shrank, declinedWithRoom)
	}
}

func TestLocalRuntimeTheDraftRunsOnlyWhereItFitsBesideOtherPrograms(t *testing.T) {
	m := lr6Model(t)
	capacity := lr6Discrete(32)
	busy, drafted := 0, 0
	for quarters := 56; quarters < 120; quarters++ {
		live := capacity
		live.UsableVRAMBytes = int64(float64(quarters) / 4 * float64(lr6GiB))
		p := lr6Preset(t, m.gguf, capacity, 0, &live, "b11671")
		alone := lr6Preset(t, m.gguf, live, 0, nil, "b11671")
		if alone.Spilled {
			busy++
		}
		if lr6Drafts(p) {
			drafted++
			if !lr6Drafts(alone) || alone.Spilled {
				t.Fatal(float64(quarters) / 4)
			}
		}
	}
	if busy == 0 || drafted == 0 {
		t.Fatal(busy, drafted)
	}
}

func TestLocalRuntimeOnWindowsUnifiedMemoryTheDraftKeepsTheSmallestPromptCache(t *testing.T) {
	m := lr6Model(t)
	capacity := NewHardwareBudget(48*lr6GiB, 48*lr6GiB, 64*lr6GiB)
	capacity.UMA = true
	drafted := 0
	for quarters := 64; quarters < 160; quarters++ {
		room := int64(float64(quarters) / 4 * float64(lr6GiB))
		live := capacity
		live.UsableVRAMBytes = min(capacity.UsableVRAMBytes, int64(float64(room)*0.8))
		live.CommitRoomBytes = &room
		p := lr6Preset(t, m.gguf, capacity, 0, &live, "b11671")
		if lr6Drafts(p) {
			drafted++
			cache, err := strconv.ParseInt(p.Keys["cache-ram"], 10, 64)
			if err != nil || cache<<20 < hwPromptCacheMin {
				t.Fatal(float64(quarters)/4, p.Keys["cache-ram"])
			}
		}
	}
	if drafted == 0 {
		t.Fatal("never drafted")
	}
}

func TestLocalRuntimeAGrownWindowWinsOverTheDraft(t *testing.T) {
	m := lr6Model(t)
	var live *HardwareBudget
	for tenths := 160; tenths < 400 && live == nil; tenths += 2 {
		b := lr6Discrete(float64(tenths) / 10)
		if p := lr6Preset(t, m.gguf, b, 0, nil, "b11671"); lr6Drafts(p) && p.Window < lr6Preset(t, m.gguf, b, 0, nil, "").Window {
			live = &b
		}
	}
	if live == nil {
		t.Fatal("no budget where the draft shrinks the window")
	}
	mtp := lr6Preset(t, m.gguf, lr6Roomy(), lr6Native, live, "")
	grown := lr6Preset(t, m.gguf, lr6Roomy(), lr6Native, live, "b11671")
	if !lr6PresetEq(grown, mtp) || grown.Keys["spec-type"] != "draft-mtp" {
		t.Fatalf("%+v %+v", grown, mtp)
	}
}

func TestLocalRuntimeEachDraftPositionPricesACopyOfTheRecurrentState(t *testing.T) {
	m := lr6Model(t)
	budget := lr6Discrete(32)
	if !lr6Drafts(lr6Preset(t, m.gguf, budget, 0, nil, "b11671")) {
		t.Fatal("small state must draft")
	}
	m.state = lr6GiB
	if lr6Drafts(lr6Preset(t, m.gguf, budget, 0, nil, "b11671")) {
		t.Fatal("eight copies of a large state must not draft")
	}
}

func TestLocalRuntimeGrowthCountsTheOutgoingDraftAsFreedMemory(t *testing.T) {
	m := lr6Model(t)
	budget := lr6Discrete(28)
	running := lr6Preset(t, m.gguf, budget, 0, nil, "b11671")
	if !lr6Drafts(running) || running.Window >= lr6Native {
		t.Fatalf("%+v", running)
	}
	saved := localGrowthDeps
	t.Cleanup(func() { localGrowthDeps = saved })
	localGrowthDeps.stagedModels = func() []string { return []string{m.gguf} }
	localGrowthDeps.getSupervisor = func() *LlamaServerSupervisor { return &LlamaServerSupervisor{} }
	localGrowthDeps.isIdle = func(*LlamaServerSupervisor, string) bool { return true }
	localGrowthDeps.isManagedEndpoint = func(string) bool { return true }
	localGrowthDeps.readHeader = presetReadHeader
	localGrowthDeps.profileFromHeader = presetProfileFromGGUF
	localGrowthDeps.probeBudget = func(bool) (HardwareBudget, error) { return budget, nil }
	localGrowthDeps.readPresets = func(string) map[string]*PresetEntry { return map[string]*PresetEntry{lr6Build.ModelID(): running} }
	var freed []int64
	localGrowthDeps.launchBudget = func(b HardwareBudget, own int64) *HardwareBudget { freed = append(freed, own); return &b }
	localGrowthDeps.refreshLocalServer = func() bool { return false }
	if _, _, err := MaybeGrowWindow(lr6Build.ModelID(), "http://127.0.0.1:1/v1", running.Window, running.Window, nil); err != nil {
		t.Fatal(err)
	}
	drafting := lr6Footprint(t, m.gguf, budget, running.Window, true)
	if len(freed) != 1 || freed[0] != drafting || drafting <= lr6Footprint(t, m.gguf, budget, running.Window, false) {
		t.Fatal(freed, drafting)
	}
}

func TestLocalRuntimeASpilledModelNeverTakesTheDraft(t *testing.T) {
	m := lr6Model(t)
	if p := lr6Preset(t, m.gguf, lr6Discrete(12), 0, nil, "b11671"); !p.Spilled || lr6Drafts(p) {
		t.Fatalf("%+v", p)
	}
}

func TestLocalRuntimeAModelWithoutMTPDraftsWithoutBackendSampling(t *testing.T) {
	m := lr6Model(t)
	m.entry.MTP, m.entry.DraftMinEngine = false, "b11671"
	p := lr6Preset(t, m.gguf, lr6Roomy(), 0, nil, "b11671")
	if _, sampling := p.Keys["backend-sampling"]; !lr6Drafts(p) || p.Keys["parallel"] != "1" || sampling {
		t.Fatalf("%+v", p)
	}
}

func TestLocalRuntimeTheResidencyPriceIsTheLaunchTheModelRuns(t *testing.T) {
	m := lr6Model(t)
	larger := map[string]bool{}
	for _, state := range []int64{150 * lr6MiB, 600 * lr6MiB} {
		m.state = state
		mtp := lr6Footprint(t, m.gguf, lr6Roomy(), lr6Native, false)
		drafting := lr6Footprint(t, m.gguf, lr6Roomy(), lr6Native, true)
		if drafting > mtp {
			larger["draft"] = true
		} else {
			larger["mtp"] = true
		}
		if got := lr6LaunchFootprint(t, m.gguf, lr6Roomy(), "b11671"); got != max(mtp, drafting) {
			t.Fatal(state, got, mtp, drafting)
		}
		if lr6LaunchFootprint(t, m.gguf, lr6Roomy(), "b11000") != mtp || lr6LaunchFootprint(t, m.gguf, lr6Roomy(), "") != mtp {
			t.Fatal("an engine that cannot load the draft prices MTP alone")
		}
	}
	if !larger["draft"] || !larger["mtp"] {
		t.Fatal(larger)
	}
}

func TestLocalRuntimeTwoModelsStayResidentWhereTwoDraftedLaunchesFit(t *testing.T) {
	m := lr6Model(t)
	lr6Touch(t, m.gguf)
	held := max(lr6Footprint(t, m.gguf, lr6Roomy(), lr6Native, false), lr6Footprint(t, m.gguf, lr6Roomy(), lr6Native, true))
	card := lr6Roomy()
	card.UsableVRAMBytes = 2*held + lr6GiB
	if got, err := AdmittedResidencyCount(filepath.Dir(m.gguf), card, 4, nil, "b11671"); got != 2 || err != nil {
		t.Fatal(got, err)
	}
}
