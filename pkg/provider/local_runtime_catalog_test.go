package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const lr4GiB = int64(1) << 30

// Each catalog test starts never refreshed with the packaged catalog and no network
func lr4CatalogReset(t *testing.T) {
	t.Helper()
	packaged, err := packagedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	lrCatalogMu.Lock()
	prevEntries, prevLoaded := catalogCurrent, catalogLoaded
	lrCatalogMu.Unlock()
	prevAttempt, prevFetch, prevMono, prevGo := catalogLastAttempt, catalogFetch, catalogMonotonic, catalogGo
	setCatalog(packaged)
	catalogLastAttempt = nil
	catalogFetch = func() ([]byte, error) { return nil, errors.New("network disabled in tests") }
	t.Cleanup(func() {
		lrCatalogMu.Lock()
		catalogCurrent, catalogLoaded = prevEntries, prevLoaded
		lrCatalogMu.Unlock()
		catalogLastAttempt, catalogFetch, catalogMonotonic, catalogGo = prevAttempt, prevFetch, prevMono, prevGo
	})
}

// Fetchable catalog document built by mutating the packaged JSON
func lr4DocFrom(t *testing.T, mutate func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(packagedCatalogJSON, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Source path of the packaged catalog beside this test file
func lr4CatalogPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "local_runtime_catalog.json")
}

func lr4Budget(vramGiB, ramGiB float64) HardwareBudget {
	vram := int64(vramGiB * float64(lr4GiB))
	return NewHardwareBudget(vram, vram, int64(ramGiB*float64(lr4GiB)))
}

func lr4Discrete(sizeGB float64) HardwareBudget {
	total := int64(sizeGB * float64(lr4GiB))
	margin := max(2*lr4GiB, int64(float64(total)*0.09))
	b := NewHardwareBudget(max(0, total-margin), total, 64*lr4GiB)
	b.UMA = false
	return b
}

func lr4Unified(sizeGB float64) HardwareBudget {
	total := int64(sizeGB * float64(lr4GiB))
	b := NewHardwareBudget(int64(float64(total)*0.80), total, 0)
	b.UMA = true
	return b
}

func lr4Entry(t *testing.T, id string) CatalogEntry {
	t.Helper()
	e, ok := CatalogByID()[id]
	if !ok {
		t.Fatalf("catalog has no %s", id)
	}
	return e
}

func lr4Picked(budget HardwareBudget, entries []CatalogEntry, backend string) string {
	e, reason := RecommendedEntry(budget, entries, backend)
	if e == nil {
		return "None"
	}
	return e.ID + "/" + reason
}

func TestCatalogPackagedJSONRoundTripsTheCatalog(t *testing.T) {
	lr4CatalogReset(t)
	if len(Catalog()) < 4 {
		t.Fatalf("catalog has %d entries", len(Catalog()))
	}
	for _, e := range Catalog() {
		if len(e.Variants) == 0 || e.NCtxTrain <= 0 || e.PerLayerF16 < 0 {
			t.Fatalf("%s lacks estimator inputs", e.ID)
		}
		var sizes []int64
		for _, v := range e.Variants {
			sizes = append(sizes, v.SizeBytes())
		}
		if !sort.SliceIsSorted(sizes, func(i, j int) bool { return sizes[i] > sizes[j] }) {
			t.Fatalf("%s not best-first", e.ID)
		}
	}
	first := Catalog()[0]
	if len(first.Sampling) == 0 || first.Sampling[0].Key != "chat-template-kwargs" {
		t.Fatalf("sampling lost its document order: %v", first.Sampling)
	}
}

