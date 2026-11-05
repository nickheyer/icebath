package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const lrtSWAArch = "gemma4"

func lrtSWAHeader(extra map[string]any) *GGUFHeader {
	meta := map[string]any{
		"general.architecture":                   lrtSWAArch,
		lrtSWAArch + ".block_count":              int64(6),
		lrtSWAArch + ".context_length":           int64(262144),
		lrtSWAArch + ".attention.head_count_kv":  int64(8),
		lrtSWAArch + ".attention.sliding_window": int64(1024),
		lrtSWAArch + ".attention.key_length":     int64(512),
		lrtSWAArch + ".attention.value_length":   int64(512),
	}
	for k, v := range extra {
		meta[k] = v
	}
	return &GGUFHeader{Path: "test.gguf", Version: 3, Metadata: meta}
}

func lrtKinds(p ModelProfile) []LayerKind {
	out := []LayerKind{}
	for _, l := range p.Layers {
		out = append(out, l.Kind)
	}
	return out
}

func lrtAllKind(p ModelProfile, k LayerKind) bool {
	for _, l := range p.Layers {
		if l.Kind != k {
			return false
		}
	}
	return true
}

func TestLocalRuntimeUnknownArchWithoutPatternFallsBackToFullAttention(t *testing.T) {
	if p := lrtMustProfile(t, lrtSWAHeader(nil)); !lrtAllKind(p, LayerFull) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
}

func TestLocalRuntimeUnknownArchWithPatternIsPricedPerLayer(t *testing.T) {
	p := lrtMustProfile(t, lrtSWAHeader(map[string]any{
		lrtSWAArch + ".attention.sliding_window_pattern": []any{int64(1), int64(1), int64(1), int64(1), int64(1), int64(0)},
		lrtSWAArch + ".attention.key_length_swa":         int64(256),
		lrtSWAArch + ".attention.value_length_swa":       int64(256),
	}))
	want := []LayerKind{LayerSWA, LayerSWA, LayerSWA, LayerSWA, LayerSWA, LayerFull}
	if !reflect.DeepEqual(lrtKinds(p), want) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
	swa, full := p.Layers[0].KVBytesPerTok, p.Layers[5].KVBytesPerTok
	if swa != 8*(256+256)*2 || full != 8*(512+512)*2 {
		t.Fatalf("swa %d full %d", swa, full)
	}
	small := CtxBytes(p, 2048, false)
	large := CtxBytes(p, 65536, false)
	if large-small != full*(65536-2048) {
		t.Fatalf("growth %d", large-small)
	}
}

func lrtSWAFile(t *testing.T, arch string, blocks int, pattern lrtKV) ModelProfile {
	path := filepath.Join(t.TempDir(), "m.gguf")
	meta := []lrtKV{
		{"general.architecture", 8, arch},
		{arch + ".block_count", 4, blocks},
		{arch + ".context_length", 4, 262144},
		{arch + ".attention.head_count_kv", 4, 8},
		{arch + ".attention.sliding_window", 4, 1024},
		{arch + ".attention.key_length", 4, 512},
		{arch + ".attention.value_length", 4, 512},
		pattern,
	}
	if pattern.vtype == 9 && len(pattern.value.(lrtArray).items) == blocks {
		meta = append(meta, lrtKV{arch + ".attention.key_length_swa", 4, 256},
			lrtKV{arch + ".attention.value_length_swa", 4, 256})
	}
	lrtWriteGGUFTyped(t, path, meta, nil, 0)
	return lrtMustProfile(t, lrtMustHeader(t, path))
}

func TestLocalRuntimeBinaryArrayPatternUint8ElementsRoundTrips(t *testing.T) {
	p := lrtSWAFile(t, lrtSWAArch, 6, lrtKV{lrtSWAArch + ".attention.sliding_window_pattern", 9,
		lrtArray{0, []any{1, 1, 1, 1, 1, 0}}})
	want := []LayerKind{LayerSWA, LayerSWA, LayerSWA, LayerSWA, LayerSWA, LayerFull}
	if !reflect.DeepEqual(lrtKinds(p), want) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
}

