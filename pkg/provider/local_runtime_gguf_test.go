package provider

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// One metadata entry with its GGUF value type, array values as lrtArray
type lrtKV struct {
	key   string
	vtype uint32
	value any
}

type lrtArray struct {
	etype uint32
	items []any
}

type lrtTensor struct {
	name  string
	elems uint64
}

func lrtPackValue(b *bytes.Buffer, vtype uint32, v any) {
	le := binary.LittleEndian
	switch vtype {
	case 8:
		s := v.(string)
		binary.Write(b, le, uint64(len(s)))
		b.WriteString(s)
	case 9:
		a := v.(lrtArray)
		binary.Write(b, le, a.etype)
		binary.Write(b, le, uint64(len(a.items)))
		for _, item := range a.items {
			lrtPackValue(b, a.etype, item)
		}
	case 0:
		b.WriteByte(byte(v.(int)))
	case 1:
		b.WriteByte(byte(int8(v.(int))))
	case 4:
		binary.Write(b, le, uint32(v.(int)))
	case 6:
		binary.Write(b, le, math.Float32bits(float32(v.(float64))))
	case 7:
		if v.(bool) {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
	default:
		panic(fmt.Sprintf("unsupported test vtype %d", vtype))
	}
}

// Writes a GGUF v3 file with typed metadata and 1-D tensors of the given element counts and type
func lrtWriteGGUFTyped(t *testing.T, path string, meta []lrtKV, tensors []lrtTensor, ttype uint32) {
	t.Helper()
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("GGUF")
	binary.Write(&b, le, uint32(3))
	binary.Write(&b, le, uint64(len(tensors)))
	binary.Write(&b, le, uint64(len(meta)))
	for _, kv := range meta {
		lrtPackValue(&b, 8, kv.key)
		binary.Write(&b, le, kv.vtype)
		lrtPackValue(&b, kv.vtype, kv.value)
	}
	for _, tn := range tensors {
		lrtPackValue(&b, 8, tn.name)
		binary.Write(&b, le, uint32(1))
		binary.Write(&b, le, tn.elems)
		binary.Write(&b, le, ttype)
		binary.Write(&b, le, uint64(0))
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Writes a GGUF v3 file whose metadata alternates keys with uint32 or string values and f32 tensors
func lrtWriteGGUF(t *testing.T, path string, meta []any, tensors []lrtTensor) {
	t.Helper()
	var kvs []lrtKV
	for i := 0; i+1 < len(meta); i += 2 {
		key := meta[i].(string)
		if s, ok := meta[i+1].(string); ok {
			kvs = append(kvs, lrtKV{key, 8, s})
		} else {
			kvs = append(kvs, lrtKV{key, 4, meta[i+1]})
		}
	}
	lrtWriteGGUFTyped(t, path, kvs, tensors, 0)
}

func lrtMustHeader(t *testing.T, path string) *GGUFHeader {
	t.Helper()
	h, err := ReadGGUFHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func lrtMustProfile(t *testing.T, h *GGUFHeader) ModelProfile {
	t.Helper()
	p, err := ProfileFromGGUF(h)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Value of an accessor that must not fail, panicking the test otherwise
func lrtOK[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Block indexes whose ffn_up tensor an -ot pattern moves
func lrtMoved(t *testing.T, args []string, n int) []int {
	t.Helper()
	re := regexp.MustCompile(strings.TrimSuffix(args[1], "=CPU"))
	moved := []int{}
	for i := 0; i < n; i++ {
		if re.MatchString(fmt.Sprintf("blk.%d.ffn_up.weight", i)) {
			moved = append(moved, i)
		}
	}
	return moved
}

func lrtI64(v int64) *int64 { return &v }

func TestLocalRuntimeHybridFFNBlockSizesDrivePerBlockSpill(t *testing.T) {
	tensors := []lrtTensor{{"token_embd.weight", 64}}
	for i := 0; i < 12; i++ {
		tensors = append(tensors, lrtTensor{fmt.Sprintf("blk.%d.attn_norm.weight", i), 16},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_norm.weight", i), 16},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_up.weight", i), 256},
			lrtTensor{fmt.Sprintf("blk.%d.ffn_down.weight", i), 240})
	}
	gguf := filepath.Join(t.TempDir(), "hybrid.gguf")
	lrtWriteGGUF(t, gguf, []any{"general.architecture", "qwen35", "qwen35.block_count", 12,
		"qwen35.context_length", 65536, "qwen35.full_attention_interval", 4,
		"qwen35.attention.head_count", 8, "qwen35.attention.head_count_kv", 2,
		"qwen35.attention.key_length", 128, "qwen35.attention.value_length", 128}, tensors)

	header := lrtMustHeader(t, gguf)
	perBlock := int64((16 + 256 + 240) * 4)
	want := map[int]int64{}
	for i := 0; i < 12; i++ {
		want[i] = perBlock
	}
	if !reflect.DeepEqual(header.FFNBlockBytes, want) {
		t.Fatalf("ffn blocks %v", header.FFNBlockBytes)
	}
	profile := lrtMustProfile(t, header)
	kv := lrtOK(header.HeadCountsKV())
	if !reflect.DeepEqual(kv, []int64{0, 0, 0, 2, 0, 0, 0, 2, 0, 0, 0, 2}) {
		t.Fatalf("kv heads %v", kv)
	}
	args := SpillOverrides(profile, lrtI64(4*perBlock+1))
	if got := lrtMoved(t, args, 12); !reflect.DeepEqual(got, []int{0, 1, 2, 4, 5}) {
		t.Fatalf("moved %v", got)
	}
	if regexp.MustCompile(strings.TrimSuffix(args[1], "=CPU")).MatchString("blk.1.attn_norm.weight") {
		t.Fatal("attn_norm moved")
	}
}

func TestLocalRuntimeReaderSizesGGMLTypes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ttype         uint32
		elems, expect int64
	}{
		{"token_embd.weight", 39, 64, 34},
		{"blk.0.ssm_in.weight", 40, 128, 72},
		{"blk.0.ssm_in.weight", 41, 256, 36},
		{"blk.0.ssm_in.weight", 42, 128, 36},
	} {
		gguf := filepath.Join(t.TempDir(), "m.gguf")
		lrtWriteGGUFTyped(t, gguf, nil, []lrtTensor{{tc.name, uint64(tc.elems)}}, tc.ttype)
		h := lrtMustHeader(t, gguf)
		if h.TensorBytes != tc.expect {
			t.Fatalf("type %d bytes %d want %d", tc.ttype, h.TensorBytes, tc.expect)
		}
		if tc.ttype == 39 && h.EmbdTableBytes != h.TensorBytes {
			t.Fatalf("embd %d", h.EmbdTableBytes)
		}
	}
}

func TestLocalRuntimeReaderRefusesUnknownTensorType(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "m.gguf")
	lrtWriteGGUFTyped(t, gguf, nil, []lrtTensor{{"blk.0.x.weight", 32}}, 99)
	_, err := ReadGGUFHeader(gguf)
	if err == nil || !GGUFReadSkippable(err) || !strings.Contains(err.Error(), "unknown ggml tensor type 99") {
		t.Fatalf("err %v", err)
	}
}

func lrtSSMModel(t *testing.T, path, arch string, nLayer int, recurrent func(int) bool, ssm []any) {
	tensors := []lrtTensor{{"token_embd.weight", 64}}
	for i := 0; i < nLayer; i++ {
		tensors = append(tensors, lrtTensor{fmt.Sprintf("blk.%d.attn_norm.weight", i), 16})
		if recurrent(i) {
			tensors = append(tensors, lrtTensor{fmt.Sprintf("blk.%d.ssm_conv1d.weight", i), 16})
		}
	}
	meta := []any{"general.architecture", arch, arch + ".block_count", nLayer, arch + ".context_length", 262144}
	for i := 0; i+1 < len(ssm); i += 2 {
		meta = append(meta, arch+".ssm."+ssm[i].(string), ssm[i+1])
	}
	lrtWriteGGUF(t, path, meta, tensors)
}

func TestLocalRuntimeReaderPricesOneRecurrentStateCopyFromTheSSMShape(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "qwen.gguf")
	lrtSSMModel(t, gguf, "qwen35", 64, func(i int) bool { return (i+1)%4 != 0 },
		[]any{"inner_size", 6144, "state_size", 128, "conv_kernel", 4, "group_count", 16})
	header := lrtMustHeader(t, gguf)
	state := lrtOK(header.RecurrentStateBytes())
	if state != 48*4*(6144*128+3*(6144+2*16*128)) {
		t.Fatalf("state %d", state)
	}
	if math.Round(float64(state)/(1<<20)*10)/10 != 149.6 {
		t.Fatalf("MiB %f", float64(state)/(1<<20))
	}
	if lrtMustProfile(t, header).RecurrentStateBytes != state {
		t.Fatal("profile state differs")
	}
}

