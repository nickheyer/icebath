package provider

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ggufMagic = []byte("GGUF")

// Matches the split suffix "-00001-of-00003.gguf" and captures part and total
var GGUFSplitPartRE = regexp.MustCompile(`-(\p{Nd}{5})-of-(\p{Nd}{5})\.gguf\n?\z`)

var (
	ggufPartSuffixRE = regexp.MustCompile(`-\p{Nd}{5}-of-\p{Nd}{5}(\n?)\z`)
	ggufFFNWeightRE  = regexp.MustCompile(`^blk\.(\p{Nd}+)\.ffn_.*\.weight`)
	ggufSSMConvRE    = regexp.MustCompile(`^blk\.(\p{Nd}+)\.ssm_conv1d\.weight`)
)

// Tensors each architecture reads row by row from disk instead of loading
var ggufLazyReadTensors = map[string]map[string]bool{
	"qwen4exp": {"per_layer_token_embd.weight": true},
	"gemma4":   {"per_layer_token_embd.weight": true},
}

var ggufLazyReadCandidates = func() map[string]bool {
	out := map[string]bool{}
	for _, names := range ggufLazyReadTensors {
		for name := range names {
			out[name] = true
		}
	}
	return out
}()

const ggufLazyReadMinBytes = int64(4) << 30

// Model id from a GGUF file stem with any split part suffix removed
func ModelIDFromStem(stem string) string {
	return ggufPartSuffixRE.ReplaceAllString(stem, "$1")
}

// ggml tensor type id to block bytes and elements per block
var ggmlTypeSizes = map[uint32][2]int64{
	0: {4, 1}, 1: {2, 1}, 2: {18, 32}, 3: {20, 32}, 6: {22, 32}, 7: {24, 32},
	8: {34, 32}, 9: {36, 32}, 10: {84, 256}, 11: {110, 256}, 12: {144, 256},
	13: {176, 256}, 14: {210, 256}, 15: {292, 256}, 16: {66, 256},
	17: {74, 256}, 18: {98, 256}, 19: {50, 256}, 20: {18, 32},
	21: {110, 256}, 22: {82, 256}, 23: {136, 256}, 24: {1, 1}, 25: {2, 1},
	26: {4, 1}, 27: {8, 1}, 28: {8, 1}, 29: {56, 256}, 30: {2, 1},
	39: {17, 32}, 40: {36, 64}, 41: {18, 128}, 42: {18, 64},
}

const (
	ggufVString = 8
	ggufVArray  = 9
)

// GGUF sampling metadata suffix to preset INI key
var ggufSamplingINIKey = map[string]string{
	"temp": "temp", "temperature": "temp", "top_p": "top-p",
	"top_k": "top-k", "min_p": "min-p",
	"repeat_penalty":   "repeat-penalty",
	"presence_penalty": "presence-penalty",
}

// Carries the Python exception class a header read or metadata coercion raised
type GGUFError struct {
	Class string
	Msg   string
}

func (e *GGUFError) Error() string { return e.Msg }

// Reports whether err is one callers treat as an unreadable model, a ValueError or an OS error
func GGUFReadSkippable(err error) bool {
	if err == nil {
		return false
	}
	ge, ok := err.(*GGUFError)
	return !ok || ge.Class == "ValueError"
}

// one model's metadata and tensor table totals, summed across split parts
type GGUFHeader struct {
	Path               string
	Version            uint32
	Metadata           map[string]any
	MetadataKeys       []string
	NTensors           int64
	TensorBytes        int64
	EmbdTableBytes     int64
	FFNBlockBytes      map[int]int64
	LazyCandidateBytes map[string]int64
	RecurrentBlocks    map[int]bool
}

// Python type name of a metadata value for exception messages
func ggufTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, uint64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// Value of a Unicode decimal digit rune, or -1 when it is not one
func ggufDigitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	if !unicode.Is(unicode.Nd, r) {
		return -1
	}
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}

