package provider

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	lrtGiB = int64(1) << 30
	lrtMiB = int64(1) << 20
	lrtKiB = 1024
)

func lrtLayers(kind LayerKind, perToken int64, n int) []ProfileLayer {
	out := make([]ProfileLayer, n)
	for i := range out {
		out[i] = ProfileLayer{kind, perToken}
	}
	return out
}

func lrtDense(layers int, perToken, weightsGiB int64, native int) ModelProfile {
	return NewModelProfile("dense-32b", weightsGiB*lrtGiB, 0, native, lrtLayers(LayerFull, perToken, layers))
}

func lrtDenseDefault() ModelProfile { return lrtDense(64, 4096, 20, 128*lrtKiB) }

func lrtHybrid(full, recurrent int, perToken, weightsGiB int64, native int) ModelProfile {
	layers := append(lrtLayers(LayerFull, perToken, full), lrtLayers(LayerRecurrent, 0, recurrent)...)
	return NewModelProfile("hybrid-30b", weightsGiB*lrtGiB, 0, native, layers)
}

func lrtHybridDefault() ModelProfile { return lrtHybrid(16, 48, 4096, 22, 1024*1024) }

func lrtMoE() ModelProfile {
	p := NewModelProfile("moe-30b", 17*lrtGiB, 0, 256*1024, lrtLayers(LayerFull, 3072, 48))
	p.MoE = true
	return p
}

func lrtCard(vramGiB, ramGiB float64, uma bool) HardwareBudget {
	b := NewHardwareBudget(int64(vramGiB*float64(lrtGiB)), int64(vramGiB*float64(lrtGiB)), int64(ramGiB*float64(lrtGiB)))
	b.UMA = uma
	return b
}

func TestLocalRuntimeFullAttentionLinearInWindow(t *testing.T) {
	p := lrtDenseDefault()
	b32, b64, b128 := CtxBytes(p, 32*lrtKiB, true), CtxBytes(p, 64*lrtKiB, true), CtxBytes(p, 128*lrtKiB, true)
	if math.Abs(float64(b64)/float64(b32)-2) >= 0.01 || math.Abs(float64(b128)/float64(b64)-2) >= 0.01 {
		t.Fatalf("not linear %d %d %d", b32, b64, b128)
	}
}

func TestLocalRuntimeRecurrentStateConstantInWindow(t *testing.T) {
	p := lrtHybridDefault()
	delta := CtxBytes(p, 1024*lrtKiB, true) - CtxBytes(p, 32*lrtKiB, true)
	fullOnly := NewModelProfile("x", 0, 0, p.NCtxTrain, lrtLayers(LayerFull, 4096, 16))
	expected := CtxBytes(fullOnly, 1024*lrtKiB, true) - CtxBytes(fullOnly, 32*lrtKiB, true)
	if d := delta - expected; d > 1 || d < -1 {
		t.Fatalf("delta %d expected %d", delta, expected)
	}
}

func TestLocalRuntimeSWALayersCappedAtWindow(t *testing.T) {
	p := NewModelProfile("swa", 0, 0, 128*lrtKiB,
		append(lrtLayers(LayerSWA, 4096, 5), lrtLayers(LayerFull, 4096, 1)...))
	p.SWAWindow = 1024
	small, big := CtxBytes(p, 4*lrtKiB, true), CtxBytes(p, 32*lrtKiB, true)
	if float64(big)/float64(small) >= 0.6*8 {
		t.Fatalf("ratio %f", float64(big)/float64(small))
	}
}

func TestLocalRuntimeNonFAFallbackDoublesCtxCost(t *testing.T) {
	p := lrtDenseDefault()
	want := float64(CtxBytes(p, ContextFloor, true)) * 64 / 34
	if got := float64(CtxBytes(p, ContextFloor, false)); math.Abs(got-want) > want*0.001 {
		t.Fatalf("got %f want %f", got, want)
	}
}

