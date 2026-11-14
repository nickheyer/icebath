package local

import (
	"fmt"
	"math"
)

// Bytes per key value element under q8_0 and under f16
const (
	q8BytesPerElem  = 34.0 / 32.0
	f16BytesPerElem = 2.0
	// Allowance for the state of one recurrent layer and one sequence
	recurrentStatePerLayer = 4 << 20
)

// Share of sliding window layers for architectures older files describe without a per layer pattern
var swaLayerFraction = map[string]float64{"gemma3": 5.0 / 6.0, "gemma2": 1.0 / 2.0}

// Architectures whose loader opens each period with the full attention layer
var swaDenseFirstArchs = map[string]bool{"cohere2moe": true, "modern-bert": true, "smallthinker": true, "laguna": true}

// Compute buffer bytes per microbatch token per window token and context, by architecture
var windowComputeBytes = map[string]int64{"qwen4exp": 46}

// How one layer keeps its attention state
type LayerKind uint8

// Full attention, sliding window attention, and recurrent state without a cache
const (
	LayerFull LayerKind = iota + 1
	LayerSWA
	LayerRecurrent
)

// One layer with its key value bytes per token at f16
type Layer struct {
	Kind     LayerKind
	PerToken int64
}

// Everything the window policy needs to know about a model
type ModelProfile struct {
	Name string
	// Weights the engine loads when it reads the lazy tensors from disk on demand
	WeightsBytes   int64
	EmbdTableBytes int64
	NCtxTrain      int
	Layers         []Layer
	SWAWindow      int
	MoE            bool
	Architecture   string
	NVocab         int
	// Context cost multiplier, above one when a draft context runs beside the main one
	KVScale float64
	// Feed forward weight bytes by block index, empty when unknown
	FFNBlockBytes map[int]int64
	// Bytes of tensors the engine can read from disk on demand
	LazyBytes int64
	// Compute buffer bytes per window token at the launch posture
	WindowComputePerToken int64
	// Bytes of one copy of one sequence's recurrent state, zero when unknown
	RecurrentStateBytes int64
}

// Compute buffer bytes per microbatch token per window token of the architecture
func (p *ModelProfile) WindowComputeBytes() int64 { return windowComputeBytes[p.Architecture] }

// Key value bytes per token at f16 over every attention layer, uncapped
func (p *ModelProfile) PerTokenKVF16() int64 {
	var total int64
	for _, l := range p.Layers {
		if l.Kind != LayerRecurrent {
			total += l.PerToken
		}
	}
	return total
}

// Number of layers that keep recurrent state
func (p *ModelProfile) RecurrentLayerCount() int {
	n := 0
	for _, l := range p.Layers {
		if l.Kind == LayerRecurrent {
			n++
		}
	}
	return n
}

// Memory a model may be priced against
type HardwareBudget struct {
	// Free device memory on a discrete card, a derived figure on unified memory
	UsableVRAMBytes   int64
	TotalDeviceBytes  int64
	RAMAvailableBytes int64
	UMA               bool
	GPUName           string
	// Operating system family of the machine being priced
	Platform string
	// Packed PCI device and vendor id, zero when unknown
	GPUPCIID uint32
	// False where the engine loads lazy tensors up front
	LazyReads bool
	// Commit the server may still take on Windows unified memory, nil everywhere else
	CommitRoomBytes *int64
}

// Profile as this machine loads it, lazy tensors counted as weights where the engine reads them up front
func AsLoaded(p ModelProfile, b HardwareBudget) ModelProfile {
	if p.LazyBytes != 0 && !b.LazyReads {
		p.WeightsBytes += p.LazyBytes
		p.LazyBytes = 0
	}
	return p
}

// Per layer sliding window flags from a scalar period, one full attention layer per cycle
func expandSWAPeriod(period, layers int, denseFirst bool) []int {
	if period <= 0 {
		return []int{}
	}
	out := make([]int, max(0, layers))
	for i := range out {
		if denseFirst {
			if i%period != 0 {
				out[i] = 1
			}
		} else if i%period < period-1 {
			out[i] = 1
		}
	}
	return out
}