// Python int of a decimal string with sign, underscores and Unicode digits
func ggufParseIntString(s string) (int64, error) {
	bad := &GGUFError{"ValueError", "invalid literal for int() with base 10: " + pyStringRepr(s)}
	t := strings.TrimFunc(s, unicode.IsSpace)
	neg := false
	if t != "" && (t[0] == '+' || t[0] == '-') {
		neg = t[0] == '-'
		t = t[1:]
	}
	if t == "" {
		return 0, bad
	}
	var digits []int
	prevUnderscore := true
	for _, r := range t {
		if r == '_' {
			if prevUnderscore {
				return 0, bad
			}
			prevUnderscore = true
			continue
		}
		d := ggufDigitValue(r)
		if d < 0 {
			return 0, bad
		}
		digits = append(digits, d)
		prevUnderscore = false
	}
	if prevUnderscore {
		return 0, bad
	}
	var n uint64
	for _, d := range digits {
		if n > (math.MaxUint64-uint64(d))/10 {
			return 0, &GGUFError{"OverflowError", "int too large to convert to int64: " + s}
		}
		n = n*10 + uint64(d)
	}
	if neg {
		if n > 1<<63 {
			return 0, &GGUFError{"OverflowError", "int too large to convert to int64: " + s}
		}
		return -int64(n), nil
	}
	if n > math.MaxInt64 {
		return 0, &GGUFError{"OverflowError", "int too large to convert to int64: " + s}
	}
	return int64(n), nil
}

// Python int of a metadata value, failing with the exception class Python raises
func ggufInt(v any) (int64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case uint64:
		if t > math.MaxInt64 {
			return 0, &GGUFError{"OverflowError", "int too large to convert to int64: " + strconv.FormatUint(t, 10)}
		}
		return int64(t), nil
	case float64:
		if math.IsNaN(t) {
			return 0, &GGUFError{"ValueError", "cannot convert float NaN to integer"}
		}
		if math.IsInf(t, 0) {
			return 0, &GGUFError{"OverflowError", "cannot convert float infinity to integer"}
		}
		if t >= 9223372036854775808.0 || t < -9223372036854775808.0 {
			return 0, &GGUFError{"OverflowError", "int too large to convert to int64: " + pyFloatRepr(t)}
		}
		return int64(t), nil
	case string:
		return ggufParseIntString(t)
	}
	return 0, &GGUFError{"TypeError", "int() argument must be a string, a bytes-like object or a real number, not '" + ggufTypeName(v) + "'"}
}

// Python int(v or 0) of a metadata value
func ggufIntOrZero(v any) (int64, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	return ggufInt(v)
}

func (h *GGUFHeader) Architecture() string {
	v, ok := h.Metadata["general.architecture"]
	if !ok {
		return ""
	}
	return pyStr(v)
}

// Bytes of weights llama.cpp keeps on disk and reads row by row instead of loading
func (h *GGUFHeader) LazyBytes() int64 {
	marked := ggufLazyReadTensors[h.Architecture()]
	var total int64
	for name, n := range h.LazyCandidateBytes {
		if marked[name] && n > ggufLazyReadMinBytes {
			total = ggufSatAdd(total, n)
		}
	}
	return total
}

func (h *GGUFHeader) archKey(suffix string) any {
	return h.Metadata[h.Architecture()+"."+suffix]
}

func (h *GGUFHeader) archInt(suffix string) (int64, error) {
	return ggufIntOrZero(h.archKey(suffix))
}

// One sequence's f32 conv window plus SSM state across stateful blocks, zero without an SSM shape
func (h *GGUFHeader) RecurrentStateBytes() (int64, error) {
	inner, err := h.archInt("ssm.inner_size")
	if err != nil {
		return 0, err
	}
	state, err := h.archInt("ssm.state_size")
	if err != nil {
		return 0, err
	}
	if len(h.RecurrentBlocks) == 0 || inner == 0 || state == 0 {
		return 0, nil
	}
	groups, err := h.archInt("ssm.group_count")
	if err != nil {
		return 0, err
	}
	kernel, err := h.archInt("ssm.conv_kernel")
	if err != nil {
		return 0, err
	}
	convDim := inner + 2*groups*state
	perBlock := 4 * (inner*state + max(0, kernel-1)*convDim)
	return int64(len(h.RecurrentBlocks)) * perBlock, nil
}

