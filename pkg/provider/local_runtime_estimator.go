package provider

import (
	"fmt"
	"math"
)

const (
	q8BytesPerElem  = 34.0 / 32.0
	f16BytesPerElem = 2.0
)

// Fraction of layers that are sliding window for architectures with a known SWA pattern
var swaLayerFraction = map[string]float64{"gemma3": 5.0 / 6.0, "gemma2": 1.0 / 2.0}

// Architectures whose loader opens each scalar SWA period with the full attention layer
var swaDenseFirstArchs = map[string]bool{"cohere2moe": true, "modern-bert": true, "smallthinker": true, "laguna": true}

// Per layer SWA flags from a scalar period, one full attention layer per cycle like llama.cpp
func expandSWAPeriod(period, nLayer int64, denseFirst bool) []int64 {
	if period <= 0 {
		return []int64{}
	}
	out := make([]int64, max(0, nLayer))
	for i := range out {
		pos := int64(i) % period
		if denseFirst {
			if pos != 0 {
				out[i] = 1
			}
		} else if pos < period-1 {
			out[i] = 1
		}
	}
	return out
}

const recurrentStatePerLayer = int64(4) << 20

// Compute buffer bytes per microbatch token per window token for whole window scoring attention
var windowComputeBytes = map[string]int64{"qwen4exp": 46}

// The attention kind of one model layer
type LayerKind string

const (
	LayerFull      LayerKind = "full"
	LayerSWA       LayerKind = "swa"
	LayerRecurrent LayerKind = "recurrent"
)

// one layer's kind and f16 KV bytes per token
type ProfileLayer struct {
	Kind          LayerKind
	KVBytesPerTok int64
}

// everything the context policy needs about one model
type ModelProfile struct {
	Name                  string
	WeightsBytes          int64
	EmbdTableBytes        int64
	NCtxTrain             int
	Layers                []ProfileLayer
	SWAWindow             int
	MoE                   bool
	Architecture          string
	NVocab                int64
	KVScale               float64
	FFNBlockBytes         map[int]int64
	LazyBytes             int64
	WindowComputePerToken int64
	RecurrentStateBytes   int64
}

// Profile with the default KV scale of one and an empty FFN block map
func NewModelProfile(name string, weightsBytes, embdTableBytes int64, nCtxTrain int, layers []ProfileLayer) ModelProfile {
	return ModelProfile{Name: name, WeightsBytes: weightsBytes, EmbdTableBytes: embdTableBytes,
		NCtxTrain: nCtxTrain, Layers: layers, KVScale: 1.0, FFNBlockBytes: map[int]int64{}}
}

// Compute buffer bytes per microbatch token per window token for this architecture
func (p ModelProfile) WindowComputeBytes() int64 { return windowComputeBytes[p.Architecture] }

// Uncapped per token KV cost of full and SWA layers
func (p ModelProfile) PerTokenKVF16() int64 {
	var total int64
	for _, l := range p.Layers {
		if l.Kind != LayerRecurrent {
			total += l.KVBytesPerTok
		}
	}
	return total
}

func (p ModelProfile) RecurrentLayerCount() int {
	n := 0
	for _, l := range p.Layers {
		if l.Kind == LayerRecurrent {
			n++
		}
	}
	return n
}

// The memory the physics check budgets against on one machine
type HardwareBudget struct {
	UsableVRAMBytes   int64
	TotalDeviceBytes  int64
	RAMAvailableBytes int64
	UMA               bool
	GPUName           string
	Platform          string
	GPUPCIID          *int64
	LazyReads         bool
	CommitRoomBytes   *int64
}

// Budget with lazy reads on and every optional field at its default
func NewHardwareBudget(usableVRAM, totalDevice, ramAvailable int64) HardwareBudget {
	return HardwareBudget{UsableVRAMBytes: usableVRAM, TotalDeviceBytes: totalDevice,
		RAMAvailableBytes: ramAvailable, LazyReads: true}
}

// Profile as this machine loads it, on demand tensors counted as weights where read up front
func AsLoaded(profile ModelProfile, budget HardwareBudget) ModelProfile {
	if profile.LazyBytes != 0 && !budget.LazyReads {
		profile.WeightsBytes += profile.LazyBytes
		profile.LazyBytes = 0
	}
	return profile
}