func TestCatalogRefreshSwapsInMemoryOnly(t *testing.T) {
	lr4CatalogReset(t)
	before, err := os.ReadFile(lr4CatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	body := lr4DocFrom(t, func(doc map[string]any) {
		models := doc["models"].([]any)
		day0 := map[string]any{}
		for k, v := range models[0].(map[string]any) {
			day0[k] = v
		}
		day0["id"], day0["display_name"], day0["description"], day0["min_engine"] = "day0-model", "Day 0", "new", "b99999"
		doc["models"] = append(models, day0)
	})
	catalogFetch = func() ([]byte, error) { return body, nil }
	if !RefreshCatalog(true) {
		t.Fatal("refresh reported no swap")
	}
	if CatalogByID()["day0-model"].MinEngine != "b99999" {
		t.Fatal("the fetched day 0 entry is missing")
	}
	after, _ := os.ReadFile(lr4CatalogPath())
	if !bytes.Equal(before, after) {
		t.Fatal("the packaged catalog changed on disk")
	}
}

func TestCatalogRefreshFailureKeepsCurrentCatalog(t *testing.T) {
	lr4CatalogReset(t)
	catalogFetch = func() ([]byte, error) { return nil, errors.New("offline") }
	before := len(Catalog())
	if RefreshCatalog(true) || len(Catalog()) != before {
		t.Fatal("a failed fetch replaced the catalog")
	}
}

func TestCatalogRefreshRejectsWrongSchema(t *testing.T) {
	lr4CatalogReset(t)
	body := lr4DocFrom(t, func(doc map[string]any) { doc["schema_version"] = catalogSchemaVersion + 1 })
	catalogFetch = func() ([]byte, error) { return body, nil }
	before := Catalog()
	if RefreshCatalog(true) || len(Catalog()) != len(before) {
		t.Fatal("a newer schema replaced the catalog")
	}
	doc, _ := moaJSONLoads(string(body))
	if _, err := loadCatalog(doc, func(int) []string { return nil }); err == nil || err.Error() != "catalog schema 4 (this build reads 3)" {
		t.Fatalf("schema error = %v", err)
	}
}

func TestCatalogLoaderIgnoresUnknownFields(t *testing.T) {
	body := lr4DocFrom(t, func(doc map[string]any) {
		doc["models"].([]any)[0].(map[string]any)["future_field"] = map[string]any{"anything": true}
	})
	entries, err := loadCatalogJSON(body)
	if err != nil || entries[0].ID != "qwen3.8-27b" {
		t.Fatalf("load = %v, %v", entries, err)
	}
}

func TestCatalogLoaderCoercesScalarsLikePython(t *testing.T) {
	body := lr4DocFrom(t, func(doc map[string]any) {
		m := doc["models"].([]any)[0].(map[string]any)
		m["n_ctx_train"], m["moe"], m["decode_fraction"], m["min_engine"], m["quality"] = "4096", 1, "0.5", 7, true
		m["sampling"] = []any{[]any{"temp", "0.7"}, "ab"}
	})
	entries, err := loadCatalogJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if e.NCtxTrain != 4096 || !e.MoE || e.DecodeFraction != 0.5 || e.MinEngine != "7" || e.Quality != 1 {
		t.Fatalf("coerced entry = %+v", e)
	}
	if fmt.Sprint(e.Sampling) != "[{temp 0.7} {a b}]" {
		t.Fatalf("sampling = %v", e.Sampling)
	}
	missing := lr4DocFrom(t, func(doc map[string]any) { delete(doc["models"].([]any)[0].(map[string]any), "full_layers") })
	if _, err := loadCatalogJSON(missing); err == nil {
		t.Fatal("a missing required field loaded")
	}
}

func TestCatalogFirstRefreshIsDueShortlyAfterBoot(t *testing.T) {
	lr4CatalogReset(t)
	catalogMonotonic = func() time.Duration { return 60 * time.Second }
	started := 0
	catalogGo = func(func()) { started++ }
	RefreshCatalogSoon()
	if started != 1 {
		t.Fatalf("refresh threads started = %d", started)
	}
	body := lr4DocFrom(t, func(map[string]any) {})
	catalogFetch = func() ([]byte, error) { return body, nil }
	if !RefreshCatalog(false) {
		t.Fatal("the first refresh after boot was throttled")
	}
	if RefreshCatalog(false) {
		t.Fatal("the TTL gate let a second refresh through")
	}
	RefreshCatalogSoon()
	if started != 1 {
		t.Fatal("a refresh started inside the TTL")
	}
}

var lr4MakerCompressed = map[string]bool{"nemotron-3.5-bolt-74b-a9b": true}

func TestCatalogEveryEntryShipsExactlyOneQ4Build(t *testing.T) {
	lr4CatalogReset(t)
	for _, e := range Catalog() {
		if len(e.Variants) != 1 {
			t.Fatalf("%s: %d variants", e.ID, len(e.Variants))
		}
		build := e.Variants[0]
		q4 := false
		for _, p := range []string{"UD-Q4", "Q4", "UD-IQ4", "IQ4"} {
			q4 = q4 || strings.HasPrefix(build.Quant, p)
		}
		if !lr4MakerCompressed[e.ID] && !q4 {
			t.Fatalf("%s ships %s", e.ID, build.Quant)
		}
		for _, a := range e.DownloadFiles(build) {
			if a.SizeBytes <= 0 {
				t.Fatalf("%s: no size on %s", e.ID, a.Path)
			}
		}
	}
}

func TestCatalogSplitVariantsHaveCoherentParts(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "deepseek-v4-flash")
	for _, v := range e.Variants {
		var total int64
		for _, f := range v.Files {
			total += f.SizeBytes
		}
		if len(v.Files) < 2 || !strings.Contains(v.Files[0].Path, "00001-of") || v.SizeBytes() != total {
			t.Fatalf("incoherent split variant %+v", v)
		}
	}
	if e.Draft == nil {
		t.Fatal("DSpark draft missing")
	}
}

