package local

import (
	"fmt"
	"strconv"
	"strings"
)

// Window guarantee, growth pacing and launch microbatch sizes, fixed values no config key changes
const (
	Floor                = 64 * 1024
	ladderGrowth         = 1.5
	growAtOccupancy      = 0.85
	SpeedFloorTokS       = 6.0
	earlyCostCtxFraction = 0.15
	TargetWindow         = 144 * 1024
	DefaultUbatch        = 512
	PrefillUbatch        = 2048
)

// Load cost beyond weights and cache at the default microbatch, one and a half GiB
const RuntimeOverheadBytes int64 = 3 << 29

// Window rungs from 64K growing by half up to native, native always last
func Ladder(native int) []int {
	var rungs []int
	for step := float64(Floor); step < float64(native); step *= ladderGrowth {
		rungs = append(rungs, int(step))
	}
	return append(rungs, native)
}

// Window a model launches or regrows with and what that costs in placement
type WindowDecision struct {
	Window int
	// Weight bytes displaced to the host at this window
	SpillBytes int64
	KVOnGPU    bool
	Reasons    []string
}

// Reports weights displaced to the host
func (d WindowDecision) Spilled() bool { return d.SpillBytes > 0 }

// Launch choice, the largest cheap rung and never below the guarantee, or the refusal when physics forbids the model
func InitialWindow(p ModelProfile, b HardwareBudget, flashAttention bool, overhead int64) (*WindowDecision, *PhysicsRefusal) {
	if refusal := PhysicsCheck(p, b, Floor, flashAttention, overhead); refusal != nil {
		return nil, refusal
	}
	native := p.NCtxTrain
	if native == 0 {
		native = Floor
	}
	rungs := Ladder(native)
	kv := func(rung int) int64 { return CtxBytes(p, rung, flashAttention) }
	need := func(rung int) int64 { return FootprintBytes(p, rung, flashAttention, overhead) }
	best, found := 0, false
	for _, rung := range rungs {
		if need(rung) > b.UsableVRAMBytes {
			break
		}
		best, found = rung, true
	}
	var window int
	var reason string
	if found && best >= min(Floor, native) {
		window = best
		reason = fmt.Sprintf("largest zero-spill rung (%dK)", window/1024)
	} else {
		limit := int64(float64(b.UsableVRAMBytes) * earlyCostCtxFraction)
		window = min(Floor, native)
		for _, rung := range rungs {
			if rung < window {
				continue
			}
			if kv(rung) > limit || need(rung) > b.UsableVRAMBytes+b.RAMAvailableBytes {
				break
			}
			window = rung
		}
		reason = fmt.Sprintf("floor held at %dK; weights spill (deliberate price of the guarantee)", window/1024)
	}
	return &WindowDecision{
		Window: window, Reasons: []string{reason},
		SpillBytes: max(0, need(window)-b.UsableVRAMBytes),
		KVOnGPU:    kv(window)+overhead <= b.UsableVRAMBytes,
	}, nil
}

// Outcome of planning one launch, exactly one of Decision and Refusal set
type LaunchPlan struct {
	Decision *WindowDecision
	Refusal  *PhysicsRefusal
	// True for the large microbatch beside speculative decoding
	MTPPrefill    bool
	OverheadBytes int64
	// Model as priced at the chosen posture
	Profile ModelProfile
}

// Model priced at one posture, compute buffers scaled by its microbatch and context count
func PostureProfile(p ModelProfile, mtpCapable, mtpPrefill bool) ModelProfile {
	ubatch, contexts := int64(DefaultUbatch), int64(1)
	if mtpPrefill || !mtpCapable {
		ubatch = PrefillUbatch
	}
	if mtpCapable {
		contexts = 2
	}
	p.WindowComputePerToken = p.WindowComputeBytes() * ubatch * contexts
	return p
}

// What a launch plan is asked for
type PlanOptions struct {
	// Model carries a speculative decoding head
	MTPCapable bool
	// Load cost beyond weights and cache, RuntimeOverheadBytes plus any projector bytes
	FixedOverhead int64
	// Window an earlier growth granted, zero for none
	RequestedWindow int
}