func TestLocalRuntimeBinaryScalarPeriodDefaultsToDenseFirstFalse(t *testing.T) {
	p := lrtSWAFile(t, lrtSWAArch, 7, lrtKV{lrtSWAArch + ".attention.sliding_window_pattern", 4, 4})
	want := []LayerKind{LayerSWA, LayerSWA, LayerSWA, LayerFull, LayerSWA, LayerSWA, LayerSWA}
	if !reflect.DeepEqual(lrtKinds(p), want) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
}

func TestLocalRuntimeBinaryScalarPeriodHonorsDenseFirstTrueArchitectures(t *testing.T) {
	p := lrtSWAFile(t, "laguna", 7, lrtKV{"laguna.attention.sliding_window_pattern", 4, 4})
	want := []LayerKind{LayerFull, LayerSWA, LayerSWA, LayerSWA, LayerFull, LayerSWA, LayerSWA}
	if !reflect.DeepEqual(lrtKinds(p), want) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
}

func TestLocalRuntimeBinaryLengthMismatchedArrayFallsBackToFullAttention(t *testing.T) {
	p := lrtSWAFile(t, lrtSWAArch, 6, lrtKV{lrtSWAArch + ".attention.sliding_window_pattern", 9,
		lrtArray{7, []any{true, true, true, false}}})
	if !lrtAllKind(p, LayerFull) {
		t.Fatalf("kinds %v", lrtKinds(p))
	}
}

var lrtSplitArch = []any{
	"general.architecture", "llama", "llama.block_count", 12, "llama.context_length", 65536,
	"llama.attention.head_count", 8, "llama.attention.head_count_kv", 2,
	"llama.attention.key_length", 128, "llama.attention.value_length", 128,
}

func lrtSplitKeys(no, count int) []any {
	return []any{"split.no", no, "split.count", count, "split.tensors.count", 0}
}

type lrtSplitSpec struct {
	meta    []any
	tensors []lrtTensor
}

// Writes a split as gguf-split lays it out, only shard one carrying model metadata
func lrtSplit(t *testing.T, dir, stem string, parts []lrtSplitSpec, total int) []string {
	if total == 0 {
		total = len(parts)
	}
	var written []string
	for i, spec := range parts {
		path := filepath.Join(dir, fmt.Sprintf("%s-%05d-of-%05d.gguf", stem, i+1, total))
		meta := spec.meta
		if meta == nil {
			meta = lrtSplitArch
			if i > 0 {
				meta = lrtSplitKeys(i, total)
			}
		}
		lrtWriteGGUF(t, path, meta, spec.tensors)
		written = append(written, path)
	}
	return written
}

func TestLocalRuntimeSplitWeightsAreTheSumAcrossShards(t *testing.T) {
	parts := lrtSplit(t, t.TempDir(), "qwen-flash", []lrtSplitSpec{
		{}, {tensors: []lrtTensor{{"blk.0.attn_q.weight", 32 << 20}}},
		{tensors: []lrtTensor{{"blk.1.attn_q.weight", 64 << 20}}},
	}, 0)
	h := lrtMustHeader(t, parts[0])
	if h.NTensors != 2 || h.TensorBytes != ((32+64)*4)<<20 {
		t.Fatalf("tensors %d bytes %d", h.NTensors, h.TensorBytes)
	}
	if lrtOK(h.NLayer()) != 12 || lrtOK(h.NCtxTrain()) != 65536 {
		t.Fatal("metadata not from part one")
	}
	if lrtMustProfile(t, h).WeightsBytes != h.TensorBytes {
		t.Fatal("weights differ")
	}
	if h.Path != parts[0] {
		t.Fatalf("path %s", h.Path)
	}
}

func TestLocalRuntimeSingleShardModelIsUnchanged(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "single.gguf")
	lrtWriteGGUF(t, gguf, lrtSplitArch, []lrtTensor{{"blk.0.attn_q.weight", 16 << 20}})
	if b := lrtMustHeader(t, gguf).TensorBytes; b != 64<<20 {
		t.Fatalf("bytes %d", b)
	}
}