func TestCatalogSelectionIsTheQ4BuildEvenWithHeadroom(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "qwen3.8-27b")
	c := SelectVariant(e, lr4Budget(60, 64))
	if c == nil || !c.ZeroSpill || c.Variant.Quant != e.Variants[len(e.Variants)-1].Quant || c.ReasonKey != "best-large-window" {
		t.Fatalf("choice = %+v", c)
	}
}

func TestCatalogSelectedBuildConstantAndFitShapeMonotoneInVRAM(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "qwen3.8-27b")
	rank := map[string]int{"smallest-fits-spilled": 0, "best-fits": 1, "best-large-window": 2}
	var shapes []int
	for _, vram := range []float64{8, 12, 16, 24, 32, 48} {
		c := SelectVariant(e, lr4Budget(vram, 64))
		if c == nil || c.Variant.Quant != e.Variants[len(e.Variants)-1].Quant {
			t.Fatalf("%v GiB choice = %+v", vram, c)
		}
		shapes = append(shapes, rank[c.ReasonKey])
	}
	if !sort.IntsAreSorted(shapes) {
		t.Fatalf("fit shape not monotone: %v", shapes)
	}
}

func TestCatalogSmallCardGetsQ4SpilledNeverBelow(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "qwen3.8-27b")
	c := SelectVariant(e, lr4Budget(8, 64))
	if c == nil || c.ZeroSpill || c.ReasonKey != "smallest-fits-spilled" || c.Variant.Quant != e.Variants[len(e.Variants)-1].Quant {
		t.Fatalf("choice = %+v", c)
	}
}

func TestCatalogFrontierModelRefusedOnConsumerCardOfferedOnBigRAM(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "deepseek-v4-flash")
	if c := SelectVariant(e, lr4Budget(32, 32)); c != nil {
		t.Fatalf("consumer card choice = %+v", c)
	}
	if c := SelectVariant(e, lr4Budget(32, 192)); c == nil || c.ZeroSpill {
		t.Fatalf("workstation choice = %+v", c)
	}
}

func TestCatalogSelectionAccountsForKVNotJustWeights(t *testing.T) {
	lr4CatalogReset(t)
	e := lr4Entry(t, "qwen3.8-27b")
	size := e.Variants[0].SizeBytes() + 100<<20
	if c := SelectVariant(e, NewHardwareBudget(size, size, 64*lr4GiB)); c == nil || c.ZeroSpill {
		t.Fatalf("choice = %+v", c)
	}
}

func TestCatalogTargetNeverDegradesBelowFloorChoice(t *testing.T) {
	lr4CatalogReset(t)
	for _, e := range Catalog() {
		for _, vram := range []float64{8, 12, 16, 24, 32, 48, 96} {
			c := SelectVariant(e, lr4Budget(vram, 256))
			if c != nil && c.ZeroSpill && c.ReasonKey != "best-large-window" && c.ReasonKey != "best-fits" {
				t.Fatalf("%s at %v GiB: %s", e.ID, vram, c.ReasonKey)
			}
		}
	}
}

func TestCatalogFindEntryForModelResolvesSplitIDs(t *testing.T) {
	lr4CatalogReset(t)
	e, v := FindEntryForModel("DeepSeek-V4-Flash-0731-UD-Q4_K_XL")
	if e == nil || e.ID != "deepseek-v4-flash" || v.Quant != "UD-Q4_K_XL" {
		t.Fatalf("hit = %v %v", e, v)
	}
	if EntryForModel("no-such-model") != nil {
		t.Fatal("an unknown model resolved")
	}
}