func TestLocalRuntimePhysicsRefusalOnlyPastVRAMPlusRAM(t *testing.T) {
	p := lrtDense(64, 4096, 60, 128*lrtKiB)
	if PhysicsCheck(p, lrtCard(24, 64, false), ContextFloor, true, 0) != nil {
		t.Fatal("refused a fitting model")
	}
	r := PhysicsCheck(p, lrtCard(24, 16, false), ContextFloor, true, 0)
	if r == nil || !strings.Contains(r.Message, "dense-32b: needs ~") || !strings.Contains(r.Message, "at the 64K floor") {
		t.Fatalf("refusal %+v", r)
	}
}

func TestLocalRuntimePhysicsCheckPricesAtFloorNotNative(t *testing.T) {
	if PhysicsCheck(lrtHybrid(16, 48, 4096, 22, 1024*1024), lrtCard(24, 8, false), ContextFloor, true, 0) != nil {
		t.Fatal("priced at native")
	}
}

func TestLocalRuntimeInitialWindowAccountsForOverheadInEveryVerdict(t *testing.T) {
	for _, uma := range []bool{false, true} {
		profile := lrtHybrid(16, 48, 4096, 8, ContextFloor)
		base := profile.WeightsBytes + CtxBytes(profile, ContextFloor, true)
		overhead := 2 * lrtGiB
		ram := 4 * lrtGiB
		if uma {
			ram = 0
		}
		budget := NewHardwareBudget(base+lrtGiB, base+lrtGiB, ram)
		budget.UMA = uma
		decision := InitialWindow(profile, budget, true, overhead)
		if uma {
			r, ok := decision.(*PhysicsRefusal)
			if !ok || r.NeededBytes != base+overhead {
				t.Fatalf("uma decision %+v", decision)
			}
		} else if d, ok := decision.(*WindowDecision); !ok || d.SpillBytes != lrtGiB {
			t.Fatalf("decision %+v", decision)
		}
		exact := budget
		exact.UsableVRAMBytes, exact.RAMAvailableBytes = base+overhead, 0
		if d := InitialWindow(profile, exact, true, overhead).(*WindowDecision); d.Spilled() {
			t.Fatal("exact budget spilled")
		}
		short := exact
		short.UsableVRAMBytes--
		if _, ok := InitialWindow(profile, short, true, overhead).(*PhysicsRefusal); !ok {
			t.Fatal("short budget admitted")
		}
		growing := lrtHybrid(4, 0, 1024, 20, 1024*lrtKiB)
		floorNeed := growing.WeightsBytes + CtxBytes(growing, ContextFloor, true) + overhead
		limited := NewHardwareBudget(8*lrtGiB, 8*lrtGiB, floorNeed-8*lrtGiB)
		d, ok := InitialWindow(growing, limited, true, overhead).(*WindowDecision)
		if !ok || d.Window != ContextFloor || d.SpillBytes != limited.RAMAvailableBytes {
			t.Fatalf("limited %+v", d)
		}
	}
}

func TestLocalRuntimeLadderShape(t *testing.T) {
	rungs := Ladder(262144)
	if rungs[0] != ContextFloor || rungs[len(rungs)-1] != 262144 {
		t.Fatalf("rungs %v", rungs)
	}
	for i := 1; i < len(rungs); i++ {
		if rungs[i-1] >= rungs[i] || float64(rungs[i])/float64(rungs[i-1]) > 2 {
			t.Fatalf("rungs %v", rungs)
		}
	}
	if !reflect.DeepEqual(Ladder(ContextFloor), []int{ContextFloor}) || !reflect.DeepEqual(Ladder(98304), []int{65536, 98304}) {
		t.Fatalf("small ladders %v %v", Ladder(ContextFloor), Ladder(98304))
	}
}

func TestLocalRuntimeInitialWindowNeverBelowFloorAndNeverAboveNative(t *testing.T) {
	for _, p := range []ModelProfile{lrtDenseDefault(), lrtHybridDefault(), lrtMoE(), lrtDense(64, 4096, 20, 32*lrtKiB)} {
		for _, vram := range []float64{8, 16, 24, 32} {
			if d, ok := InitialWindow(p, lrtCard(vram, 64, false), true, 0).(*WindowDecision); ok {
				if d.Window < min(ContextFloor, p.NCtxTrain) || d.Window > p.NCtxTrain {
					t.Fatalf("%s at %v GiB window %d", p.Name, vram, d.Window)
				}
			}
		}
	}
}

