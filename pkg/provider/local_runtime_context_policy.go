package provider

import (
	"fmt"
	"strconv"
	"strings"
)

// The guaranteed and target window of every managed local model
const ContextFloor = 64 * 1024

const (
	ladderGrowth          = 1.5
	growAtOccupancy       = 0.85
	earlyCostCtxFraction  = 0.15
	SpeedFloorTokS        = 6.0
	TargetWindow          = 144 * 1024
	RuntimeOverheadBytes  = int64(1.5 * (1 << 30))
	DefaultUbatch         = 512
	PrefillUbatch         = 2048
	moeSpillOverride      = `blk\.\d+\.ffn_.*_exps\.weight=CPU`
	recurrentSpillPattern = `blk\.(%s)\.ffn_.*\.weight=CPU`
)

// The speculative draft depth launch flags use unless told otherwise
const DefaultMTPDraftDepth = 3

// Growth verdict actions
const (
	GrowthActionGrow     = "grow"
	GrowthActionHold     = "hold"
	GrowthActionCompress = "compress-default"
)

// Window rungs from the floor growing by half each step, native always last
func Ladder(native int) []int {
	var rungs []int
	step := float64(ContextFloor)
	for step < float64(native) {
		rungs = append(rungs, int(step))
		step *= ladderGrowth
	}
	return append(rungs, native)
}

// A launch window decision or a physics refusal
type WindowOutcome interface{ windowOutcome() }

// The chosen window, the weights it displaces to host and KV residency
type WindowDecision struct {
	Window     int
	SpillBytes int64
	KVOnGPU    bool
	Reasons    []string
}

func (*WindowDecision) windowOutcome() {}

func (d *WindowDecision) Spilled() bool { return d.SpillBytes > 0 }

// Launch window as the largest zero spill rung, else the floor with bounded early context cost
func InitialWindow(profile ModelProfile, budget HardwareBudget, flashAttention bool, overheadBytes int64) WindowOutcome {
	if refusal := PhysicsCheck(profile, budget, ContextFloor, flashAttention, overheadBytes); refusal != nil {
		return refusal
	}
	native := profile.NCtxTrain
	if native == 0 {
		native = ContextFloor
	}
	rungs := Ladder(native)
	kv := func(rung int) int64 { return CtxBytes(profile, rung, flashAttention) }
	need := func(rung int) int64 { return FootprintBytes(profile, rung, flashAttention, overheadBytes) }

	bestZeroSpill, haveZeroSpill := 0, false
	for _, rung := range rungs {
		if need(rung) > budget.UsableVRAMBytes {
			break
		}
		bestZeroSpill, haveZeroSpill = rung, true
	}
	var window int
	var reason string
	if haveZeroSpill && bestZeroSpill >= min(ContextFloor, native) {
		window = bestZeroSpill
		reason = fmt.Sprintf("largest zero-spill rung (%dK)", pyFloorDiv(int64(window), 1024))
	} else {
		limit := int64(float64(budget.UsableVRAMBytes) * earlyCostCtxFraction)
		window = min(ContextFloor, native)
		for _, rung := range rungs {
			if rung < window {
				continue
			}
			if kv(rung) > limit || need(rung) > budget.UsableVRAMBytes+budget.RAMAvailableBytes {
				break
			}
			window = rung
		}
		reason = fmt.Sprintf("floor held at %dK; weights spill (deliberate price of the guarantee)", pyFloorDiv(int64(window), 1024))
	}
	kvBytes := kv(window)
	return &WindowDecision{Window: window, Reasons: []string{reason},
		SpillBytes: max(0, need(window)-budget.UsableVRAMBytes),
		KVOnGPU:    kvBytes+overheadBytes <= budget.UsableVRAMBytes}
}

// A window outcome with its prefill posture, overhead and the profile it priced
type LaunchPlan struct {
	Decision      WindowOutcome
	MTPPrefill    bool
	OverheadBytes int64
	Profile       *ModelProfile
}

// Profile at one launch posture with window compute buffers scaled by microbatch and context count
func PostureProfile(profile ModelProfile, mtpCapable, mtpPrefill bool) ModelProfile {
	ubatch := int64(DefaultUbatch)
	if mtpPrefill || !mtpCapable {
		ubatch = PrefillUbatch
	}
	contexts := int64(1)
	if mtpCapable {
		contexts = 2
	}
	profile.WindowComputePerToken = profile.WindowComputeBytes() * ubatch * contexts
	return profile
}