// Profile priced from a GGUF header with the file's SWA pattern, known fractions or all full attention
func ProfileFromGGUF(h *GGUFHeader) (ModelProfile, error) {
	var p ModelProfile
	kvHeads, err := h.HeadCountsKV()
	if err != nil {
		return p, err
	}
	dk, err := h.HeadDimK()
	if err != nil {
		return p, err
	}
	dv, err := h.HeadDimV()
	if err != nil {
		return p, err
	}
	dkSWA, err := h.KeyLengthSWA()
	if err != nil {
		return p, err
	}
	if dkSWA == 0 {
		dkSWA = dk
	}
	dvSWA, err := h.ValueLengthSWA()
	if err != nil {
		return p, err
	}
	if dvSWA == 0 {
		dvSWA = dv
	}
	pattern, err := h.SlidingWindowPattern()
	if err != nil {
		return p, err
	}
	arch := h.Architecture()
	if pattern == nil && h.SlidingWindowPatternPeriod() > 0 {
		nLayer, err := h.NLayer()
		if err != nil {
			return p, err
		}
		pattern = expandSWAPeriod(h.SlidingWindowPatternPeriod(), nLayer, swaDenseFirstArchs[arch])
	}
	slidingWindow, err := h.SlidingWindow()
	if err != nil {
		return p, err
	}
	hasPattern := pattern != nil && slidingWindow > 0 && len(pattern) == len(kvHeads)
	swaFraction := swaLayerFraction[arch]
	hasFraction := !hasPattern && slidingWindow > 0 && swaFraction > 0
	nAttnTotal := 0
	for _, heads := range kvHeads {
		if heads > 0 {
			nAttnTotal++
		}
	}
	nSWA := 0
	if hasFraction {
		nSWA = int(math.RoundToEven(float64(nAttnTotal) * swaFraction))
	}
	layers := make([]ProfileLayer, 0, len(kvHeads))
	nAttnSeen := 0
	for i, heads := range kvHeads {
		if heads == 0 {
			layers = append(layers, ProfileLayer{LayerRecurrent, 0})
			continue
		}
		isSWA := nAttnSeen < nSWA
		if hasPattern {
			isSWA = pattern[i] != 0
		}
		layerDK, layerDV := dk, dv
		kind := LayerFull
		if isSWA {
			layerDK, layerDV, kind = dkSWA, dvSWA, LayerSWA
		}
		perToken := int64(math.RoundToEven(float64(heads) * float64(layerDK+layerDV) * f16BytesPerElem))
		layers = append(layers, ProfileLayer{kind, perToken})
		nAttnSeen++
	}
	nCtxTrain, err := h.NCtxTrain()
	if err != nil {
		return p, err
	}
	expertCount, err := h.ExpertCount()
	if err != nil {
		return p, err
	}
	nVocab, err := h.NVocab()
	if err != nil {
		return p, err
	}
	recurrentState, err := h.RecurrentStateBytes()
	if err != nil {
		return p, err
	}
	ffn := make(map[int]int64, len(h.FFNBlockBytes))
	for k, v := range h.FFNBlockBytes {
		ffn[k] = v
	}
	lazy := h.LazyBytes()
	return ModelProfile{
		Name: h.Path, WeightsBytes: h.TensorBytes - lazy, EmbdTableBytes: h.EmbdTableBytes,
		NCtxTrain: int(nCtxTrain), Layers: layers, SWAWindow: int(slidingWindow),
		MoE: expertCount > 0, Architecture: arch, NVocab: nVocab, KVScale: 1.0,
		FFNBlockBytes: ffn, LazyBytes: lazy, RecurrentStateBytes: recurrentState,
	}, nil
}

// KV bytes per f16 element factor, q8_0 with flash attention and f16 without
func KVDtypeFactor(flashAttention bool) float64 {
	if flashAttention {
		return q8BytesPerElem / f16BytesPerElem
	}
	return 1.0
}

// Memory that grows with the window across full, SWA and recurrent layers plus window compute buffers
func CtxBytes(profile ModelProfile, window int, flashAttention bool) int64 {
	factor := KVDtypeFactor(flashAttention)
	total := 0.0
	for _, l := range profile.Layers {
		switch l.Kind {
		case LayerRecurrent:
			total += float64(recurrentStatePerLayer)
		case LayerSWA:
			total += float64(l.KVBytesPerTok) * factor * float64(min(window, profile.SWAWindow))
		default:
			total += float64(l.KVBytesPerTok) * factor * float64(window)
		}
	}
	return int64(total*profile.KVScale) + profile.WindowComputePerToken*int64(window)
}

// The only true refusal, weights plus floor KV exceeding VRAM plus RAM
type PhysicsRefusal struct {
	NeededBytes    int64
	AvailableBytes int64
	Message        string
}

func (*PhysicsRefusal) windowOutcome() {}

// Complete estimated footprint of weights, window memory and runtime overhead
func FootprintBytes(profile ModelProfile, window int, flashAttention bool, overheadBytes int64) int64 {
	return profile.WeightsBytes + CtxBytes(profile, window, flashAttention) + max(0, overheadBytes)
}

// Refusal when the model at the floor window exceeds VRAM plus RAM, nil when it fits
func PhysicsCheck(profile ModelProfile, budget HardwareBudget, floor int, flashAttention bool, overheadBytes int64) *PhysicsRefusal {
	window := floor
	if profile.NCtxTrain != 0 {
		window = min(floor, profile.NCtxTrain)
	}
	needed := FootprintBytes(profile, window, flashAttention, overheadBytes)
	available := budget.UsableVRAMBytes + budget.RAMAvailableBytes
	if needed <= available {
		return nil
	}
	gib := float64(int64(1) << 30)
	return &PhysicsRefusal{
		NeededBytes: needed, AvailableBytes: available,
		Message: fmt.Sprintf("%s: needs ~%.1f GiB at the %dK floor but only ~%.1f GiB of VRAM+RAM are available — try a smaller model or a supported smaller quant",
			profile.Name, float64(needed)/gib, pyFloorDiv(int64(floor), 1024), float64(available)/gib),
	}
}
