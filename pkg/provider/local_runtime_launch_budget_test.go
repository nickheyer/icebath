package provider

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const lr6Model27B = "Qwen3.8-27B-UD-Q4_K_M"

func lr6CardTotal() int64 {
	gib := lr6GiB
	return int64(31.84 * float64(gib))
}

func lr6CardCapacity() HardwareBudget {
	total := lr6CardTotal()
	return NewHardwareBudget(total-max(hwMarginFloor, int64(float64(total)*hwMarginFraction)), total, 256*lr6GiB)
}

// Card with others GiB held by other programs as nvidia-smi reports it
func lr6Card(others float64, total int64) {
	hwNvidiaVRAM = lr2VRAM(total, total-int64(others*float64(lr6GiB)), "NVIDIA GeForce RTX 5090", nil)
}

// Records every boot probe and fails the test unless each one asked for the capacity budget
func lr6CapacityProbe(t *testing.T) *[]bool {
	t.Helper()
	calls := &[]bool{}
	prev, prevGrowth := bootProbeBudget, localGrowthDeps.probeBudget
	probe := func(planning bool) (HardwareBudget, error) {
		*calls = append(*calls, planning)
		return lr6CardCapacity(), nil
	}
	bootProbeBudget, localGrowthDeps.probeBudget = probe, probe
	t.Cleanup(func() {
		bootProbeBudget, localGrowthDeps.probeBudget = prev, prevGrowth
		for _, planning := range *calls {
			if !planning {
				t.Errorf("a boot path probed live free memory %v", *calls)
			}
		}
	})
	return calls
}

// Stages the 27B build with its projector and returns the home tmp dir, models dir and model path
func lr6Stage27B(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".hermes"))
	lr2Isolate(t)
	entry := CatalogByID()["qwen3.8-27b"]
	var variant QuantVariant
	for _, v := range entry.Variants {
		if v.ModelID() == lr6Model27B {
			variant = v
		}
	}
	mdir := localModelsDir()
	gguf := filepath.Join(mdir, lr6Model27B+".gguf")
	lr5Touch(t, gguf, "GGUF"+strings.Repeat("\x00", 64))
	lr6Touch(t, filepath.Join(localAssetsDir(), entry.MMProj.LocalName()))
	profile := entry.Profile(variant)
	lr6FakeHeaders(t, func(string) ModelProfile { return profile })
	return dir, mdir, gguf
}