func TestCatalogHybridLongContextStaysCheap(t *testing.T) {
	lr4CatalogReset(t)
	hybrid := lr4Entry(t, "qwen3.6-35b-a3b")
	hp := hybrid.Profile(hybrid.Variants[len(hybrid.Variants)-1])
	var dense []ProfileLayer
	full := 0
	for _, l := range hp.Layers {
		dense = append(dense, ProfileLayer{Kind: LayerFull, KVBytesPerTok: hybrid.PerLayerF16})
		if l.Kind == LayerFull {
			full++
		}
	}
	dp := NewModelProfile("synthetic-dense", hp.WeightsBytes, 0, hybrid.NCtxTrain, dense)
	denseKV, hybridKV := CtxBytes(dp, ContextFloor, true), CtxBytes(hp, ContextFloor, true)
	expected := float64(denseKV) * float64(full) / float64(len(hp.Layers)) * hp.KVScale
	if hybridKV >= denseKV || math.Abs(float64(hybridKV)-expected)/expected >= 0.25 {
		t.Fatalf("hybrid KV %d dense %d expected %.0f", hybridKV, denseKV, expected)
	}
}

func TestCatalogRecommendationDecisionTable(t *testing.T) {
	lr4CatalogReset(t)
	for _, tc := range []struct {
		size float64
		kind string
		want string
	}{
		{8, "discrete", "None"}, {8, "unified", "None"}, {16, "discrete", "None"}, {16, "unified", "None"},
		{24, "discrete", "qwen3.8-27b/best-quality-resident"}, {24, "unified", "None"},
		{32, "discrete", "qwen3.8-27b/best-quality-resident"}, {32, "unified", "qwen3.6-35b-a3b/speed-gated-quality"},
		{48, "discrete", "qwen3.8-27b/best-quality-resident"}, {48, "unified", "qwen3.6-35b-a3b/speed-gated-quality"},
		{96, "discrete", "qwen3.8-flash-next/best-quality-resident"}, {96, "unified", "qwen3.8-flash-next/best-quality-resident"},
		{128, "discrete", "qwen3.8-flash-next/best-quality-resident"}, {128, "unified", "qwen3.8-flash-next/best-quality-resident"},
		{256, "discrete", "qwen3.8-flash-next/best-quality-resident"}, {256, "unified", "qwen3.8-flash-next/best-quality-resident"},
		{512, "discrete", "qwen3.8-flash-next/best-quality-resident"}, {512, "unified", "qwen3.8-flash-next/best-quality-resident"},
	} {
		b := lr4Discrete(tc.size)
		if tc.kind == "unified" {
			b = lr4Unified(tc.size)
		}
		if got := lr4Picked(b, nil, "auto"); got != tc.want {
			t.Errorf("%vGB-%s = %s, want %s", tc.size, tc.kind, got, tc.want)
		}
	}
}

func TestCatalogMeasuredN1XProfileChangesSpeedEligibilityNotFitOrQuality(t *testing.T) {
	lr4CatalogReset(t)
	prev := catalogProductDefaults
	catalogProductDefaults = map[catalogHardwareKey]string{}
	t.Cleanup(func() { catalogProductDefaults = prev })
	for _, name := range []string{"NVIDIA RTX Spark N1X", "NVIDIA RTX Spark N1X (5120-core Blackwell RTX GPU)",
		"NVIDIA RTX Spark N1X (updated device description)"} {
		for capacity, want := range map[float64]string{24: "None", 48: "qwen3.8-27b", 256: "qwen3.8-flash-next"} {
			b := lr4Unified(capacity)
			b.GPUName, b.Platform = name, "win32"
			e := lr4Entry(t, "qwen3.8-27b")
			if PredictedDecodeTokS(e, e.Variants[0], b, false, "auto") < PleasantFloorTokS {
				t.Fatalf("%s: measured rate below the floor", name)
			}
			got, _ := RecommendedEntry(b, nil, "auto")
			id := "None"
			if got != nil {
				id = got.ID
			}
			if id != want {
				t.Fatalf("%s at %v GB = %s, want %s", name, capacity, id, want)
			}
		}
	}
}