// Profile of a model from its file header
func ProfileFromGGUF(h *GGUFHeader) ModelProfile {
	kvHeads := h.HeadCountsKV()
	dk, dv := h.HeadDimK(), h.HeadDimV()
	dkSWA, dvSWA := h.KeyLengthSWA(), h.ValueLengthSWA()
	if dkSWA == 0 {
		dkSWA = dk
	}
	if dvSWA == 0 {
		dvSWA = dv
	}
	pattern := h.SlidingWindowPattern()
	if pattern == nil && h.SlidingWindowPatternPeriod() > 0 {
		pattern = expandSWAPeriod(h.SlidingWindowPatternPeriod(), h.NLayer(), swaDenseFirstArchs[h.Architecture()])
	}
	hasPattern := pattern != nil && h.SlidingWindow() > 0 && len(pattern) == len(kvHeads)
	fraction := swaLayerFraction[h.Architecture()]
	hasFraction := !hasPattern && h.SlidingWindow() > 0 && fraction > 0
	attention := 0
	for _, heads := range kvHeads {
		if heads > 0 {
			attention++
		}
	}
	swaLayers := 0
	if hasFraction {
		swaLayers = int(math.RoundToEven(float64(attention) * fraction))
	}
	layers := make([]Layer, 0, len(kvHeads))
	seen := 0
	for i, heads := range kvHeads {
		if heads == 0 {
			layers = append(layers, Layer{Kind: LayerRecurrent})
			continue
		}
		swa := seen < swaLayers
		if hasPattern {
			swa = pattern[i] != 0
		}
		layerDK, layerDV, kind := dk, dv, LayerFull
		if swa {
			layerDK, layerDV, kind = dkSWA, dvSWA, LayerSWA
		}
		layers = append(layers, Layer{Kind: kind, PerToken: int64(math.RoundToEven(float64(heads*(layerDK+layerDV)) * f16BytesPerElem))})
		seen++
	}
	ffn := make(map[int]int64, len(h.FFNBlockBytes))
	for block, nbytes := range h.FFNBlockBytes {
		ffn[block] = nbytes
	}
	lazy := h.LazyBytes()
	return ModelProfile{
		Name: h.Path, WeightsBytes: h.TensorBytes - lazy, EmbdTableBytes: h.EmbdTableBytes,
		NCtxTrain: h.NCtxTrain(), Layers: layers, SWAWindow: h.SlidingWindow(),
		MoE: h.ExpertCount() > 0, Architecture: h.Architecture(), NVocab: h.NVocab(), KVScale: 1,
		FFNBlockBytes: ffn, LazyBytes: lazy, RecurrentStateBytes: h.RecurrentStateBytes(),
	}
}

// Size of the cache element type relative to f16, q8_0 under flash attention
func KVDtypeFactor(flashAttention bool) float64 {
	if flashAttention {
		return q8BytesPerElem / f16BytesPerElem
	}
	return 1
}

// Memory that grows with the window, sliding layers capped at their window and recurrent layers constant
func CtxBytes(p ModelProfile, window int, flashAttention bool) int64 {
	factor := KVDtypeFactor(flashAttention)
	total := 0.0
	for _, l := range p.Layers {
		switch l.Kind {
		case LayerRecurrent:
			total += recurrentStatePerLayer
		case LayerSWA:
			total += float64(l.PerToken) * factor * float64(min(window, p.SWAWindow))
		default:
			total += float64(l.PerToken) * factor * float64(window)
		}
	}
	return int64(total*p.KVScale) + p.WindowComputePerToken*int64(window)
}

// The one true refusal, a model whose weights and floor context exceed device memory and RAM together
type PhysicsRefusal struct {
	NeededBytes    int64
	AvailableBytes int64
	Message        string
}

// Message naming the model, what it needs and what the machine has
func (r *PhysicsRefusal) Error() string { return r.Message }

// Whole estimated footprint at a window, the budget already excluding its reserve
func FootprintBytes(p ModelProfile, window int, flashAttention bool, overhead int64) int64 {
	return p.WeightsBytes + CtxBytes(p, window, flashAttention) + max(0, overhead)
}

// Refusal when the model cannot run at the floor window on this budget, nil when it can
func PhysicsCheck(p ModelProfile, b HardwareBudget, floor int, flashAttention bool, overhead int64) *PhysicsRefusal {
	window := floor
	if p.NCtxTrain != 0 {
		window = min(floor, p.NCtxTrain)
	}
	needed := FootprintBytes(p, window, flashAttention, overhead)
	available := b.UsableVRAMBytes + b.RAMAvailableBytes
	if needed <= available {
		return nil
	}
	const gib = float64(1 << 30)
	return &PhysicsRefusal{NeededBytes: needed, AvailableBytes: available, Message: fmt.Sprintf(
		"%s: needs ~%.1f GiB at the %dK floor but only ~%.1f GiB of VRAM+RAM are available — try a smaller model or a supported smaller quant",
		p.Name, float64(needed)/gib, floor/1024, float64(available)/gib)}
}