func lr6Live(t *testing.T, capacity HardwareBudget) *HardwareBudget {
	t.Helper()
	live, err := LaunchBudget(capacity, 0)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func TestLocalRuntime27BWindowFollowsOtherPrograms(t *testing.T) {
	for _, c := range []struct {
		others float64
		k      int
	}{{0.0, 216}, {3.0, 216}, {3.6, 144}, {4.5, 144}, {5.4, 144}, {6.5, 96}, {8.5, 64}} {
		_, _, gguf := lr6Stage27B(t)
		lr6Card(c.others, lr6CardTotal())
		e := lr6Preset(t, gguf, lr6CardCapacity(), 0, lr6Live(t, lr6CardCapacity()), "")
		if _, ot := e.Keys["override-tensor"]; e.Window != c.k*1024 || e.Spilled || ot || e.Keys["ctx-size"] != strconv.Itoa(c.k*1024) ||
			!strings.HasSuffix(e.Keys["mmproj"], "mmproj-Qwen3.8-27B-BF16.gguf") {
			t.Fatalf("%v %+v", c, e)
		}
	}
}

func TestLocalRuntimeBusyCardHoldsTheFloorAndKeepsWeightsOnTheGPU(t *testing.T) {
	_, _, gguf := lr6Stage27B(t)
	lr6Card(14.0, lr6CardTotal())
	e := lr6Preset(t, gguf, lr6CardCapacity(), 0, lr6Live(t, lr6CardCapacity()), "")
	if _, ot := e.Keys["override-tensor"]; e.Window != 64*1024 || e.Spilled || ot {
		t.Fatalf("%+v", e)
	}
}

func TestLocalRuntimeACapacityPlanThatSpillsIsLeftAlone(t *testing.T) {
	_, _, gguf := lr6Stage27B(t)
	small := NewHardwareBudget(10*lr6GiB, 12*lr6GiB, 64*lr6GiB)
	lr6Card(3.0, 12*lr6GiB)
	capacityOnly := lr6Preset(t, gguf, small, 0, nil, "")
	withLive := lr6Preset(t, gguf, small, 0, lr6Live(t, small), "")
	if !capacityOnly.Spilled || !lr6PresetEq(withLive, capacityOnly) {
		t.Fatalf("%+v %+v", capacityOnly, withLive)
	}
}

func TestLocalRuntimeRecommendationsStillPriceAgainstCapacity(t *testing.T) {
	lr2Isolate(t)
	lr6Card(8.5, lr6CardTotal())
	entry := CatalogByID()["qwen3.8-27b"]
	for _, v := range entry.Variants {
		if v.ModelID() == lr6Model27B {
			if d, ok := entry.LaunchPlan(v, lr6CardCapacity()).Decision.(*WindowDecision); !ok || d.Window != 216*1024 {
				t.Fatal(d)
			}
			return
		}
	}
	t.Fatal("no 27B variant")
}

func TestLocalRuntimeBootWritesTheLaunchWindow(t *testing.T) {
	dir, mdir, _ := lr6Stage27B(t)
	lr6CapacityProbe(t)
	lr6Card(5.4, lr6CardTotal())
	path := filepath.Join(dir, "presets.ini")
	if got := generateBootPresets(mdir, path, 1, ""); got != path || readPresetDecisions(path)[lr6Model27B].Window != 144*1024 {
		t.Fatal(got, readPresetDecisions(path)[lr6Model27B])
	}
}

func TestLocalRuntimeBootKeepsCapacityWhenTheProbeFails(t *testing.T) {
	dir, mdir, _ := lr6Stage27B(t)
	lr6CapacityProbe(t)
	hwNvidiaVRAM = func() (*NvidiaGPUQuery, error) { return nil, errors.New("nvidia-smi vanished") }
	path := filepath.Join(dir, "presets.ini")
	if got := generateBootPresets(mdir, path, 1, ""); got != path || readPresetDecisions(path)[lr6Model27B].Window != 216*1024 {
		t.Fatal(got, readPresetDecisions(path)[lr6Model27B])
	}
}

func lr6GrowthFakes(t *testing.T) {
	t.Helper()
	saved := localGrowthDeps
	t.Cleanup(func() { localGrowthDeps = saved })
	localGrowthDeps.getSupervisor = func() *LlamaServerSupervisor { return &LlamaServerSupervisor{} }
	localGrowthDeps.isIdle = func(*LlamaServerSupervisor, string) bool { return true }
	localGrowthDeps.isManagedEndpoint = func(string) bool { return true }
	localGrowthDeps.readHeader = presetReadHeader
	localGrowthDeps.profileFromHeader = presetProfileFromGGUF
}

func TestLocalRuntimeGrowthCountsTheGrowingModelAsFree(t *testing.T) {
	dir, mdir, gguf := lr6Stage27B(t)
	lr6GrowthFakes(t)
	lr6CapacityProbe(t)
	own := lr6Footprint(t, gguf, lr6CardCapacity(), 144*1024, false)
	lr6Card(2.0+float64(own)/float64(lr6GiB), lr6CardTotal())
	path := filepath.Join(dir, "presets.ini")
	localGrowthDeps.refreshLocalServer = func() bool {
		_, err := GeneratePresets(mdir, lr6CardCapacity(), path, nil, nil, 1, "")
		return err == nil
	}
	localGrowthDeps.readPresets = func(string) map[string]*PresetEntry { return readPresetDecisions(path) }
	if grown, ok, err := MaybeGrowWindow(lr6Model27B, "http://127.0.0.1:1/v1", 140*1024, 144*1024, nil); grown != 216*1024 || !ok || err != nil {
		t.Fatal(grown, ok, err)
	}
}

func TestLocalRuntimeGrowthRefusesARungOtherProgramsLeaveNoRoomFor(t *testing.T) {
	_, _, gguf := lr6Stage27B(t)
	lr6GrowthFakes(t)
	lr6CapacityProbe(t)
	own := lr6Footprint(t, gguf, lr6CardCapacity(), 144*1024, false)
	lr6Card(5.4+float64(own)/float64(lr6GiB), lr6CardTotal())
	localGrowthDeps.refreshLocalServer = func() bool { t.Fatal("bounced"); return false }
	if grown, ok, err := MaybeGrowWindow(lr6Model27B, "http://127.0.0.1:1/v1", 140*1024, 144*1024, nil); grown != 0 || ok || err != nil {
		t.Fatal(grown, ok, err)
	}
	if len(LoadWindowOverrides()) != 0 {
		t.Fatal(LoadWindowOverrides())
	}
}

func TestLocalRuntimeResidencyCapPricesCapacity(t *testing.T) {
	_, mdir, _ := lr6Stage27B(t)
	calls := lr6CapacityProbe(t)
	admittedModelsMax(mdir, 4, "")
	if len(*calls) != 1 || !(*calls)[0] {
		t.Fatal(*calls)
	}
}