func TestCatalogUnmatchedOrSpilledProfilesKeepTheExistingEstimate(t *testing.T) {
	lr4CatalogReset(t)
	for i, tc := range []struct {
		budget  func(*HardwareBudget)
		entry   func(*CatalogEntry)
		quant   string
		backend string
		spilled bool
	}{
		{func(b *HardwareBudget) { b.GPUName = "" }, nil, "UD-Q4_K_M", "cuda", false},
		{func(b *HardwareBudget) { b.GPUName = "NVIDIA Other Device" }, nil, "UD-Q4_K_M", "cuda", false},
		{func(b *HardwareBudget) { b.GPUName = "NVIDIA RTX Spark N1X2" }, nil, "UD-Q4_K_M", "cuda", false},
		{func(b *HardwareBudget) { b.GPUName = "NVIDIA RTX Spark N1X Pro" }, nil, "UD-Q4_K_M", "cuda", false},
		{func(b *HardwareBudget) { b.Platform = "linux" }, nil, "UD-Q4_K_M", "cuda", false},
		{func(b *HardwareBudget) { b.UMA = false }, nil, "UD-Q4_K_M", "cuda", false},
		{nil, nil, "UD-Q4_K_M", "vulkan", false},
		{nil, nil, "UD-Q4_K_M", "cpu", false},
		{nil, func(e *CatalogEntry) { e.MTP = false }, "UD-Q4_K_M", "cuda", false},
		{nil, func(e *CatalogEntry) { e.MTPDraftDepth = 3 }, "UD-Q4_K_M", "cuda", false},
		{nil, nil, "Q8_0", "cuda", false},
		{nil, nil, "UD-Q4_K_M", "cuda", true},
	} {
		b := lr4Unified(48)
		b.GPUName, b.Platform = "NVIDIA RTX Spark N1X (5120-core Blackwell RTX GPU)", "win32"
		if tc.budget != nil {
			tc.budget(&b)
		}
		e := lr4Entry(t, "qwen3.8-27b")
		if tc.entry != nil {
			tc.entry(&e)
		}
		v := e.Variants[0]
		v.Quant = tc.quant
		plain := b
		plain.GPUName = ""
		fallback := PredictedDecodeTokS(e, v, plain, tc.spilled, tc.backend)
		if got := PredictedDecodeTokS(e, v, b, tc.spilled, tc.backend); got != fallback {
			t.Fatalf("case %d: %v, want the estimate %v", i, got, fallback)
		}
	}
}

func TestCatalogEveryEntryCarriesTheRecommendationAxes(t *testing.T) {
	lr4CatalogReset(t)
	for _, e := range Catalog() {
		if (e.Recommend && e.Quality <= 0) || e.DecodeFraction <= 0 || e.DecodeFraction > 1 || (!e.MoE && e.DecodeFraction != 1) {
			t.Fatalf("%s lacks recommendation axes", e.ID)
		}
	}
}

func TestCatalogNemotronBoltIsAnOptionNeverTheRecommendation(t *testing.T) {
	lr4CatalogReset(t)
	bolt := lr4Entry(t, "nemotron-3.5-bolt-74b-a9b")
	for _, b := range []HardwareBudget{lr4Discrete(32), lr4Discrete(48), lr4Discrete(96), lr4Unified(32), lr4Unified(64), lr4Unified(128)} {
		c := SelectVariant(bolt, b)
		if bolt.Recommend || c == nil || !c.ZeroSpill || PredictedDecodeTokS(bolt, c.Variant, b, false, "auto") < PleasantFloorTokS {
			t.Fatalf("bolt on %+v = %+v", b, c)
		}
		if got := lr4Picked(b, []CatalogEntry{bolt}, "auto"); got != "None" {
			t.Fatalf("bolt alone picked %s", got)
		}
		if got, _ := RecommendedEntry(b, nil, "auto"); got != nil && got.ID == bolt.ID {
			t.Fatal("bolt was recommended")
		}
	}
}

func TestCatalogUnifiedNeverRecommendsABelowFloorDenseModel(t *testing.T) {
	lr4CatalogReset(t)
	b := lr4Unified(128)
	pick, _ := RecommendedEntry(b, nil, "auto")
	if pick == nil {
		t.Fatal("no pick")
	}
	c := SelectVariant(*pick, b)
	if c == nil || !c.ZeroSpill {
		t.Fatalf("pick %s does not run resident", pick.ID)
	}
	clears := false
	for _, e := range Catalog() {
		if ec := SelectVariant(e, b); ec != nil && ec.ZeroSpill && PredictedDecodeTokS(e, ec.Variant, b, false, "auto") >= PleasantFloorTokS {
			clears = true
		}
	}
	if clears && PredictedDecodeTokS(*pick, c.Variant, b, false, "auto") < PleasantFloorTokS {
		t.Fatalf("%s runs below the floor", pick.ID)
	}
}