// Window first then prefill posture, both priced at the effective window with a restored window honoured
func PlanLaunch(profile ModelProfile, budget HardwareBudget, mtpCapable bool, fixedOverhead int64, requestedWindow int) LaunchPlan {
	if mtpCapable && profile.KVScale == 1.0 {
		profile.KVScale = 1.2
	}
	initial := map[bool]WindowOutcome{}
	candidate := func(stacked bool) LaunchPlan {
		posture := PostureProfile(profile, mtpCapable, stacked)
		overhead := fixedOverhead + UbLogitsBytes(profile.NVocab, mtpCapable, stacked)
		decision := InitialWindow(posture, budget, true, overhead)
		initial[stacked] = decision
		if d, ok := decision.(*WindowDecision); ok && requestedWindow != 0 {
			target := requestedWindow
			if profile.NCtxTrain != 0 {
				target = min(requestedWindow, profile.NCtxTrain)
			}
			if target > d.Window && PhysicsCheck(posture, budget, target, true, overhead) == nil {
				need := FootprintBytes(posture, target, true, overhead)
				decision = &WindowDecision{Window: target,
					SpillBytes: max(0, need-budget.UsableVRAMBytes),
					KVOnGPU:    CtxBytes(posture, target, true)+overhead <= budget.UsableVRAMBytes,
					Reasons:    []string{fmt.Sprintf("grown window restored (%dK)", pyFloorDiv(int64(target), 1024))}}
			}
		}
		return LaunchPlan{decision, stacked, overhead, &posture}
	}
	lean := candidate(false)
	if !mtpCapable {
		return lean
	}
	stacked := candidate(true)
	stackedDecision, stackedOK := stacked.Decision.(*WindowDecision)
	if !stackedOK {
		return lean
	}
	leanDecision, leanOK := lean.Decision.(*WindowDecision)
	if !leanOK {
		return stacked
	}
	if stackedDecision.Window < leanDecision.Window {
		return lean
	}
	if !stackedDecision.Spilled() {
		return stacked
	}
	stackedInitial, sOK := initial[true].(*WindowDecision)
	leanInitial, lOK := initial[false].(*WindowDecision)
	if requestedWindow != 0 && sOK && lOK && !stackedInitial.Spilled() &&
		stackedInitial.Window >= leanInitial.Window &&
		stackedDecision.Window > stackedInitial.Window && leanDecision.Spilled() {
		return stacked
	}
	return lean
}

// Resident plan narrowed to the window free memory holds now, never below the floor
func FitToFreeMemory(plan LaunchPlan, profile ModelProfile, live HardwareBudget, mtpCapable bool, fixedOverhead int64) LaunchPlan {
	decision, ok := plan.Decision.(*WindowDecision)
	if !ok || decision.Spilled() {
		return plan
	}
	now := PlanLaunch(profile, live, mtpCapable, fixedOverhead, 0)
	if nd, ok := now.Decision.(*WindowDecision); ok && !nd.Spilled() {
		if nd.Window >= decision.Window {
			return plan
		}
		narrowed := *nd
		narrowed.Reasons = []string{fmt.Sprintf("%dK fits beside other programs' GPU memory (%dK would not)",
			pyFloorDiv(int64(nd.Window), 1024), pyFloorDiv(int64(decision.Window), 1024))}
		return LaunchPlan{&narrowed, now.MTPPrefill, now.OverheadBytes, now.Profile}
	}
	floor := ContextFloor
	if profile.NCtxTrain != 0 {
		floor = min(ContextFloor, profile.NCtxTrain)
	}
	if decision.Window <= floor {
		return plan
	}
	return LaunchPlan{&WindowDecision{Window: floor, SpillBytes: 0, KVOnGPU: true, Reasons: []string{
		fmt.Sprintf("floor held at %dK; other programs hold most of the GPU memory", pyFloorDiv(int64(floor), 1024))}},
		plan.MTPPrefill, plan.OverheadBytes, plan.Profile}
}

// One end of turn verdict to grow to the next window, hold or compress
type GrowthDecision struct {
	Action     string
	NextWindow int
	Reason     string
}