func TestLocalRuntimeNamingAloneIsNotASplit(t *testing.T) {
	only := filepath.Join(t.TempDir(), "half-00001-of-00003.gguf")
	lrtWriteGGUF(t, only, lrtSplitArch, []lrtTensor{{"blk.0.attn_q.weight", 16 << 20}})
	if b := lrtMustHeader(t, only).TensorBytes; b != 64<<20 {
		t.Fatalf("bytes %d", b)
	}
	if SplitParts(only) != nil {
		t.Fatal("lone part reported as split")
	}
}

func TestLocalRuntimeEmbeddingTableOutsidePartOneIsStillPriced(t *testing.T) {
	parts := lrtSplit(t, t.TempDir(), "embd-split", []lrtSplitSpec{
		{}, {tensors: []lrtTensor{{"token_embd.weight", 8 << 20}}},
	}, 0)
	h := lrtMustHeader(t, parts[0])
	if h.EmbdTableBytes != 32<<20 || h.EmbdTableBytes > h.TensorBytes {
		t.Fatalf("embd %d tensors %d", h.EmbdTableBytes, h.TensorBytes)
	}
}

func TestLocalRuntimeFFNBlocksSpreadAcrossShardsStillPlaceSpill(t *testing.T) {
	hybrid := append(append([]any{}, lrtSplitArch...), "llama.full_attention_interval", 4)
	tensors := []lrtTensor{{"token_embd.weight", 64}}
	for i := 0; i < 12; i++ {
		tensors = append(tensors, lrtTensor{fmt.Sprintf("blk.%d.attn_norm.weight", i), 16},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_norm.weight", i), 16},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_up.weight", i), 256},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_down.weight", i), 240})
	}
	parts := lrtSplit(t, t.TempDir(), "hybrid-split", []lrtSplitSpec{
		{meta: hybrid, tensors: tensors[:1]}, {tensors: tensors[1:37]}, {tensors: tensors[37:]},
	}, 3)
	p := lrtMustProfile(t, lrtMustHeader(t, parts[0]))
	perBlock := int64((16 + 256 + 240) * 4)
	want := map[int]int64{}
	for i := 0; i < 12; i++ {
		want[i] = perBlock
	}
	if !reflect.DeepEqual(p.FFNBlockBytes, want) {
		t.Fatalf("ffn %v", p.FFNBlockBytes)
	}
	if got := lrtMoved(t, SpillOverrides(p, lrtI64(2*perBlock)), 12); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Fatalf("moved %v", got)
	}
}

func TestLocalRuntimeUnreadableShardDoesNotPriceTheSplitAway(t *testing.T) {
	parts := lrtSplit(t, t.TempDir(), "half-downloaded", []lrtSplitSpec{
		{tensors: []lrtTensor{{"blk.0.attn_q.weight", 16 << 20}}},
		{tensors: []lrtTensor{{"blk.1.attn_q.weight", 16 << 20}}},
		{tensors: []lrtTensor{{"blk.2.attn_q.weight", 16 << 20}}},
	}, 0)
	if err := os.WriteFile(parts[1], []byte("GGUF\x03\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := lrtMustHeader(t, parts[0]).TensorBytes; b != 2*(64<<20) {
		t.Fatalf("bytes %d", b)
	}
}

func TestLocalRuntimeATruncatedHeaderSkipsTheModelInsteadOfRaising(t *testing.T) {
	parts := lrtSplit(t, t.TempDir(), "cut-head", []lrtSplitSpec{
		{}, {tensors: []lrtTensor{{"blk.0.attn_q.weight", 16 << 20}}},
	}, 0)
	if err := os.WriteFile(parts[0], []byte("GGUF\x03\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadGGUFHeader(parts[0])
	ge, ok := err.(*GGUFError)
	if !ok || ge.Class != "ValueError" || !GGUFReadSkippable(err) {
		t.Fatalf("err %#v", err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), ".icebath"))
	budget := NewHardwareBudget(64<<30, 64<<30, 64<<30)
	if need, priced, err := residentFootprint(parts[0], budget, 8192, false); priced || need != 0 || err != nil {
		t.Fatalf("footprint %d %v %v", need, priced, err)
	}
}