// Plans window first and prefill second, pricing the lean and the stacked posture at the effective window
func PlanLaunch(p ModelProfile, b HardwareBudget, o PlanOptions) LaunchPlan {
	if o.MTPCapable && p.KVScale == 1.0 {
		p.KVScale = 1.2
	}
	initial := map[bool]*WindowDecision{}
	candidate := func(stacked bool) LaunchPlan {
		posture := PostureProfile(p, o.MTPCapable, stacked)
		overhead := o.FixedOverhead + UbLogitsBytes(p.NVocab, o.MTPCapable, stacked)
		decision, refusal := InitialWindow(posture, b, true, overhead)
		initial[stacked] = decision
		if decision != nil && o.RequestedWindow != 0 {
			target := o.RequestedWindow
			if p.NCtxTrain != 0 {
				target = min(target, p.NCtxTrain)
			}
			if target > decision.Window && PhysicsCheck(posture, b, target, true, overhead) == nil {
				need := FootprintBytes(posture, target, true, overhead)
				decision = &WindowDecision{
					Window: target, SpillBytes: max(0, need-b.UsableVRAMBytes),
					KVOnGPU: CtxBytes(posture, target, true)+overhead <= b.UsableVRAMBytes,
					Reasons: []string{fmt.Sprintf("grown window restored (%dK)", target/1024)},
				}
			}
		}
		return LaunchPlan{Decision: decision, Refusal: refusal, MTPPrefill: stacked, OverheadBytes: overhead, Profile: posture}
	}
	lean := candidate(false)
	if !o.MTPCapable {
		return lean
	}
	stacked := candidate(true)
	switch {
	case stacked.Refusal != nil:
		return lean
	case lean.Refusal != nil:
		return stacked
	case stacked.Decision.Window < lean.Decision.Window:
		return lean
	case !stacked.Decision.Spilled():
		return stacked
	}
	stackedInitial, leanInitial := initial[true], initial[false]
	if o.RequestedWindow != 0 && stackedInitial != nil && leanInitial != nil && !stackedInitial.Spilled() &&
		stackedInitial.Window >= leanInitial.Window && stacked.Decision.Window > stackedInitial.Window && lean.Decision.Spilled() {
		return stacked
	}
	return lean
}

// Narrows a resident plan to the window free device memory holds now, never below the guarantee and never moving weights
func FitToFreeMemory(plan LaunchPlan, p ModelProfile, live HardwareBudget, mtpCapable bool, fixedOverhead int64) LaunchPlan {
	decision := plan.Decision
	if decision == nil || decision.Spilled() {
		return plan
	}
	now := PlanLaunch(p, live, PlanOptions{MTPCapable: mtpCapable, FixedOverhead: fixedOverhead})
	if now.Decision != nil && !now.Decision.Spilled() {
		if now.Decision.Window >= decision.Window {
			return plan
		}
		narrowed := *now.Decision
		narrowed.Reasons = []string{fmt.Sprintf("%dK fits beside other programs' GPU memory (%dK would not)", narrowed.Window/1024, decision.Window/1024)}
		return LaunchPlan{Decision: &narrowed, MTPPrefill: now.MTPPrefill, OverheadBytes: now.OverheadBytes, Profile: now.Profile}
	}
	floor := Floor
	if p.NCtxTrain != 0 {
		floor = min(Floor, p.NCtxTrain)
	}
	if decision.Window <= floor {
		return plan
	}
	return LaunchPlan{
		Decision: &WindowDecision{Window: floor, KVOnGPU: true, Reasons: []string{
			fmt.Sprintf("floor held at %dK; other programs hold most of the GPU memory", floor/1024)}},
		MTPPrefill: plan.MTPPrefill, OverheadBytes: plan.OverheadBytes, Profile: plan.Profile,
	}
}

// What one growth evaluation concludes
type GrowthAction string

// Grant the next rung, keep the window, or leave the session to compression
const (
	Grow            GrowthAction = "grow"
	Hold            GrowthAction = "hold"
	CompressDefault GrowthAction = "compress-default"
)

// Verdict of one growth evaluation, NextWindow set only for a grant
type GrowthDecision struct {
	Action     GrowthAction
	NextWindow int
	Reason     string
}

// Facts one growth evaluation reads at the end of a turn
type GrowthInput struct {
	CurrentWindow int
	SessionTokens int
	// Decode speed measured this session, nil when none was measured
	MeasuredDecodeTokS *float64
	ServerIdle         bool
	FlashAttention     bool
	// Skips the occupancy gate when the caller's own compression gate already fired
	OccupancyConfirmed bool
}