func TestLocalRuntimeInitialWindowMonotoneInVRAM(t *testing.T) {
	prev := 0
	for _, vram := range []float64{8, 12, 16, 24, 32, 48} {
		d, ok := InitialWindow(lrtDenseDefault(), lrtCard(vram, 64, false), true, 0).(*WindowDecision)
		if !ok || d.Window < prev {
			t.Fatalf("vram %v decision %+v", vram, d)
		}
		prev = d.Window
	}
}

func TestLocalRuntimeFlatCurveReachesNativeWhereDenseDoesNot(t *testing.T) {
	h := lrtHybrid(16, 48, 384, 18, 1024*lrtKiB)
	d := lrtDense(64, 8192, 18, 1024*lrtKiB)
	dh, ok1 := InitialWindow(h, lrtCard(24, 64, false), true, 0).(*WindowDecision)
	dd, ok2 := InitialWindow(d, lrtCard(24, 64, false), true, 0).(*WindowDecision)
	if !ok1 || !ok2 || dh.Window != 1024*lrtKiB || dh.Spilled() || dd.Window >= 1024*lrtKiB {
		t.Fatalf("hybrid %+v dense %+v", dh, dd)
	}
}

func TestLocalRuntimeDenseOnSmallCardHoldsFloorAndSpills(t *testing.T) {
	d, ok := InitialWindow(lrtDense(64, 4096, 20, 128*lrtKiB), lrtCard(12, 64, false), true, 0).(*WindowDecision)
	if !ok || d.Window != ContextFloor || !d.Spilled() {
		t.Fatalf("decision %+v", d)
	}
	if !strings.HasPrefix(d.Reasons[0], "floor held at 64K") {
		t.Fatalf("reason %q", d.Reasons[0])
	}
}

func TestLocalRuntimeUMABudgetCapsTheWindowThroughPhysics(t *testing.T) {
	p := lrtHybrid(16, 48, 4096, 8, 1024*lrtKiB)
	unified := lrtCard(38.4, 0, true)
	d, ok := InitialWindow(p, unified, true, 0).(*WindowDecision)
	if !ok || d.Spilled() {
		t.Fatalf("decision %+v", d)
	}
	if 8*lrtGiB+CtxBytes(p, d.Window, true) > unified.UsableVRAMBytes {
		t.Fatal("window exceeds the unified budget")
	}
}

func lrtGrow(p ModelProfile, b HardwareBudget, current, session int, tokS *float64, idle bool) GrowthDecision {
	return DecideGrowth(p, b, current, session, tokS, idle, true, false)
}

func lrtF64(v float64) *float64 { return &v }

func TestLocalRuntimeGrowthHoldsBelowOccupancy(t *testing.T) {
	d := lrtGrow(lrtDenseDefault(), lrtCard(24, 64, false), ContextFloor, ContextFloor/2, lrtF64(40), true)
	if d.Action != GrowthActionHold {
		t.Fatalf("decision %+v", d)
	}
}

func TestLocalRuntimeGrowthRequiresIdleServer(t *testing.T) {
	d := lrtGrow(lrtDenseDefault(), lrtCard(24, 64, false), ContextFloor, lrtNinety(ContextFloor), lrtF64(40), false)
	if d.Action != GrowthActionHold || !strings.Contains(d.Reason, "idle") {
		t.Fatalf("decision %+v", d)
	}
}

func TestLocalRuntimeGrowthStepsOneRungAndIsMonotone(t *testing.T) {
	d := lrtGrow(lrtDense(64, 4096, 20, 262144), lrtCard(32, 64, false), ContextFloor, lrtNinety(ContextFloor), lrtF64(40), true)
	rungs := Ladder(262144)
	if d.Action != GrowthActionGrow || d.NextWindow <= ContextFloor || d.NextWindow != rungs[slices.Index(rungs, ContextFloor)+1] {
		t.Fatalf("decision %+v", d)
	}
	if d.Reason != "rung 64K -> 96K" {
		t.Fatalf("reason %q", d.Reason)
	}
}

func TestLocalRuntimeGrowthStopsAtNative(t *testing.T) {
	d := lrtGrow(lrtDense(64, 4096, 20, 128*lrtKiB), lrtCard(48, 64, false), 128*lrtKiB, lrtNinety(128*lrtKiB), lrtF64(40), true)
	if d.Action != GrowthActionCompress {
		t.Fatalf("decision %+v", d)
	}
}