func (h *GGUFHeader) NLayer() (int64, error)        { return h.archInt("block_count") }
func (h *GGUFHeader) NCtxTrain() (int64, error)     { return h.archInt("context_length") }
func (h *GGUFHeader) NEmbd() (int64, error)         { return h.archInt("embedding_length") }
func (h *GGUFHeader) SlidingWindow() (int64, error) { return h.archInt("attention.sliding_window") }
func (h *GGUFHeader) ExpertCount() (int64, error)   { return h.archInt("expert_count") }

// Every Nth layer is full attention in GDN hybrids, zero when absent
func (h *GGUFHeader) FullAttentionInterval() (int64, error) {
	return h.archInt("full_attention_interval")
}

// Per token key size of sliding window layers when it differs from the global one, zero when absent
func (h *GGUFHeader) KeyLengthSWA() (int64, error) { return h.archInt("attention.key_length_swa") }

// Per token value size of sliding window layers when it differs from the global one, zero when absent
func (h *GGUFHeader) ValueLengthSWA() (int64, error) {
	return h.archInt("attention.value_length_swa")
}

// Per layer SWA flags from the file's array form, nil when the file declares no array
func (h *GGUFHeader) SlidingWindowPattern() ([]int64, error) {
	v, ok := h.archKey("attention.sliding_window_pattern").([]any)
	if !ok {
		return nil, nil
	}
	out := make([]int64, len(v))
	for i, x := range v {
		n, err := ggufInt(x)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// Scalar SWA period declared by the file, zero when absent or in array form
func (h *GGUFHeader) SlidingWindowPatternPeriod() int64 {
	switch t := h.archKey("attention.sliding_window_pattern").(type) {
	case int64:
		return t
	case uint64:
		return math.MaxInt64
	}
	return 0
}

// Vocabulary size from vocab_size metadata, else the tokenizer list length
func (h *GGUFHeader) NVocab() (int64, error) {
	if v := h.archKey("vocab_size"); pyTruthy(v) {
		return ggufInt(v)
	}
	if toks, ok := h.Metadata["tokenizer.ggml.tokens"].([]any); ok {
		return int64(len(toks)), nil
	}
	return 0, nil
}

// Python round of a float to four decimal places
func ggufRound4(f float64) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return f
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64)
	return r
}

// Upstream recommended sampling from general.sampling keys as preset INI keys, empty when absent
func (h *GGUFHeader) SamplingDefaults() (map[string]string, error) {
	out := map[string]string{}
	for _, key := range h.metadataOrder() {
		if !strings.HasPrefix(key, "general.sampling.") {
			continue
		}
		name, ok := ggufSamplingINIKey[key[strings.LastIndex(key, ".")+1:]]
		if !ok {
			continue
		}
		var f float64
		switch t := h.Metadata[key].(type) {
		case bool:
			if t {
				f = 1
			}
		case int64:
			f = float64(t)
		case uint64:
			f = float64(t)
		case float64:
			f = t
		default:
			continue
		}
		num := ggufRound4(f)
		if math.IsNaN(num) {
			return nil, &GGUFError{"ValueError", "cannot convert float NaN to integer"}
		}
		if math.IsInf(num, 0) {
			return nil, &GGUFError{"OverflowError", "cannot convert float infinity to integer"}
		}
		if num == math.Trunc(num) {
			if num == 0 {
				out[name] = "0"
			} else {
				out[name] = strconv.FormatFloat(num, 'f', 0, 64)
			}
		} else {
			out[name] = pyFloatRepr(num)
		}
	}
	return out, nil
}

