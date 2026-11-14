// Plans, launches and supervises the managed llama.cpp server and finds local model servers already running
package local

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	// Name ending of one shard of a split model file
	splitPartPattern  = regexp.MustCompile(`-(\d{5})-of-(\d{5})\.gguf$`)
	partSuffixPattern = regexp.MustCompile(`-\d{5}-of-\d{5}$`)
	ffnWeightPattern  = regexp.MustCompile(`^blk\.(\d+)\.ffn_.*\.weight`)
	ssmConvPattern    = regexp.MustCompile(`^blk\.(\d+)\.ssm_conv1d\.weight`)
)

// Tensors the engine reads row by row from disk for an architecture instead of loading them
var lazyReadTensors = map[string]map[string]bool{
	"qwen4exp": {"per_layer_token_embd.weight": true},
	"gemma4":   {"per_layer_token_embd.weight": true},
}

// Smallest marked tensor the engine reads lazily under its default mode
const lazyReadMinBytes = 4 << 30

// Block bytes and block elements of each ggml tensor type
var ggmlTypeSizes = map[uint32][2]uint64{
	0: {4, 1}, 1: {2, 1}, 2: {18, 32}, 3: {20, 32}, 6: {22, 32}, 7: {24, 32},
	8: {34, 32}, 9: {36, 32}, 10: {84, 256}, 11: {110, 256}, 12: {144, 256},
	13: {176, 256}, 14: {210, 256}, 15: {292, 256}, 16: {66, 256},
	17: {74, 256}, 18: {98, 256}, 19: {50, 256}, 20: {18, 32},
	21: {110, 256}, 22: {82, 256}, 23: {136, 256}, 24: {1, 1}, 25: {2, 1},
	26: {4, 1}, 27: {8, 1}, 28: {8, 1}, 29: {56, 256}, 30: {2, 1},
	39: {17, 32}, 40: {36, 64}, 41: {18, 128}, 42: {18, 64},
}

// Preset key of each sampling metadata key a model file may carry
var samplingINIKey = map[string]string{
	"temp": "temp", "temperature": "temp", "top_p": "top-p", "top_k": "top-k", "min_p": "min-p",
	"repeat_penalty": "repeat-penalty", "presence_penalty": "presence-penalty",
}

// Model file that could not be read as GGUF
type GGUFError struct {
	Path   string
	Reason string
}

// Reason with the file it concerns
func (e *GGUFError) Error() string { return e.Reason + ": " + e.Path }

// Metadata and tensor table totals of one model, a split model summed over its shards on disk
type GGUFHeader struct {
	Path     string
	Version  uint32
	Metadata map[string]any
	// Metadata keys in file order
	Keys        []string
	Tensors     uint64
	TensorBytes int64
	// Bytes of the token embedding table
	EmbdTableBytes int64
	// Feed forward weight bytes by block index
	FFNBlockBytes map[int]int64
	// Sizes of tensors some architecture reads lazily, by tensor name
	LazyCandidateBytes map[string]int64
	// Blocks that keep recurrent state
	RecurrentBlocks map[int]bool
}

// Model id of a file stem, the shard suffix of a split removed
func ModelIDFromStem(stem string) string { return partSuffixPattern.ReplaceAllString(stem, "") }