func TestLocalRuntimeSpeedFloorFlipsDefaultToCompression(t *testing.T) {
	d := lrtGrow(lrtDenseDefault(), lrtCard(24, 64, false), ContextFloor, lrtNinety(ContextFloor), lrtF64(SpeedFloorTokS-2), true)
	want := "decode 4.0 tok/s below the ~6 tok/s floor; growth is now an explicit per-session choice"
	if d.Action != GrowthActionCompress || d.Reason != want {
		t.Fatalf("decision %+v", d)
	}
}

func TestLocalRuntimeGrowthRefitsAgainstLiveBudget(t *testing.T) {
	d := lrtGrow(lrtDense(64, 4096, 20, 128*lrtKiB), lrtCard(2, 1, false), ContextFloor, lrtNinety(ContextFloor), lrtF64(40), true)
	if d.Action != GrowthActionCompress || !strings.Contains(d.Reason, "physics") {
		t.Fatalf("decision %+v", d)
	}
}

func TestLocalRuntimeSpillOverridesPreferExpertAndRecurrentFFN(t *testing.T) {
	if !strings.Contains(strings.Join(SpillOverrides(lrtMoE(), nil), " "), "exps") ||
		!strings.Contains(strings.Join(SpillOverrides(lrtHybridDefault(), nil), " "), "ffn") ||
		len(SpillOverrides(lrtDenseDefault(), nil)) != 0 {
		t.Fatal("spill override shapes")
	}
}

func lrtOTPattern(t *testing.T, args []string) string {
	t.Helper()
	if args[0] != "-ot" || !strings.HasSuffix(args[1], "=CPU") {
		t.Fatalf("args %v", args)
	}
	return strings.TrimSuffix(args[1], "=CPU")
}

func lrtMovedDown(pattern string, n int) []int {
	re := regexp.MustCompile(pattern)
	out := []int{}
	for i := 0; i < n; i++ {
		if re.MatchString("blk." + strconv.Itoa(i) + ".ffn_down.weight") {
			out = append(out, i)
		}
	}
	return out
}

func TestLocalRuntimeHybridSpillMovesOnlyRecurrentBlocksNeededForSpillBytes(t *testing.T) {
	var layers []ProfileLayer
	var recurrent []int
	ffn := map[int]int64{}
	for i := 0; i < 16; i++ {
		if i%4 == 3 {
			layers = append(layers, ProfileLayer{LayerFull, 4096})
		} else {
			layers = append(layers, ProfileLayer{LayerRecurrent, 0})
			recurrent = append(recurrent, i)
		}
		ffn[i] = 100 * lrtMiB
	}
	p := NewModelProfile("interleaved-hybrid", 8*lrtGiB, 0, ContextFloor, layers)
	p.FFNBlockBytes = ffn
	for _, tc := range []struct {
		spill int64
		want  []int
	}{{250 * lrtMiB, []int{0, 1, 2}}, {300 * lrtMiB, []int{0, 1, 2}}, {301 * lrtMiB, []int{0, 1, 2, 4}}} {
		if got := RecurrentSpillBlocks(p, lrtI64(tc.spill)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("spill %d got %v", tc.spill, got)
		}
	}
	if got := lrtMovedDown(lrtOTPattern(t, SpillOverrides(p, lrtI64(250*lrtMiB))), 16); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("moved %v", got)
	}
	if got := lrtMovedDown(lrtOTPattern(t, SpillOverrides(p, lrtI64(150*lrtMiB))), 16); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("moved %v", got)
	}
	big := lrtOTPattern(t, SpillOverrides(p, lrtI64(20*lrtGiB)))
	if got := lrtMovedDown(big, 16); !reflect.DeepEqual(got, recurrent) {
		t.Fatalf("moved %v", got)
	}
	if regexp.MustCompile(big).MatchString("blk.3.ffn_up.weight") {
		t.Fatal("full attention block moved")
	}
}