// Metadata keys in file order, then any keys set directly in sorted order
func (h *GGUFHeader) metadataOrder() []string {
	seen := map[string]bool{}
	var keys []string
	for _, k := range h.MetadataKeys {
		if _, ok := h.Metadata[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range h.Metadata {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// Python max over a metadata list, numbers by value and strings by code point
func ggufListMax(v []any) (any, error) {
	if len(v) == 0 {
		return nil, &GGUFError{"ValueError", "max() iterable argument is empty"}
	}
	best := v[0]
	for _, x := range v[1:] {
		greater, err := ggufGreater(x, best)
		if err != nil {
			return nil, err
		}
		if greater {
			best = x
		}
	}
	return best, nil
}

// Python a > b for metadata scalars, TypeError across incomparable types
func ggufGreater(a, b any) (bool, error) {
	num := func(v any) (float64, *big3, bool) {
		switch t := v.(type) {
		case bool:
			if t {
				return 1, nil, true
			}
			return 0, nil, true
		case int64:
			return float64(t), &big3{i: t}, true
		case uint64:
			return float64(t), &big3{u: t, big: true}, true
		case float64:
			return t, nil, true
		}
		return 0, nil, false
	}
	af, ai, aok := num(a)
	bf, bi, bok := num(b)
	if aok && bok {
		if ai != nil && bi != nil {
			return ai.greater(bi), nil
		}
		return af > bf, nil
	}
	as, aStr := a.(string)
	bs, bStr := b.(string)
	if aStr && bStr {
		return as > bs, nil
	}
	return false, &GGUFError{"TypeError", "'>' not supported between instances of '" + ggufTypeName(a) + "' and '" + ggufTypeName(b) + "'"}
}

// Exact integer used to compare int64 and large uint64 metadata values
type big3 struct {
	i   int64
	u   uint64
	big bool
}

func (x *big3) greater(y *big3) bool {
	switch {
	case x.big && y.big:
		return x.u > y.u
	case x.big:
		return true
	case y.big:
		return false
	}
	return x.i > y.i
}

// Attention head count, the maximum when the file stores one per layer
func (h *GGUFHeader) NHead() (int64, error) {
	v := h.archKey("attention.head_count")
	if list, ok := v.([]any); ok {
		m, err := ggufListMax(list)
		if err != nil {
			return 0, err
		}
		return ggufInt(m)
	}
	return ggufIntOrZero(v)
}

// Per layer KV head counts where zero marks a recurrent or linear layer
func (h *GGUFHeader) HeadCountsKV() ([]int64, error) {
	v := h.archKey("attention.head_count_kv")
	if list, ok := v.([]any); ok {
		out := make([]int64, len(list))
		for i, x := range list {
			n, err := ggufInt(x)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	}
	scalar, err := ggufIntOrZero(v)
	if err != nil {
		return nil, err
	}
	interval, err := h.FullAttentionInterval()
	if err != nil {
		return nil, err
	}
	nLayer, err := h.NLayer()
	if err != nil {
		return nil, err
	}
	out := make([]int64, max(0, nLayer))
	for i := range out {
		if interval <= 1 || int64(i+1)%interval == 0 {
			out[i] = scalar
		}
	}
	return out, nil
}

// Per head key size from key_length, else embedding size over head count
func (h *GGUFHeader) HeadDimK() (int64, error) {
	if v := h.archKey("attention.key_length"); pyTruthy(v) {
		return ggufInt(v)
	}
	nHead, err := h.NHead()
	if err != nil || nHead == 0 {
		return 0, err
	}
	nEmbd, err := h.NEmbd()
	if err != nil {
		return 0, err
	}
	return pyFloorDiv(nEmbd, nHead), nil
}

// Per head value size from value_length, else the key size
func (h *GGUFHeader) HeadDimV() (int64, error) {
	if v := h.archKey("attention.value_length"); pyTruthy(v) {
		return ggufInt(v)
	}
	return h.HeadDimK()
}

// Path text as pathlib renders it, separators collapsed and dot components dropped
func ggufPathStr(p string) string {
	if p == "" {
		return "."
	}
	p = filepath.FromSlash(p)
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	prefix := ""
	if strings.HasPrefix(rest, sep) {
		prefix = sep
		if sep == "/" && strings.HasPrefix(rest, "//") && !strings.HasPrefix(rest, "///") {
			prefix = "//"
		}
	}
	var parts []string
	for _, s := range strings.Split(rest, sep) {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	out := vol + prefix + strings.Join(parts, sep)
	if out == "" {
		return "."
	}
	return out
}

// Python floor division of two int64 values
func pyFloorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Sum capped at the int64 maximum so corrupt tensor tables price huge instead of wrapping
func ggufSatAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Product capped at the int64 maximum for non negative operands
func ggufSatMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// Every on disk part of the split path belongs to, first first, nil unless another part exists
func SplitParts(path string) []string {
	path = ggufPathStr(path)
	name := filepath.Base(path)
	m := GGUFSplitPartRE.FindStringSubmatchIndex(name)
	if m == nil {
		return nil
	}
	stem := name[:m[0]]
	total, err := ggufParseIntString(name[m[4]:m[5]])
	if err != nil {
		return nil
	}
	dir := filepath.Dir(path)
	var parts []string
	for i := int64(1); i <= total; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%s-%05d-of-%05d.gguf", stem, i, total))
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			parts = append(parts, p)
		}
	}
	if len(parts) > 1 {
		return parts
	}
	return nil
}

// Sequential reader over one GGUF file that fails like the Python struct reads
type ggufReader struct {
	r    io.Reader
	path string
}

func (g *ggufReader) truncated() error {
	return &GGUFError{"ValueError", "truncated GGUF header: " + g.path}
}

func (g *ggufReader) exact(n int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(g.r, n))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != n {
		return nil, g.truncated()
	}
	return data, nil
}

func (g *ggufReader) u32() (uint32, error) {
	b, err := g.exact(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (g *ggufReader) u64() (uint64, error) {
	b, err := g.exact(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// Length prefixed string decoded as UTF-8 with replacement, short reads kept as Python does
func (g *ggufReader) str() (string, error) {
	n, err := g.u64()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(g.r, int64(min(n, math.MaxInt64))))
	if err != nil {
		return "", err
	}
	return ggufDecodeUTF8(data), nil
}

// UTF-8 decode with errors replace that keeps a leading byte order mark
func ggufDecodeUTF8(b []byte) string {
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		return "\ufeff" + ggufDecodeUTF8(b[3:])
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return decodeUTF8Replace(b)
}

func (g *ggufReader) value(vtype uint32) (any, error) {
	switch vtype {
	case ggufVString:
		return g.str()
	case ggufVArray:
		etype, err := g.u32()
		if err != nil {
			return nil, err
		}
		n, err := g.u64()
		if err != nil {
			return nil, err
		}
		out := []any{}
		for i := uint64(0); i < n; i++ {
			v, err := g.value(etype)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	sizes := map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}
	size, ok := sizes[vtype]
	if !ok {
		return nil, &GGUFError{"KeyError", strconv.FormatUint(uint64(vtype), 10)}
	}
	b, err := g.exact(size)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	switch vtype {
	case 0:
		return int64(b[0]), nil
	case 1:
		return int64(int8(b[0])), nil
	case 2:
		return int64(le.Uint16(b)), nil
	case 3:
		return int64(int16(le.Uint16(b))), nil
	case 4:
		return int64(le.Uint32(b)), nil
	case 5:
		return int64(int32(le.Uint32(b))), nil
	case 6:
		return float64(math.Float32frombits(le.Uint32(b))), nil
	case 7:
		return b[0] != 0, nil
	case 10:
		u := le.Uint64(b)
		if u > math.MaxInt64 {
			return u, nil
		}
		return int64(u), nil
	case 11:
		return int64(le.Uint64(b)), nil
	}
	return math.Float64frombits(le.Uint64(b)), nil
}

// One file's own header with its metadata and its own tensor table
func readGGUFPart(path string) (*GGUFHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	g := &ggufReader{r: bufio.NewReader(f), path: path}
	magic, err := io.ReadAll(io.LimitReader(g.r, 4))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(magic, ggufMagic) {
		return nil, &GGUFError{"ValueError", "not a GGUF file: " + path}
	}
	version, err := g.u32()
	if err != nil {
		return nil, err
	}
	nTensors, err := g.u64()
	if err != nil {
		return nil, err
	}
	nKV, err := g.u64()
	if err != nil {
		return nil, err
	}
	h := &GGUFHeader{Path: path, Version: version, Metadata: map[string]any{},
		NTensors: int64(min(nTensors, math.MaxInt64)), FFNBlockBytes: map[int]int64{},
		LazyCandidateBytes: map[string]int64{}, RecurrentBlocks: map[int]bool{}}
	for i := uint64(0); i < nKV; i++ {
		key, err := g.str()
		if err != nil {
			return nil, err
		}
		vtype, err := g.u32()
		if err != nil {
			return nil, err
		}
		v, err := g.value(vtype)
		if err != nil {
			return nil, err
		}
		if _, dup := h.Metadata[key]; !dup {
			h.MetadataKeys = append(h.MetadataKeys, key)
		}
		h.Metadata[key] = v
	}
	for i := uint64(0); i < nTensors; i++ {
		if err := readGGUFTensor(g, h); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// One tensor info entry folded into the header's byte totals and block maps
func readGGUFTensor(g *ggufReader, h *GGUFHeader) error {
	name, err := g.str()
	if err != nil {
		return err
	}
	nDims, err := g.u32()
	if err != nil {
		return err
	}
	raw, err := g.exact(8 * int64(nDims))
	if err != nil {
		return err
	}
	ttype, err := g.u32()
	if err != nil {
		return err
	}
	if _, err := io.ReadAll(io.LimitReader(g.r, 8)); err != nil {
		return err
	}
	size, ok := ggmlTypeSizes[ttype]
	if !ok {
		return &GGUFError{"ValueError", fmt.Sprintf("unknown ggml tensor type %d in %s", ttype, g.path)}
	}
	elems := int64(1)
	for i := 0; i < int(nDims); i++ {
		d := binary.LittleEndian.Uint64(raw[8*i:])
		elems = ggufSatMul(elems, int64(min(d, math.MaxInt64)))
	}
	nbytes := ggufSatMul(elems/size[1], size[0])
	h.TensorBytes = ggufSatAdd(h.TensorBytes, nbytes)
	if name == "token_embd.weight" {
		h.EmbdTableBytes = nbytes
	} else if m := ggufFFNWeightRE.FindStringSubmatch(name); m != nil {
		block := ggufBlockIndex(m[1])
		h.FFNBlockBytes[block] = ggufSatAdd(h.FFNBlockBytes[block], nbytes)
	} else if ggufLazyReadCandidates[name] {
		h.LazyCandidateBytes[name] = nbytes
	} else if m := ggufSSMConvRE.FindStringSubmatch(name); m != nil {
		h.RecurrentBlocks[ggufBlockIndex(m[1])] = true
	}
	return nil
}

// Block index from a decimal digit run, capped at the int maximum
func ggufBlockIndex(s string) int {
	n, err := ggufParseIntString(s)
	if err != nil {
		return math.MaxInt
	}
	return int(n)
}

// Header for a model, a split priced as the sum of its readable parts with metadata from part one
func ReadGGUFHeader(path string) (*GGUFHeader, error) {
	path = ggufPathStr(path)
	parts := SplitParts(path)
	if parts == nil {
		return readGGUFPart(path)
	}
	first, err := readGGUFPart(parts[0])
	if err != nil {
		return nil, err
	}
	readable := []*GGUFHeader{first}
	for _, part := range parts[1:] {
		h, err := readGGUFPart(part)
		if err != nil {
			if !GGUFReadSkippable(err) {
				return nil, err
			}
			slog.Debug("split part unreadable", "part", filepath.Base(part), "err", err)
			continue
		}
		readable = append(readable, h)
	}
	if len(readable) == 1 {
		return first, nil
	}
	out := &GGUFHeader{Path: path, Version: first.Version, Metadata: first.Metadata,
		MetadataKeys: first.MetadataKeys, FFNBlockBytes: map[int]int64{},
		LazyCandidateBytes: map[string]int64{}, RecurrentBlocks: map[int]bool{}}
	for _, h := range readable {
		out.NTensors = ggufSatAdd(out.NTensors, h.NTensors)
		out.TensorBytes = ggufSatAdd(out.TensorBytes, h.TensorBytes)
		out.EmbdTableBytes = ggufSatAdd(out.EmbdTableBytes, h.EmbdTableBytes)
		for block, n := range h.FFNBlockBytes {
			out.FFNBlockBytes[block] = ggufSatAdd(out.FFNBlockBytes[block], n)
		}
		for name, n := range h.LazyCandidateBytes {
			out.LazyCandidateBytes[name] = n
		}
		for block := range h.RecurrentBlocks {
			out.RecurrentBlocks[block] = true
		}
	}
	return out, nil
}