// Evaluates growth by occupancy, native cap, idleness, speed and a refit against live free memory, in that order
func DecideGrowth(p ModelProfile, b HardwareBudget, in GrowthInput) GrowthDecision {
	if !in.OccupancyConfirmed && float64(in.SessionTokens) < float64(in.CurrentWindow)*growAtOccupancy {
		return GrowthDecision{Action: Hold, Reason: "session below growth occupancy"}
	}
	native := p.NCtxTrain
	if native == 0 {
		native = in.CurrentWindow
	}
	if in.CurrentWindow >= native {
		return GrowthDecision{Action: CompressDefault, Reason: "at native window; compression is the only move"}
	}
	if !in.ServerIdle {
		return GrowthDecision{Action: Hold, Reason: "server busy; re-grant deferred to idle"}
	}
	if in.MeasuredDecodeTokS != nil && *in.MeasuredDecodeTokS < SpeedFloorTokS {
		return GrowthDecision{Action: CompressDefault, Reason: fmt.Sprintf(
			"decode %.1f tok/s below the ~%.0f tok/s floor; growth is now an explicit per-session choice",
			*in.MeasuredDecodeTokS, SpeedFloorTokS)}
	}
	next := native
	for _, rung := range Ladder(native) {
		if rung > in.CurrentWindow {
			next = rung
			break
		}
	}
	if p.WeightsBytes+CtxBytes(p, next, in.FlashAttention) > b.UsableVRAMBytes+b.RAMAvailableBytes {
		return GrowthDecision{Action: CompressDefault, Reason: "next rung exceeds physics; compression instead"}
	}
	return GrowthDecision{Action: Grow, NextWindow: next, Reason: fmt.Sprintf("rung %dK -> %dK", in.CurrentWindow/1024, next/1024)}
}

// Recurrent block indices whose feed forward weights cover spill, lowest first, every such block when spill is nil or sizes are unknown
func RecurrentSpillBlocks(p ModelProfile, spill *int64) []int {
	var recurrent []int
	known := true
	for i, l := range p.Layers {
		if l.Kind == LayerRecurrent {
			recurrent = append(recurrent, i)
			if _, ok := p.FFNBlockBytes[i]; !ok {
				known = false
			}
		}
	}
	if spill == nil || !known {
		return recurrent
	}
	var chosen []int
	var covered int64
	for _, i := range recurrent {
		if covered >= *spill {
			break
		}
		chosen = append(chosen, i)
		covered += p.FFNBlockBytes[i]
	}
	return chosen
}

// Tensor placement flags of a spilled launch, expert or recurrent feed forward weights sent to the host
func SpillOverrides(p ModelProfile, spill *int64) []string {
	if p.MoE {
		return []string{"-ot", `blk\.\d+\.ffn_.*_exps\.weight=CPU`}
	}
	blocks := RecurrentSpillBlocks(p, spill)
	if len(blocks) == 0 {
		return nil
	}
	names := make([]string, len(blocks))
	for i, block := range blocks {
		names[i] = strconv.Itoa(block)
	}
	return []string{"-ot", `blk\.(` + strings.Join(names, "|") + `)\.ffn_.*\.weight=CPU`}
}

// Posture a launch command line is written for
type LaunchOptions struct {
	FlashAttention bool
	MTPCapable     bool
	// Draft tokens per speculative step, three when zero
	MTPDraftDepth int
	UMA           bool
	MTPPrefill    bool
}

// Server flags of one model from its window decision, placement overrides on discrete cards only
func LaunchArgs(p ModelProfile, d WindowDecision, o LaunchOptions) []string {
	args := []string{"-c", strconv.Itoa(d.Window)}
	if o.MTPCapable {
		depth := o.MTPDraftDepth
		if depth == 0 {
			depth = 3
		}
		args = append(args, "--spec-type", "draft-mtp", "--spec-draft-n-max", strconv.Itoa(depth),
			"--backend-sampling", "--spec-draft-backend-sampling")
		if o.MTPPrefill {
			args = append(args, "-b", "4096", "-ub", strconv.Itoa(PrefillUbatch))
		}
	} else {
		args = append(args, "-b", strconv.Itoa(PrefillUbatch), "-ub", strconv.Itoa(PrefillUbatch))
	}
	if o.FlashAttention {
		args = append(args, "-ctk", "q8_0", "-ctv", "q8_0", "-fa", "on")
	}
	if d.Spilled() && !o.UMA {
		spill := d.SpillBytes
		args = append(args, SpillOverrides(p, &spill)...)
	}
	return args
}

// Device logits buffer bytes of the microbatch posture, priced from the vocabulary size
func UbLogitsBytes(nVocab int, mtpCapable, mtpPrefill bool) int64 {
	v := int64(max(0, nVocab))
	switch {
	case mtpCapable && mtpPrefill:
		return int64(float64(2048*v*4) * 1.5)
	case mtpCapable:
		return 512 * v * 4 * 2
	}
	return 2048 * v * 4
}