func TestLocalRuntimeHybridSpillWithoutFFNSizesMovesEveryRecurrentBlockOnly(t *testing.T) {
	p := lrtHybrid(16, 48, 4096, 22, 1024*1024)
	want := []int{}
	for i := 16; i < 64; i++ {
		want = append(want, i)
	}
	if got := lrtMovedDown(lrtOTPattern(t, SpillOverrides(p, lrtI64(lrtGiB))), 64); !reflect.DeepEqual(got, want) {
		t.Fatalf("moved %v", got)
	}
	partial := p
	partial.FFNBlockBytes = map[int]int64{}
	for i := 16; i < 63; i++ {
		partial.FFNBlockBytes[i] = lrtGiB
	}
	if got := RecurrentSpillBlocks(partial, lrtI64(lrtGiB)); !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks %v", got)
	}
}

func TestLocalRuntimeLaunchArgsSizesHybridSpillFromDecision(t *testing.T) {
	p := lrtHybrid(2, 6, 4096, 22, 1024*1024)
	for i := 0; i < 8; i++ {
		p.FFNBlockBytes[i] = lrtGiB
	}
	spilled := &WindowDecision{Window: ContextFloor, SpillBytes: int64(1.5 * float64(lrtGiB)), KVOnGPU: true}
	args := LaunchArgs(p, spilled, true, false, DefaultMTPDraftDepth, false, false)
	i := slices.Index(args, "-ot")
	if got := lrtMovedDown(lrtOTPattern(t, args[i:i+2]), 8); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("moved %v", got)
	}
	if slices.Contains(LaunchArgs(p, spilled, true, false, DefaultMTPDraftDepth, true, false), "-ot") {
		t.Fatal("uma pinned tensors")
	}
}

func lrtArgAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestLocalRuntimeLaunchArgsContract(t *testing.T) {
	p := lrtMoE()
	spilled := &WindowDecision{Window: ContextFloor, SpillBytes: 4 * lrtGiB, KVOnGPU: true}
	resident := &WindowDecision{Window: 131072, KVOnGPU: true}
	a := LaunchArgs(p, spilled, true, true, DefaultMTPDraftDepth, false, false)
	if !reflect.DeepEqual(a[:2], []string{"-c", strconv.Itoa(ContextFloor)}) || !slices.Contains(a, "q8_0") ||
		!slices.Contains(a, "-ot") || !slices.Contains(a, "--spec-type") || lrtArgAfter(a, "--spec-draft-n-max") != "3" {
		t.Fatalf("spilled args %v", a)
	}
	b := LaunchArgs(p, resident, true, true, 2, false, false)
	if slices.Contains(b, "-ot") || !slices.Contains(b, "--spec-type") || lrtArgAfter(b, "--spec-draft-n-max") != "2" ||
		!slices.Contains(b, "--backend-sampling") || !slices.Contains(b, "--spec-draft-backend-sampling") || slices.Contains(b, "-ub") {
		t.Fatalf("resident args %v", b)
	}
	s := LaunchArgs(p, resident, true, true, 2, false, true)
	if lrtArgAfter(s, "-ub") != "2048" || lrtArgAfter(s, "-b") != "4096" || !slices.Contains(s, "--spec-type") {
		t.Fatalf("prefill args %v", s)
	}
	v := int64(248320)
	if UbLogitsBytes(v, true, false) != 512*v*4*2 || UbLogitsBytes(v, true, true) != int64(float64(2048*v*4)*1.5) {
		t.Fatal("logits pricing")
	}
	c := LaunchArgs(p, resident, true, false, DefaultMTPDraftDepth, false, false)
	if lrtArgAfter(c, "-ub") != "2048" || slices.Contains(c, "--spec-type") {
		t.Fatalf("plain args %v", c)
	}
	if d := LaunchArgs(p, spilled, false, false, DefaultMTPDraftDepth, false, false); slices.Contains(d, "q8_0") {
		t.Fatalf("non fa args %v", d)
	}
}