func TestLocalRuntimeReaderCountsOnlyBlocksThatHoldRecurrentState(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "bolt.gguf")
	lrtSSMModel(t, gguf, "nemotron_h_moe", 88, func(i int) bool { return i < 80 && i%2 == 0 },
		[]any{"inner_size", 8192, "state_size", 128, "conv_kernel", 4, "group_count", 8})
	state := lrtOK(lrtMustHeader(t, gguf).RecurrentStateBytes())
	if math.Round(float64(state)/(1<<20)*10)/10 != 164.7 {
		t.Fatalf("MiB %f", float64(state)/(1<<20))
	}
}

func TestLocalRuntimeSplitModelCountsRecurrentBlocksAcrossParts(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "m-00001-of-00002.gguf")
	lrtSSMModel(t, first, "qwen35", 4, func(i int) bool { return i < 2 },
		[]any{"inner_size", 64, "state_size", 8, "conv_kernel", 4, "group_count", 1})
	lrtWriteGGUF(t, filepath.Join(dir, "m-00002-of-00002.gguf"), []any{"split.count", 2},
		[]lrtTensor{{"blk.2.ssm_conv1d.weight", 16}, {"blk.3.ssm_conv1d.weight", 16}})
	state := lrtOK(lrtMustHeader(t, first).RecurrentStateBytes())
	if state != 4*4*(64*8+3*(64+2*8)) {
		t.Fatalf("state %d", state)
	}
}