func TestCatalogADerivedRecommendationAlwaysClearsTheFloor(t *testing.T) {
	lr4CatalogReset(t)
	var budgets []HardwareBudget
	for _, gb := range []float64{16, 24, 31.5, 32, 48, 64, 96, 128, 256} {
		budgets = append(budgets, lr4Unified(gb))
	}
	for _, gb := range []float64{8, 12, 16, 24, 32, 48, 96} {
		budgets = append(budgets, lr4Discrete(gb))
	}
	for _, b := range budgets {
		pick, reason := RecommendedEntry(b, nil, "auto")
		if pick == nil || reason == "product-default" {
			continue
		}
		c := SelectVariant(*pick, b)
		if c == nil || !c.ZeroSpill || PredictedDecodeTokS(*pick, c.Variant, b, false, "auto") < PleasantFloorTokS {
			t.Fatalf("%s on %+v does not clear the floor", pick.ID, b)
		}
	}
}

func TestCatalogQualityDecidesWhereSpeedPermits(t *testing.T) {
	lr4CatalogReset(t)
	b := lr4Discrete(512)
	pick, _ := RecommendedEntry(b, nil, "auto")
	best := ""
	quality := -1
	for _, e := range Catalog() {
		if c := SelectVariant(e, b); e.Recommend && c != nil && c.ZeroSpill && e.Quality > quality {
			best, quality = e.ID, e.Quality
		}
	}
	if pick == nil || pick.ID != best {
		t.Fatalf("pick = %v, want %s", pick, best)
	}
}

const lr4N1XPCIID = int64(0x2E1210DE)

func TestCatalogTheRTXSparkDefaultsToThe27BWithFlashNextStillFitting(t *testing.T) {
	lr4CatalogReset(t)
	for _, identity := range []func(*HardwareBudget){
		func(b *HardwareBudget) { id := lr4N1XPCIID; b.GPUPCIID = &id },
		func(b *HardwareBudget) { b.GPUName = "NVIDIA RTX Spark N1X (5120-core Blackwell RTX GPU)" },
	} {
		b := lr4Unified(128)
		b.Platform = "win32"
		identity(&b)
		if got := lr4Picked(b, nil, "auto"); got != "qwen3.8-27b/product-default" {
			t.Fatalf("picked %s", got)
		}
		if c := SelectVariant(lr4Entry(t, "qwen3.8-flash-next"), b); c == nil || !c.ZeroSpill {
			t.Fatal("flash next no longer fits the spark")
		}
	}
}

func TestCatalogTheDerivedRuleDecidesOnOtherPlatformsBackendsAndWhenTheDefaultIsIneligible(t *testing.T) {
	lr4CatalogReset(t)
	spark := lr4Unified(128)
	id := lr4N1XPCIID
	spark.Platform, spark.GPUPCIID = "win32", &id
	var without []CatalogEntry
	for _, e := range Catalog() {
		if e.ID != "qwen3.8-27b" {
			without = append(without, e)
		}
	}
	linux := spark
	linux.Platform = "linux"
	for _, got := range []string{lr4Picked(spark, without, "auto"), lr4Picked(linux, nil, "auto"), lr4Picked(spark, nil, "vulkan")} {
		if strings.HasSuffix(got, "/product-default") || got == "None" {
			t.Fatalf("picked %s", got)
		}
	}
}

func TestCatalogRecommendationPolicy(t *testing.T) {
	lr4CatalogReset(t)
	for _, gb := range []float64{8, 16} {
		if got := lr4Picked(lr4Discrete(gb), nil, "auto"); got != "None" {
			t.Fatalf("%v GB discrete picked %s", gb, got)
		}
	}
	pick, reason := RecommendedEntry(lr4Discrete(24), nil, "auto")
	if pick == nil || reason != "best-quality-resident" {
		t.Fatalf("24 GB pick = %v %s", pick, reason)
	}
	if c := SelectVariant(*pick, lr4Discrete(24)); c == nil || !c.ZeroSpill {
		t.Fatal("24 GB pick does not run resident")
	}
	if c := SelectVariant(lr4Entry(t, "qwen3.8-27b"), lr4Discrete(16)); c == nil || c.ZeroSpill {
		t.Fatalf("16 GB 27B choice = %+v", c)
	}
	if got := lr4Picked(lr4Unified(24), nil, "auto"); got != "None" {
		t.Fatalf("24 GB unified picked %s", got)
	}
}