func TestLocalRuntimeLaunchArgsUMANeverPinsTensors(t *testing.T) {
	p := lrtMoE()
	spilled := &WindowDecision{Window: ContextFloor, SpillBytes: 4 * lrtGiB, KVOnGPU: true}
	u := LaunchArgs(p, spilled, true, false, DefaultMTPDraftDepth, true, false)
	if slices.Contains(u, "-ot") || !reflect.DeepEqual(u[:2], []string{"-c", strconv.Itoa(ContextFloor)}) || !slices.Contains(u, "q8_0") {
		t.Fatalf("uma args %v", u)
	}
	disc := LaunchArgs(p, spilled, true, false, DefaultMTPDraftDepth, false, false)
	if !slices.Contains(disc, "-ot") {
		t.Fatalf("discrete args %v", disc)
	}
	strip := func(args []string) []string {
		out := []string{}
		for _, x := range args {
			if x != "-ot" && !strings.HasPrefix(x, "blk") {
				out = append(out, x)
			}
		}
		return out
	}
	if !reflect.DeepEqual(strip(disc), strip(u)) {
		t.Fatalf("shapes differ %v %v", disc, u)
	}
}

func TestLocalRuntimeUbLogitsBytesPricesTheFlagChoice(t *testing.T) {
	v := int64(248320)
	if UbLogitsBytes(v, false, false) != 2048*v*4 || UbLogitsBytes(v, true, false) != 512*v*4*2 || UbLogitsBytes(0, true, false) != 0 {
		t.Fatal("logits pricing")
	}
}

func TestLocalRuntimeKVScalePricesMTPDraftContext(t *testing.T) {
	p := lrtMoE()
	base := CtxBytes(p, 131072, true)
	scaled := p
	scaled.KVScale = 1.2
	got := CtxBytes(scaled, 131072, true)
	if got != int64(float64(base)*1.2) || got <= base {
		t.Fatalf("base %d scaled %d", base, got)
	}
}

// Ninety percent of a window, the session depth growth tests use by default
func lrtNinety(window int) int { return int(float64(window) * 0.9) }

// Launch plan and flags for an MTP capable model as the preset pass would request them
func lrtMTPPlan(p ModelProfile, device, ram int64, override int) (*WindowDecision, LaunchPlan, []string) {
	plan := PlanLaunch(p, NewHardwareBudget(device, device, ram), true, RuntimeOverheadBytes, override)
	d := plan.Decision.(*WindowDecision)
	return d, plan, LaunchArgs(*plan.Profile, d, true, true, DefaultMTPDraftDepth, false, plan.MTPPrefill)
}

func TestLocalRuntimeMTPPlanMatchesCostAtInitialAndRestoredWindows(t *testing.T) {
	p := NewModelProfile("mtp-fit", 16*lrtGiB, 0, 262144, lrtLayers(LayerFull, 4096, 32))
	p.MoE, p.NVocab = true, 151936
	priced := p
	priced.KVScale = 1.2
	lean := RuntimeOverheadBytes + UbLogitsBytes(p.NVocab, true, false)
	stacked := RuntimeOverheadBytes + UbLogitsBytes(p.NVocab, true, true)
	floorNeed := p.WeightsBytes + CtxBytes(priced, ContextFloor, true)

	d, _, args := lrtMTPPlan(p, floorNeed+lean, 8*lrtGiB, 0)
	if d.Window != ContextFloor || d.Spilled() || slices.Contains(args, "-ub") || lrtArgAfter(args, "--spec-type") != "draft-mtp" {
		t.Fatalf("initial %+v %v", d, args)
	}
	for _, override := range []int{0, ContextFloor, 73728} {
		d, _, args := lrtMTPPlan(p, 16*lrtGiB, 64*lrtGiB, override)
		if !d.Spilled() || slices.Contains(args, "-ub") {
			t.Fatalf("spilled boot %d %+v %v", override, d, args)
		}
	}
	device := floorNeed + stacked
	control, _, args := lrtMTPPlan(p, device, 8*lrtGiB, 0)
	if lrtArgAfter(args, "-ub") != "2048" {
		t.Fatalf("control %v", args)
	}
	for _, ram := range []int64{8 * lrtGiB, 0} {
		g, _, args := lrtMTPPlan(p, device, ram, 73728)
		if g.Window != 73728 || g.Spilled() || slices.Contains(args, "-ub") || slices.Contains(args, "-ot") ||
			lrtArgAfter(args, "--spec-type") != "draft-mtp" || p.WeightsBytes+CtxBytes(priced, g.Window, true)+lean > device {
			t.Fatalf("grown ram %d %+v %v", ram, g, args)
		}
		if g.Reasons[0] != "grown window restored (72K)" {
			t.Fatalf("reason %q", g.Reasons[0])
		}
	}
	both, _, args := lrtMTPPlan(p, device, 64*lrtGiB, 147456)
	if both.Window != 147456 || !both.Spilled() || lrtArgAfter(args, "-ub") != "2048" || !slices.Contains(args, "-ot") {
		t.Fatalf("both spill %+v %v", both, args)
	}
	smaller, _, _ := lrtMTPPlan(p, floorNeed+lean, 0, 73728)
	if smaller.Window != ContextFloor || smaller.Spilled() {
		t.Fatalf("smaller boot %+v", smaller)
	}
	if p.WeightsBytes+CtxBytes(priced, control.Window, true)+stacked > device {
		t.Fatal("control exceeds the device")
	}
}