func TestLocalRuntimeNoSSMShapePricesNoRecurrentState(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "dense.gguf")
	lrtSSMModel(t, gguf, "llama", 4, func(int) bool { return false }, nil)
	if state := lrtOK(lrtMustHeader(t, gguf).RecurrentStateBytes()); state != 0 {
		t.Fatalf("state %d", state)
	}
}

const (
	lrtOver4GiB    = (4<<30)/4 + 256
	lrtExactly4GiB = (4 << 30) / 4
)

func lrtLazyArch(name string) []any {
	return []any{"general.architecture", name, name + ".block_count", 4, name + ".context_length", 65536,
		name + ".attention.head_count", 8, name + ".attention.head_count_kv", 2, name + ".attention.key_length", 128}
}

func TestLocalRuntimeAMarkedTableOver4GiBStaysOutOfTheLoadedWeights(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "flash.gguf")
	lrtWriteGGUF(t, gguf, lrtLazyArch("qwen4exp"), []lrtTensor{{"per_layer_token_embd.weight", lrtOver4GiB}, {"blk.0.attn_q.weight", 16 << 20}})
	h := lrtMustHeader(t, gguf)
	if h.LazyBytes() != lrtOver4GiB*4 || lrtMustProfile(t, h).WeightsBytes != 64<<20 {
		t.Fatalf("lazy %d", h.LazyBytes())
	}
}

func TestLocalRuntimeAMarkedTableOf4GiBOrLessLoadsLikeAnyWeight(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "small.gguf")
	lrtWriteGGUF(t, gguf, lrtLazyArch("qwen4exp"), []lrtTensor{{"per_layer_token_embd.weight", lrtExactly4GiB}})
	h := lrtMustHeader(t, gguf)
	if h.LazyBytes() != 0 || lrtMustProfile(t, h).WeightsBytes != h.TensorBytes {
		t.Fatalf("lazy %d", h.LazyBytes())
	}
}

func TestLocalRuntimeAnArchitectureThatMarksNothingLoadsTheSameTable(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "other.gguf")
	lrtWriteGGUF(t, gguf, lrtLazyArch("llama"), []lrtTensor{{"per_layer_token_embd.weight", lrtOver4GiB}})
	if w := lrtMustProfile(t, lrtMustHeader(t, gguf)).WeightsBytes; w != lrtOver4GiB*4 {
		t.Fatalf("weights %d", w)
	}
}