// Integer reading of a metadata value, zero for anything that is no number
func asInt(v any) int64 {
	switch n := v.(type) {
	case uint8:
		return int64(n)
	case int8:
		return int64(n)
	case uint16:
		return int64(n)
	case int16:
		return int64(n)
	case uint32:
		return int64(n)
	case int32:
		return int64(n)
	case uint64:
		return int64(n)
	case int64:
		return n
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case bool:
		if n {
			return 1
		}
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// Reports an integer typed metadata value
func isInteger(v any) bool {
	switch v.(type) {
	case uint8, int8, uint16, int16, uint32, int32, uint64, int64:
		return true
	}
	return false
}

// Value of general.architecture
func (h *GGUFHeader) Architecture() string {
	if s, ok := h.Metadata["general.architecture"].(string); ok {
		return s
	}
	return ""
}

// Metadata value under the architecture prefix
func (h *GGUFHeader) archKey(suffix string) any { return h.Metadata[h.Architecture()+"."+suffix] }

// Integer under the architecture prefix, zero when absent
func (h *GGUFHeader) archInt(suffix string) int64 { return asInt(h.archKey(suffix)) }

// Weight bytes the engine keeps on disk and reads on demand
func (h *GGUFHeader) LazyBytes() int64 {
	marked := lazyReadTensors[h.Architecture()]
	var total int64
	for name, nbytes := range h.LazyCandidateBytes {
		if marked[name] && nbytes > lazyReadMinBytes {
			total += nbytes
		}
	}
	return total
}

// Bytes of one copy of one sequence's recurrent state, zero when the file declares no state shape
func (h *GGUFHeader) RecurrentStateBytes() int64 {
	inner, state := h.archInt("ssm.inner_size"), h.archInt("ssm.state_size")
	if len(h.RecurrentBlocks) == 0 || inner == 0 || state == 0 {
		return 0
	}
	convDim := inner + 2*h.archInt("ssm.group_count")*state
	perBlock := 4 * (inner*state + max(0, h.archInt("ssm.conv_kernel")-1)*convDim)
	return int64(len(h.RecurrentBlocks)) * perBlock
}

// Block count
func (h *GGUFHeader) NLayer() int { return int(h.archInt("block_count")) }

// Context length the model was trained with
func (h *GGUFHeader) NCtxTrain() int { return int(h.archInt("context_length")) }

// Embedding width
func (h *GGUFHeader) NEmbd() int { return int(h.archInt("embedding_length")) }

// Width of the sliding attention window, zero when the model has none
func (h *GGUFHeader) SlidingWindow() int { return int(h.archInt("attention.sliding_window")) }

// Number of experts, zero for a dense model
func (h *GGUFHeader) ExpertCount() int { return int(h.archInt("expert_count")) }

// Period of full attention layers in a linear hybrid, zero when absent
func (h *GGUFHeader) FullAttentionInterval() int { return int(h.archInt("full_attention_interval")) }

// Per token key size of sliding window layers when it differs from the global one
func (h *GGUFHeader) KeyLengthSWA() int { return int(h.archInt("attention.key_length_swa")) }

// Per token value size of sliding window layers when it differs from the global one
func (h *GGUFHeader) ValueLengthSWA() int { return int(h.archInt("attention.value_length_swa")) }

// Per layer sliding window flags the file declares as an array, nil when it declares none
func (h *GGUFHeader) SlidingWindowPattern() []int {
	list, ok := h.archKey("attention.sliding_window_pattern").([]any)
	if !ok {
		return nil
	}
	out := make([]int, len(list))
	for i, v := range list {
		out[i] = int(asInt(v))
	}
	return out
}

// Scalar period of full attention layers the file declares, zero when absent or given as an array
func (h *GGUFHeader) SlidingWindowPatternPeriod() int {
	if v := h.archKey("attention.sliding_window_pattern"); isInteger(v) {
		return int(asInt(v))
	}
	return 0
}

// Vocabulary size from the metadata, else the length of the tokenizer list
func (h *GGUFHeader) NVocab() int {
	if v := h.archInt("vocab_size"); v != 0 {
		return int(v)
	}
	if tokens, ok := h.Metadata["tokenizer.ggml.tokens"].([]any); ok {
		return len(tokens)
	}
	return 0
}

// One preset key with its value text
type INIPair struct {
	Key   string
	Value string
}

// Sampling the publisher recommends, as preset keys in file order, empty when the file carries none
func (h *GGUFHeader) SamplingDefaults() []INIPair {
	var out []INIPair
	at := map[string]int{}
	for _, key := range h.Keys {
		if !strings.HasPrefix(key, "general.sampling.") {
			continue
		}
		name, ok := samplingINIKey[key[strings.LastIndexByte(key, '.')+1:]]
		if !ok {
			continue
		}
		var value float64
		switch n := h.Metadata[key].(type) {
		case float32:
			value = float64(n)
		case float64:
			value = n
		case bool:
			value = float64(asInt(n))
		default:
			if !isInteger(n) {
				continue
			}
			value = float64(asInt(n))
		}
		value = math.RoundToEven(value*1e4) / 1e4
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if i, seen := at[name]; seen {
			out[i].Value = text
			continue
		}
		at[name] = len(out)
		out = append(out, INIPair{Key: name, Value: text})
	}
	return out
}

// Largest attention head count
func (h *GGUFHeader) NHead() int {
	v := h.archKey("attention.head_count")
	if list, ok := v.([]any); ok {
		best := int64(0)
		for i, item := range list {
			if n := asInt(item); i == 0 || n > best {
				best = n
			}
		}
		return int(best)
	}
	return int(asInt(v))
}

// Key value head count of each layer, zero marking a recurrent or linear layer
func (h *GGUFHeader) HeadCountsKV() []int {
	v := h.archKey("attention.head_count_kv")
	if list, ok := v.([]any); ok {
		out := make([]int, len(list))
		for i, item := range list {
			out[i] = int(asInt(item))
		}
		return out
	}
	scalar, interval, layers := int(asInt(v)), h.FullAttentionInterval(), h.NLayer()
	out := make([]int, max(0, layers))
	for i := range out {
		if interval <= 1 || (i+1)%interval == 0 {
			out[i] = scalar
		}
	}
	return out
}

// Per token key size of a full attention layer
func (h *GGUFHeader) HeadDimK() int {
	if v := h.archInt("attention.key_length"); v != 0 {
		return int(v)
	}
	if heads := h.NHead(); heads != 0 {
		return h.NEmbd() / heads
	}
	return 0
}

// Per token value size of a full attention layer
func (h *GGUFHeader) HeadDimV() int {
	if v := h.archInt("attention.value_length"); v != 0 {
		return int(v)
	}
	return h.HeadDimK()
}

// Every shard on disk of the split that path belongs to, first shard first, nil when path is no split or stands alone
func SplitParts(path string) []string {
	base := filepath.Base(path)
	loc := splitPartPattern.FindStringSubmatchIndex(base)
	if loc == nil {
		return nil
	}
	stem := base[:loc[0]]
	total, err := strconv.Atoi(base[loc[4]:loc[5]])
	if err != nil {
		return nil
	}
	var parts []string
	for i := 1; i <= total; i++ {
		part := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-%05d-of-%05d.gguf", stem, i, total))
		if fi, err := os.Stat(part); err == nil && fi.Mode().IsRegular() {
			parts = append(parts, part)
		}
	}
	if len(parts) > 1 {
		return parts
	}
	return nil
}

// Reader over one model file that fails with a truncation error when the header ends early
type ggufReader struct {
	r    *bufio.Reader
	path string
	left int64
}

// Reads exactly len(buf) bytes
func (g *ggufReader) full(buf []byte) error {
	if int64(len(buf)) > g.left {
		return &GGUFError{Path: g.path, Reason: "truncated GGUF header"}
	}
	if _, err := io.ReadFull(g.r, buf); err != nil {
		return &GGUFError{Path: g.path, Reason: "truncated GGUF header"}
	}
	g.left -= int64(len(buf))
	return nil
}

// Little endian unsigned integer of size bytes
func (g *ggufReader) uint(size int) (uint64, error) {
	var buf [8]byte
	if err := g.full(buf[:size]); err != nil {
		return 0, err
	}
	switch size {
	case 1:
		return uint64(buf[0]), nil
	case 2:
		return uint64(binary.LittleEndian.Uint16(buf[:])), nil
	case 4:
		return uint64(binary.LittleEndian.Uint32(buf[:])), nil
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// Length prefixed text, invalid bytes replaced
func (g *ggufReader) str() (string, error) {
	n, err := g.uint(8)
	if err != nil {
		return "", err
	}
	if n > uint64(g.left) {
		return "", &GGUFError{Path: g.path, Reason: "truncated GGUF header"}
	}
	buf := make([]byte, n)
	if err := g.full(buf); err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(buf), "�"), nil
}

// One metadata value of the given type
func (g *ggufReader) value(vtype uint32) (any, error) {
	switch vtype {
	case 8:
		return g.str()
	case 9:
		etype, err := g.uint(4)
		if err != nil {
			return nil, err
		}
		n, err := g.uint(8)
		if err != nil {
			return nil, err
		}
		if n > uint64(g.left) {
			return nil, &GGUFError{Path: g.path, Reason: "truncated GGUF header"}
		}
		list := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			item, err := g.value(uint32(etype))
			if err != nil {
				return nil, err
			}
			list = append(list, item)
		}
		return list, nil
	}
	sizes := map[uint32]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}
	size, ok := sizes[vtype]
	if !ok {
		return nil, &GGUFError{Path: g.path, Reason: fmt.Sprintf("unknown GGUF value type %d", vtype)}
	}
	raw, err := g.uint(size)
	if err != nil {
		return nil, err
	}
	switch vtype {
	case 0:
		return uint8(raw), nil
	case 1:
		return int8(raw), nil
	case 2:
		return uint16(raw), nil
	case 3:
		return int16(raw), nil
	case 4:
		return uint32(raw), nil
	case 5:
		return int32(raw), nil
	case 6:
		return math.Float32frombits(uint32(raw)), nil
	case 7:
		return raw != 0, nil
	case 10:
		return raw, nil
	case 11:
		return int64(raw), nil
	}
	return math.Float64frombits(raw), nil
}

// Header of one file, its own metadata and its own tensor table
func readPart(path string) (*GGUFHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	g := &ggufReader{r: bufio.NewReaderSize(f, 1<<20), path: path, left: fi.Size()}
	magic := make([]byte, 4)
	if g.full(magic) != nil || string(magic) != "GGUF" {
		return nil, &GGUFError{Path: path, Reason: "not a GGUF file"}
	}
	version, err := g.uint(4)
	if err != nil {
		return nil, err
	}
	tensors, err := g.uint(8)
	if err != nil {
		return nil, err
	}
	pairs, err := g.uint(8)
	if err != nil {
		return nil, err
	}
	h := &GGUFHeader{
		Path: path, Version: uint32(version), Metadata: map[string]any{}, Tensors: tensors,
		FFNBlockBytes: map[int]int64{}, LazyCandidateBytes: map[string]int64{}, RecurrentBlocks: map[int]bool{},
	}
	for i := uint64(0); i < pairs; i++ {
		key, err := g.str()
		if err != nil {
			return nil, err
		}
		vtype, err := g.uint(4)
		if err != nil {
			return nil, err
		}
		value, err := g.value(uint32(vtype))
		if err != nil {
			return nil, err
		}
		if _, seen := h.Metadata[key]; !seen {
			h.Keys = append(h.Keys, key)
		}
		h.Metadata[key] = value
	}
	for i := uint64(0); i < tensors; i++ {
		name, err := g.str()
		if err != nil {
			return nil, err
		}
		dims, err := g.uint(4)
		if err != nil {
			return nil, err
		}
		elems := uint64(1)
		for d := uint64(0); d < dims; d++ {
			dim, err := g.uint(8)
			if err != nil {
				return nil, err
			}
			elems *= dim
		}
		ttype, err := g.uint(4)
		if err != nil {
			return nil, err
		}
		if _, err := g.uint(8); err != nil {
			return nil, err
		}
		size, ok := ggmlTypeSizes[uint32(ttype)]
		if !ok {
			return nil, &GGUFError{Path: path, Reason: fmt.Sprintf("unknown ggml tensor type %d", ttype)}
		}
		nbytes := int64(elems / size[1] * size[0])
		h.TensorBytes += nbytes
		switch {
		case name == "token_embd.weight":
			h.EmbdTableBytes = nbytes
		case ffnWeightPattern.MatchString(name):
			block, _ := strconv.Atoi(ffnWeightPattern.FindStringSubmatch(name)[1])
			h.FFNBlockBytes[block] += nbytes
		case name == "per_layer_token_embd.weight":
			h.LazyCandidateBytes[name] = nbytes
		case ssmConvPattern.MatchString(name):
			block, _ := strconv.Atoi(ssmConvPattern.FindStringSubmatch(name)[1])
			h.RecurrentBlocks[block] = true
		}
	}
	return h, nil
}

// Header of a model, a split priced as the sum of the shards on disk with the metadata of the first
func ReadGGUFHeader(path string) (*GGUFHeader, error) {
	parts := SplitParts(path)
	if parts == nil {
		return readPart(path)
	}
	first, err := readPart(parts[0])
	if err != nil {
		return nil, err
	}
	readable := []*GGUFHeader{first}
	for _, part := range parts[1:] {
		h, err := readPart(part)
		var bad *GGUFError
		if err != nil {
			if !errors.As(err, &bad) && !isPathError(err) {
				return nil, err
			}
			slog.Debug(fmt.Sprintf("split part unreadable %s: %v", filepath.Base(part), err))
			continue
		}
		readable = append(readable, h)
	}
	if len(readable) == 1 {
		return first, nil
	}
	sum := &GGUFHeader{
		Path: path, Version: first.Version, Metadata: first.Metadata, Keys: first.Keys,
		FFNBlockBytes: map[int]int64{}, LazyCandidateBytes: map[string]int64{}, RecurrentBlocks: map[int]bool{},
	}
	for _, h := range readable {
		sum.Tensors += h.Tensors
		sum.TensorBytes += h.TensorBytes
		sum.EmbdTableBytes += h.EmbdTableBytes
		for block, nbytes := range h.FFNBlockBytes {
			sum.FFNBlockBytes[block] += nbytes
		}
		for name, nbytes := range h.LazyCandidateBytes {
			sum.LazyCandidateBytes[name] = nbytes
		}
		for block := range h.RecurrentBlocks {
			sum.RecurrentBlocks[block] = true
		}
	}
	return sum, nil
}

// Reports a failure of the file system rather than of the format
func isPathError(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}