func TestLocalRuntimeFitToFreeMemoryNarrowsOnlyResidentPlans(t *testing.T) {
	p := lrtDense(32, 1024, 8, 262144)
	capacity := NewHardwareBudget(24*lrtGiB, 24*lrtGiB, 64*lrtGiB)
	plan := PlanLaunch(p, capacity, false, RuntimeOverheadBytes, 0)
	resident := plan.Decision.(*WindowDecision)
	if resident.Spilled() || resident.Window <= ContextFloor {
		t.Fatalf("capacity plan %+v", resident)
	}
	if got := FitToFreeMemory(plan, p, capacity, false, RuntimeOverheadBytes); got.Decision != plan.Decision {
		t.Fatal("an idle card moved the plan")
	}
	busy := NewHardwareBudget(FootprintBytes(PostureProfile(p, false, false), 98304, true, RuntimeOverheadBytes+UbLogitsBytes(0, false, false)), 24*lrtGiB, 64*lrtGiB)
	narrowed := FitToFreeMemory(plan, p, busy, false, RuntimeOverheadBytes).Decision.(*WindowDecision)
	if narrowed.Window != 98304 || narrowed.Spilled() || narrowed.Reasons[0] != fmt.Sprintf("96K fits beside other programs' GPU memory (%dK would not)", resident.Window/1024) {
		t.Fatalf("narrowed %+v", narrowed)
	}
	full := NewHardwareBudget(2*lrtGiB, 24*lrtGiB, 64*lrtGiB)
	floored := FitToFreeMemory(plan, p, full, false, RuntimeOverheadBytes)
	fd := floored.Decision.(*WindowDecision)
	if fd.Window != ContextFloor || fd.Spilled() || !fd.KVOnGPU || fd.Reasons[0] != "floor held at 64K; other programs hold most of the GPU memory" || floored.Profile != plan.Profile {
		t.Fatalf("floored %+v", fd)
	}
	spilledPlan := PlanLaunch(p, NewHardwareBudget(4*lrtGiB, 4*lrtGiB, 64*lrtGiB), false, RuntimeOverheadBytes, 0)
	if got := FitToFreeMemory(spilledPlan, p, full, false, RuntimeOverheadBytes); got.Decision != spilledPlan.Decision {
		t.Fatal("a spilled plan moved")
	}
}

func TestLocalRuntimePosturePricesWindowComputeByMicrobatchAndContexts(t *testing.T) {
	p := NewModelProfile("flash", 60*lrtGiB, 0, 262144, lrtLayers(LayerFull, 1024, 8))
	p.Architecture = "qwen4exp"
	lean := PostureProfile(p, true, false)
	stacked := PostureProfile(p, true, true)
	if stacked.WindowComputePerToken != 4*lean.WindowComputePerToken || lean.WindowComputePerToken != 46*512*2 {
		t.Fatalf("lean %d stacked %d", lean.WindowComputePerToken, stacked.WindowComputePerToken)
	}
	if plain := PostureProfile(p, false, false); plain.WindowComputePerToken != 46*2048 {
		t.Fatalf("plain %d", plain.WindowComputePerToken)
	}
	if CtxBytes(stacked, 1000, true)-CtxBytes(p, 1000, true) != stacked.WindowComputePerToken*1000 {
		t.Fatal("window compute not priced per window token")
	}
}