func TestLocalRuntimeTheTableIsFoundInALaterSplitPart(t *testing.T) {
	dir := t.TempDir()
	lrtWriteGGUF(t, filepath.Join(dir, "flash-00001-of-00002.gguf"), lrtLazyArch("qwen4exp"), nil)
	lrtWriteGGUF(t, filepath.Join(dir, "flash-00002-of-00002.gguf"),
		[]any{"split.no", 1, "split.count", 2, "split.tensors.count", 0},
		[]lrtTensor{{"per_layer_token_embd.weight", lrtOver4GiB}, {"blk.0.attn_q.weight", 16 << 20}})
	h := lrtMustHeader(t, filepath.Join(dir, "flash-00001-of-00002.gguf"))
	if h.LazyBytes() != lrtOver4GiB*4 || lrtMustProfile(t, h).WeightsBytes != 64<<20 {
		t.Fatalf("lazy %d", h.LazyBytes())
	}
}

func TestLocalRuntimeAnEngineThatLoadsTheTableUpFrontPaysForIt(t *testing.T) {
	gguf := filepath.Join(t.TempDir(), "flash.gguf")
	lrtWriteGGUF(t, gguf, lrtLazyArch("qwen4exp"), []lrtTensor{{"per_layer_token_embd.weight", lrtOver4GiB}, {"blk.0.attn_q.weight", 16 << 20}})
	p := lrtMustProfile(t, lrtMustHeader(t, gguf))
	unified := func(lazy bool) HardwareBudget {
		b := HardwareBudget{UsableVRAMBytes: 64 << 30, TotalDeviceBytes: 80 << 30, UMA: true, LazyReads: lazy}
		return b
	}
	if w := AsLoaded(p, unified(true)).WeightsBytes; w != 64<<20 {
		t.Fatalf("lazy reads %d", w)
	}
	loaded := AsLoaded(p, unified(false))
	if loaded.WeightsBytes != (64<<20)+lrtOver4GiB*4 || loaded.LazyBytes != 0 || p.LazyBytes != lrtOver4GiB*4 {
		t.Fatalf("up front %d", loaded.WeightsBytes)
	}
}

func TestLocalRuntimeMetadataCoercionFollowsPythonInt(t *testing.T) {
	h := &GGUFHeader{Metadata: map[string]any{
		"general.architecture": "x", "x.block_count": " 1_2 ", "x.context_length": 3.9,
		"x.embedding_length": []any{int64(1)}, "x.expert_count": "abc",
		"x.attention.head_count": []any{int64(2), 7.5, true}, "x.vocab_size": math.NaN(),
		"general.sampling.top_p": 0.95, "general.sampling.top_k": int64(40), "general.sampling.temp": 1.00004,
		"general.sampling.min_p": "0.1", "general.sampling.unknown": 1.0,
	}}
	if n := lrtOK(h.NLayer()); n != 12 {
		t.Fatalf("block count %d", n)
	}
	if n := lrtOK(h.NCtxTrain()); n != 3 {
		t.Fatalf("context %d", n)
	}
	if _, err := h.NEmbd(); err == nil || err.(*GGUFError).Class != "TypeError" || GGUFReadSkippable(err) {
		t.Fatalf("list coercion %v", err)
	}
	if _, err := h.ExpertCount(); err == nil || err.(*GGUFError).Class != "ValueError" || !GGUFReadSkippable(err) {
		t.Fatalf("string coercion %v", err)
	}
	if n := lrtOK(h.NHead()); n != 7 {
		t.Fatalf("head count %d", n)
	}
	if _, err := h.NVocab(); err == nil || err.(*GGUFError).Class != "ValueError" {
		t.Fatalf("nan coercion %v", err)
	}
	want := map[string]string{"top-p": "0.95", "top-k": "40", "temp": "1"}
	if got := lrtOK(h.SamplingDefaults()); !reflect.DeepEqual(got, want) {
		t.Fatalf("sampling %v", got)
	}
	if ModelIDFromStem("m-00001-of-00003") != "m" || ModelIDFromStem("m-1-of-3") != "m-1-of-3" {
		t.Fatal("stem suffix")
	}
}