// End of turn growth verdict gated on occupancy, native cap, idleness, speed floor and live memory
func DecideGrowth(profile ModelProfile, budget HardwareBudget, currentWindow, sessionTokens int,
	measuredDecodeTokS *float64, serverIdle, flashAttention, occupancyConfirmed bool) GrowthDecision {
	if !occupancyConfirmed && float64(sessionTokens) < float64(currentWindow)*growAtOccupancy {
		return GrowthDecision{Action: GrowthActionHold, Reason: "session below growth occupancy"}
	}
	native := profile.NCtxTrain
	if native == 0 {
		native = currentWindow
	}
	if currentWindow >= native {
		return GrowthDecision{Action: GrowthActionCompress, Reason: "at native window; compression is the only move"}
	}
	if !serverIdle {
		return GrowthDecision{Action: GrowthActionHold, Reason: "server busy; re-grant deferred to idle"}
	}
	if measuredDecodeTokS != nil && *measuredDecodeTokS < SpeedFloorTokS {
		return GrowthDecision{Action: GrowthActionCompress, Reason: fmt.Sprintf(
			"decode %.1f tok/s below the ~%.0f tok/s floor; growth is now an explicit per-session choice",
			*measuredDecodeTokS, SpeedFloorTokS)}
	}
	nextRung := native
	for _, r := range Ladder(native) {
		if r > currentWindow {
			nextRung = r
			break
		}
	}
	kv := CtxBytes(profile, nextRung, flashAttention)
	if profile.WeightsBytes+kv > budget.UsableVRAMBytes+budget.RAMAvailableBytes {
		return GrowthDecision{Action: GrowthActionCompress, Reason: "next rung exceeds physics; compression instead"}
	}
	return GrowthDecision{Action: GrowthActionGrow, NextWindow: nextRung,
		Reason: fmt.Sprintf("rung %dK -> %dK", pyFloorDiv(int64(currentWindow), 1024), pyFloorDiv(int64(nextRung), 1024))}
}

// Fewest lowest recurrent blocks whose FFN bytes cover the spill, all of them when unknown or short
func RecurrentSpillBlocks(profile ModelProfile, spillBytes *int64) []int {
	recurrent := []int{}
	for i, l := range profile.Layers {
		if l.Kind == LayerRecurrent {
			recurrent = append(recurrent, i)
		}
	}
	if spillBytes == nil {
		return recurrent
	}
	for _, i := range recurrent {
		if _, ok := profile.FFNBlockBytes[i]; !ok {
			return recurrent
		}
	}
	chosen := []int{}
	var covered int64
	for _, i := range recurrent {
		if covered >= *spillBytes {
			break
		}
		chosen = append(chosen, i)
		covered += profile.FFNBlockBytes[i]
	}
	return chosen
}

// Override flags moving expert or recurrent block FFN weights to host so attention and KV stay on GPU
func SpillOverrides(profile ModelProfile, spillBytes *int64) []string {
	if profile.MoE {
		return []string{"-ot", moeSpillOverride}
	}
	blocks := RecurrentSpillBlocks(profile, spillBytes)
	if len(blocks) > 0 {
		ids := make([]string, len(blocks))
		for i, b := range blocks {
			ids[i] = strconv.Itoa(b)
		}
		return []string{"-ot", fmt.Sprintf(recurrentSpillPattern, strings.Join(ids, "|"))}
	}
	return []string{}
}

// Per model llama-server flags for a window decision with q8 KV under flash attention and discrete spill placement
func LaunchArgs(profile ModelProfile, decision *WindowDecision, flashAttention, mtpCapable bool,
	mtpDraftDepth int, uma, mtpPrefill bool) []string {
	args := []string{"-c", strconv.Itoa(decision.Window)}
	if mtpCapable {
		args = append(args, "--spec-type", "draft-mtp", "--spec-draft-n-max", strconv.Itoa(mtpDraftDepth),
			"--backend-sampling", "--spec-draft-backend-sampling")
		if mtpPrefill {
			args = append(args, "-b", "4096", "-ub", strconv.Itoa(PrefillUbatch))
		}
	} else {
		args = append(args, "-b", strconv.Itoa(PrefillUbatch), "-ub", strconv.Itoa(PrefillUbatch))
	}
	if flashAttention {
		args = append(args, "-ctk", "q8_0", "-ctv", "q8_0", "-fa", "on")
	}
	if decision.Spilled() && !uma {
		spill := decision.SpillBytes
		args = append(args, SpillOverrides(profile, &spill)...)
	}
	return args
}

// GPU logits buffer bytes of the microbatch posture launch flags choose, priced from the vocab
func UbLogitsBytes(nVocab int64, mtpCapable, mtpPrefill bool) int64 {
	v := max(0, nVocab)
	if mtpCapable && mtpPrefill {
		return int64(float64(2048*v*4) * 1.5)
	}
	if mtpCapable {
		return 512 * v * 4 * 2
	}
	return 2048 * v * 4
}
