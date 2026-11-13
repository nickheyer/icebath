package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	yaml11Null      = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	yaml11Merge     = regexp.MustCompile(`^<<$`)
	yaml11Value     = regexp.MustCompile(`^=$`)
	yaml11Timestamp = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
)

const (
	safeMaxSimpleKey = 128
)

var yamlEscapes = map[rune]string{
	0: "0", 0x07: "a", 0x08: "b", 0x09: "t", 0x0A: "n", 0x0B: "v", 0x0C: "f", 0x0D: "r", 0x1B: "e",
	'"': "\"", '\\': "\\", 0x85: "N", 0xA0: "_", 0x2028: "L", 0x2029: "P",
}

// Formatting choices of safe_dump, DefaultSafeDumpOptions holds the values a caller gets by passing none
type SafeDumpOptions struct {
	DefaultFlowStyle bool
	SortKeys         bool
	AllowUnicode     bool
	Width            int
	Indent           *SafeDumpIndent
	Version          string
}

// Block indentation of mappings and sequences and the dash offset inside a sequence indent
type SafeDumpIndent struct {
	Mapping, Sequence, Offset int
}

// Indentation of the shared YAML writer, mapping 2, sequence 4 and dash offset 2
var hermesDumpIndent = SafeDumpIndent{Mapping: 2, Sequence: 4, Offset: 2}

// Indentation of a ruamel YAML instance left at its defaults, sequences indentless under a mapping key
var RuamelDefaultIndent = SafeDumpIndent{Mapping: 2, Sequence: 2, Offset: 0}

// Options safe_dump writes with when the caller passes none
func DefaultSafeDumpOptions() SafeDumpOptions {
	return SafeDumpOptions{SortKeys: true, AllowUnicode: true, Width: 80}
}

// YAML text of data as safe_dump writes it with sort_keys off, the setting every caller in the tree passes
func SafeDump(data any) ([]byte, error) {
	opts := DefaultSafeDumpOptions()
	opts.SortKeys = false
	return SafeDumpWith(data, opts)
}

// YAML text of data under the given safe_dump options
func SafeDumpWith(data any, opts SafeDumpOptions) ([]byte, error) {
	var b strings.Builder
	if err := SafeDumpTo(&b, data, opts); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// Writes data to w as YAML under the given options, nothing written when the data cannot be represented
func SafeDumpTo(w io.Writer, data any, opts SafeDumpOptions) error {
	r := &safeRepresenter{opts: opts, objects: map[aliasKey]*dumpNode{}}
	root, err := r.represent(data)
	if err != nil {
		return err
	}
	e := newSafeEmitter(opts, root)
	e.document(root)
	_, err = io.WriteString(w, e.out.String())
	return err
}

var safeDumpImpl = SafeDump

// Replaces the dumper behind AtomicYAMLWrite and returns the restore func, for tests simulating a failed dump
func SwapSafeDump(f func(any) ([]byte, error)) func() {
	old := safeDumpImpl
	safeDumpImpl = f
	return func() { safeDumpImpl = old }
}

// Writes data as YAML plus extra through a synced temp file, keeping the existing mode and owner
func AtomicYAMLWrite(ctx context.Context, path string, data any, extra string) error {
	return atomicYAMLWrite(ctx, path, data, extra, 0, false)
}

// Writes like AtomicYAMLWrite with a new file created at the given mode
func AtomicYAMLWriteCreateMode(ctx context.Context, path string, data any, extra string, createMode os.FileMode) error {
	return atomicYAMLWrite(ctx, path, data, extra, pyfs.PosixModeBits(createMode), true)
}

// Safe dumped YAML plus extra written with the existing bits kept or the create mode for a new file
func atomicYAMLWrite(ctx context.Context, path string, data any, extra string, createMode uint32, createSet bool) error {
	text, err := safeDumpImpl(data)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	mode, ok := pyfs.ModeForWrite(path, createMode, createSet, true)
	return atomicWriteDoc(ctx, path, append(text, extra...), pyfs.WriteOptions{Prefix: "." + stem + "_", Mode: mode, ModeSet: ok, PreserveOwner: true})
}

// Represented node of the safe representer, a scalar, sequence or mapping with alternating keys and values
type dumpNode struct {
	kind    byte
	isStr   bool
	value   string
	items   []*dumpNode
	flow    bool
	tag     string
	literal bool
}

// Identity of a map or list, the Python id that makes a second occurrence an alias
type aliasKey struct {
	ptr uintptr
	n   int
}

type safeRepresenter struct {
	opts    SafeDumpOptions
	objects map[aliasKey]*dumpNode
}

// Node graph of data the way SafeRepresenter builds it, shared maps and lists becoming one node
func (r *safeRepresenter) represent(v any) (*dumpNode, error) {
	switch x := v.(type) {
	case Tree:
		return r.represent(map[string]any(x))
	case map[string]any:
		keys := MappingKeys(x)
		pyKeys := make([]any, len(keys))
		for i, k := range keys {
			pyKeys[i] = k
			if pk, ok := MappingPyKey(x, k); ok {
				pyKeys[i] = pk
			}
		}
		return r.mapping(reflect.ValueOf(x), pyKeys, func(i int) any { return x[keys[i]] })
	case []any:
		return r.sequence(reflect.ValueOf(x))
	case time.Time:
		return &dumpNode{kind: 's', value: representTime(x)}, nil
	case pydatetime.Date:
		return &dumpNode{kind: 's', value: x.PyStr()}, nil
	case pydatetime.DateTime:
		return &dumpNode{kind: 's', value: x.PyStr()}, nil
	case *pydatetime.Date, *pydatetime.DateTime, *time.Time:
		return r.sharedScalar(reflect.ValueOf(x))
	case []byte:
		return representBinaryAsLiteralBlock(x)
	}
	if text, isStr, ok := representScalar(v); ok {
		return &dumpNode{kind: 's', isStr: isStr, value: text}, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		switch rv.Type().Key().Kind() {
		case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			keys := sortedReflectKeys(rv)
			return r.mapping(rv, keys, func(i int) any {
				return rv.MapIndex(reflect.ValueOf(keys[i]).Convert(rv.Type().Key())).Interface()
			})
		}
	case reflect.Slice:
		if rv.Type().Elem().Kind() != reflect.Uint8 {
			return r.sequence(rv)
		}
	case reflect.Array:
		return r.sequence(rv)
	}
	return nil, &YAMLError{fmt.Errorf("cannot represent an object: %s", pyrepr.Repr(v))}
}

// Keys of a Go map in sorted order, the order an unordered mapping is iterated in
func sortedReflectKeys(rv reflect.Value) []any {
	keys := rv.MapKeys()
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k.Interface()
	}
	sortPyKeys(out)
	return out
}

// Sorts str or int keys as Python sorted does, a mix of kinds staying in place like the TypeError path
func sortPyKeys(keys []any) {
	ints := make([]*big.Int, len(keys))
	allStr, allInt := true, true
	for i, k := range keys {
		_, isStr := k.(string)
		allStr = allStr && isStr
		n, isInt := pyrepr.Int(k)
		ints[i] = n
		allInt = allInt && isInt
	}
	switch {
	case allStr:
		sort.SliceStable(keys, func(i, j int) bool { return keys[i].(string) < keys[j].(string) })
	case allInt:
		idx := make([]int, len(keys))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(i, j int) bool { return ints[idx[i]].Cmp(ints[idx[j]]) < 0 })
		sorted := make([]any, len(keys))
		for i, at := range idx {
			sorted[i] = keys[at]
		}
		copy(keys, sorted)
	}
}

// Alias identity of a map or non empty list value, false for values Python never aliases
func identityOf(rv reflect.Value) (aliasKey, bool) {
	switch rv.Kind() {
	case reflect.Map:
		if rv.IsNil() {
			return aliasKey{}, false
		}
		return aliasKey{ptr: uintptr(rv.UnsafePointer())}, true
	case reflect.Slice:
		if rv.Cap() == 0 {
			return aliasKey{}, false
		}
		return aliasKey{ptr: uintptr(rv.UnsafePointer()), n: rv.Len()}, true
	case reflect.Pointer:
		if rv.IsNil() {
			return aliasKey{}, false
		}
		return aliasKey{ptr: uintptr(rv.UnsafePointer())}, true
	}
	return aliasKey{}, false
}

// One scalar node per date or datetime object, so a second reference to the same object becomes an alias
func (r *safeRepresenter) sharedScalar(rv reflect.Value) (*dumpNode, error) {
	id, aliased := identityOf(rv)
	if !aliased {
		return r.represent(nil)
	}
	if n, ok := r.objects[id]; ok {
		return n, nil
	}
	n, err := r.represent(rv.Elem().Interface())
	if err != nil {
		return nil, err
	}
	r.objects[id] = n
	return n, nil
}

// Mapping node with keys in insertion order or Python sorted order when sort_keys asks, values in that order
func (r *safeRepresenter) mapping(rv reflect.Value, keys []any, value func(i int) any) (*dumpNode, error) {
	id, aliased := identityOf(rv)
	if aliased {
		if n, ok := r.objects[id]; ok {
			return n, nil
		}
	}
	node := &dumpNode{kind: 'm', flow: r.opts.DefaultFlowStyle}
	if aliased {
		r.objects[id] = node
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	if r.opts.SortKeys {
		order = pySortedOrder(keys)
	}
	for _, i := range order {
		kn, err := r.represent(keys[i])
		if err != nil {
			return nil, err
		}
		vn, err := r.represent(value(i))
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, kn, vn)
	}
	return node, nil
}

// Sequence node of a list or array value
func (r *safeRepresenter) sequence(rv reflect.Value) (*dumpNode, error) {
	id, aliased := identityOf(rv)
	if aliased {
		if n, ok := r.objects[id]; ok {
			return n, nil
		}
	}
	node := &dumpNode{kind: 'q', flow: r.opts.DefaultFlowStyle}
	if aliased {
		r.objects[id] = node
	}
	for i := 0; i < rv.Len(); i++ {
		item, err := r.represent(rv.Index(i).Interface())
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, item)
	}
	return node, nil
}

// Text of a scalar and whether it carries the str tag, false when the value is no scalar
func representScalar(v any) (string, bool, bool) {
	switch x := v.(type) {
	case nil:
		return "null", false, true
	case string:
		return x, true, true
	case bool:
		if x {
			return "true", false, true
		}
		return "false", false, true
	case float32:
		return representFloat(float64(x)), false, true
	case float64:
		return representFloat(x), false, true
	}
	if n, ok := pyrepr.Int(v); ok {
		return n.String(), false, true
	}
	return "", false, false
}

func representFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	return strings.ToLower(pyrepr.Float(f))
}

// Date isoformat for a midnight UTC value, else the datetime isoformat with a space separator
func representTime(t time.Time) string {
	if t.Location() == time.UTC && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return t.Format("2006-01-02")
	}
	s := t.Format("2006-01-02 15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if t.Location() != time.UTC {
		s += t.Format("-07:00")
	}
	return s
}

// Whether the plain text of a str resolves back to str under the YAML 1.1 implicit resolvers
func plainResolvesToStr(s string) bool {
	for _, re := range []*regexp.Regexp{yaml11Bool, yaml11Int, yaml11Float, yaml11Null, yaml11Merge, yaml11Timestamp, yaml11Value} {
		if re.MatchString(s) {
			return false
		}
	}
	return true
}

type safeIndent struct {
	n         int
	none, seq bool
}

type scalarAnalysis struct {
	empty, multiline                                          bool
	allowFlowPlain, allowBlockPlain, allowSingle, allowDouble bool
}

// Port of the ruamel emitter states for the events a safe representer produces
type safeEmitter struct {
	out                   strings.Builder
	ind                   SafeDumpIndent
	version               string
	allowUnicode          bool
	bestWidth             int
	column, indent        int
	indentNone            bool
	indents               []safeIndent
	flowContext           []string
	whitespace, indention bool
	openEnded, noNewline  bool
	rootCtx, seqCtx       bool
	mapCtx, simpleKey     bool
	anchors               map[*dumpNode]string
	serialized            map[*dumpNode]bool
	lastAnchorID          int
	anchorsSeen           map[*dumpNode]bool
}

func newSafeEmitter(opts SafeDumpOptions, root *dumpNode) *safeEmitter {
	e := &safeEmitter{allowUnicode: opts.AllowUnicode, bestWidth: 80, whitespace: true, indention: true, indentNone: true,
		ind: hermesDumpIndent, version: opts.Version,
		anchors: map[*dumpNode]string{}, serialized: map[*dumpNode]bool{}, anchorsSeen: map[*dumpNode]bool{}}
	if opts.Indent != nil {
		e.ind = *opts.Indent
	}
	if opts.Width > 2*2 {
		e.bestWidth = opts.Width
	}
	e.anchorNode(root)
	return e
}

// Marks nodes reached twice with idNNN anchors in the order the serializer meets their second occurrence
func (e *safeEmitter) anchorNode(n *dumpNode) {
	if e.anchorsSeen[n] {
		if _, ok := e.anchors[n]; !ok {
			e.lastAnchorID++
			e.anchors[n] = fmt.Sprintf("id%03d", e.lastAnchorID)
		}
		return
	}
	e.anchorsSeen[n] = true
	for _, c := range n.items {
		e.anchorNode(c)
	}
}

// Emits the implicit document and stream end, closing an open ended root scalar with the document end marker
func (e *safeEmitter) document(root *dumpNode) {
	if e.version != "" {
		e.out.WriteString("%YAML " + e.version)
		e.writeLineBreak("")
		e.writeIndicator("---", true, false, false)
	}
	e.expectNode(root, true, false, false, false)
	e.writeIndent()
	if e.openEnded {
		e.writeIndicator("...", true, false, false)
		e.writeIndent()
	}
}

func (e *safeEmitter) flowLevel() int { return len(e.flowContext) }

func (e *safeEmitter) lastSeq() bool {
	return len(e.indents) >= 2 && e.indents[len(e.indents)-2].seq
}

func (e *safeEmitter) seqSeq() bool {
	return len(e.indents) >= 2 && e.indents[len(e.indents)-2].seq && e.indents[len(e.indents)-1].seq
}

// Extra spaces before a flow collection opened as a sequence item, the dash counted
func (e *safeEmitter) seqFlowAlign() int {
	if len(e.indents) < 2 || !e.indents[len(e.indents)-1].seq {
		return 0
	}
	top := e.indents[len(e.indents)-1]
	base := 0
	if !top.none {
		base = top.n
	}
	return base + e.ind.Sequence - e.column - 1
}

func (e *safeEmitter) increaseIndent(flow, seq, indentless bool) {
	e.indents = append(e.indents, safeIndent{n: e.indent, none: e.indentNone, seq: seq})
	if e.indentNone {
		if !flow {
			e.indent, e.indentNone = 0, false
		}
		return
	}
	if !indentless {
		if e.lastSeq() {
			e.indent += e.ind.Sequence
		} else {
			e.indent += e.ind.Mapping
		}
	}
}

func (e *safeEmitter) popIndent() {
	top := e.indents[len(e.indents)-1]
	e.indents = e.indents[:len(e.indents)-1]
	e.indent, e.indentNone = top.n, top.none
}

func (e *safeEmitter) write(s string) {
	e.out.WriteString(s)
	e.column += len([]rune(s))
}

func (e *safeEmitter) writeLineBreak(s string) {
	if s == "" {
		s = "\n"
	}
	e.whitespace, e.indention = true, true
	e.column = 0
	e.out.WriteString(s)
}

func (e *safeEmitter) writeIndent() {
	indent := 0
	if !e.indentNone {
		indent = e.indent
	}
	if !e.indention || e.column > indent || (e.column == indent && !e.whitespace) {
		if e.noNewline {
			e.noNewline = false
		} else {
			e.writeLineBreak("")
		}
	}
	if e.column < indent {
		e.whitespace = true
		e.out.WriteString(strings.Repeat(" ", indent-e.column))
		e.column = indent
	}
}

func (e *safeEmitter) writeIndicator(ind string, needWhitespace, whitespace, indention bool) {
	if !e.whitespace && needWhitespace {
		ind = " " + ind
	}
	e.whitespace = whitespace
	e.indention = e.indention && indention
	e.openEnded = false
	e.write(ind)
}

// Writes an anchor or alias indicator for a node the serializer marked, reporting whether one was written
func (e *safeEmitter) processAnchor(indicator string, n *dumpNode) bool {
	anchor, ok := e.anchors[n]
	if !ok {
		return false
	}
	e.writeIndicator(indicator+anchor, true, false, false)
	e.noNewline = false
	return true
}

// Emits one node, an alias when the serializer already wrote it, in block or flow layout
func (e *safeEmitter) expectNode(n *dumpNode, root, seq, mapping, simpleKey bool) {
	e.rootCtx, e.seqCtx, e.mapCtx, e.simpleKey = root, seq, mapping, simpleKey
	if e.serialized[n] {
		e.processAnchor("*", n)
		return
	}
	e.serialized[n] = true
	if e.processAnchor("&", n) && n.kind == 's' && e.seqCtx {
		e.seqCtx = false
	}
	switch n.kind {
	case 's':
		if n.tag != "" {
			e.writeIndicator(n.tag, true, false, false)
			if e.seqCtx && e.flowLevel() == 0 {
				e.noNewline = true
			}
		}
		e.increaseIndent(true, false, false)
		e.processScalar(n)
		e.popIndent()
	case 'q':
		if e.flowLevel() > 0 || n.flow || len(n.items) == 0 {
			e.expectFlowSequence(n)
		} else {
			e.expectBlockSequence(n)
		}
	case 'm':
		if e.flowLevel() > 0 || n.flow || len(n.items) == 0 {
			e.expectFlowMapping(n)
		} else {
			e.expectBlockMapping(n)
		}
	}
}

// Sequence context bookkeeping right after a sequence starts, before its first item
func (e *safeEmitter) afterSequenceStart() {
	if e.seqSeq() {
		e.indention, e.noNewline = true, false
	}
}

func (e *safeEmitter) expectFlowSequence(n *dumpNode) {
	ind := e.seqFlowAlign()
	e.writeIndicator(strings.Repeat(" ", max(ind, 0))+"[", true, true, false)
	e.increaseIndent(true, true, false)
	e.flowContext = append(e.flowContext, "[")
	e.afterSequenceStart()
	for i, item := range n.items {
		if i > 0 {
			e.writeIndicator(",", false, false, false)
		}
		if e.column > e.bestWidth {
			e.writeIndent()
		}
		e.expectNode(item, false, true, false, false)
	}
	e.popIndent()
	e.flowContext = e.flowContext[:len(e.flowContext)-1]
	e.writeIndicator("]", false, false, false)
	if len(n.items) == 0 {
		if e.flowLevel() == 0 {
			e.writeLineBreak("")
		}
		return
	}
	e.noNewline = false
}

func (e *safeEmitter) expectFlowMapping(n *dumpNode) {
	ind := e.seqFlowAlign()
	open := "{"
	if len(n.items) == 2 && e.flowLevel() > 0 && e.flowContext[len(e.flowContext)-1] == "[" {
		open = ""
	}
	e.writeIndicator(strings.Repeat(" ", max(ind, 0))+open, true, true, false)
	e.flowContext = append(e.flowContext, open)
	e.increaseIndent(true, false, false)
	for i := 0; i+1 < len(n.items); i += 2 {
		if i > 0 {
			e.writeIndicator(",", false, false, false)
		}
		if e.column > e.bestWidth {
			e.writeIndent()
		}
		key, value := n.items[i], n.items[i+1]
		if e.checkSimpleKey(key) {
			e.expectNode(key, false, false, true, true)
			e.writeIndicator(":", false, false, false)
			e.expectNode(value, false, false, true, false)
			continue
		}
		e.writeIndicator("?", true, false, false)
		e.expectNode(key, false, false, true, false)
		if e.column > e.bestWidth {
			e.writeIndent()
		}
		e.writeIndicator(":", true, false, false)
		e.expectNode(value, false, false, true, false)
	}
	e.popIndent()
	popped := e.flowContext[len(e.flowContext)-1]
	e.flowContext = e.flowContext[:len(e.flowContext)-1]
	if len(n.items) == 0 {
		e.writeIndicator("}", false, false, false)
		if e.flowLevel() == 0 {
			e.writeLineBreak("")
		}
		return
	}
	if popped != "" {
		e.writeIndicator("}", false, false, false)
	}
	e.noNewline = false
}

func (e *safeEmitter) expectBlockSequence(n *dumpNode) {
	indentless := e.mapCtx && !e.indention
	e.increaseIndent(false, true, indentless)
	e.afterSequenceStart()
	for _, item := range n.items {
		nonl := false
		if e.column == 0 {
			nonl = e.noNewline
		}
		e.writeIndent()
		e.writeIndicator(strings.Repeat(" ", e.ind.Offset)+"-", true, false, true)
		if nonl || e.ind.Offset+2 > e.ind.Sequence {
			e.noNewline = true
		}
		e.expectNode(item, false, true, false, false)
	}
	e.popIndent()
	e.noNewline = false
}

func (e *safeEmitter) expectBlockMapping(n *dumpNode) {
	e.increaseIndent(false, false, false)
	for i := 0; i+1 < len(n.items); i += 2 {
		key, value := n.items[i], n.items[i+1]
		e.writeIndent()
		if e.checkSimpleKey(key) {
			e.expectNode(key, false, false, true, true)
			e.writeIndicator(":", false, false, false)
			e.expectNode(value, false, false, true, false)
			continue
		}
		e.writeIndicator("?", true, false, true)
		e.expectNode(key, false, false, true, false)
		e.writeIndent()
		e.writeIndicator(":", true, false, true)
		e.expectNode(value, false, false, true, false)
	}
	e.popIndent()
}

// Whether a key fits the simple form, its tag and text under 128 characters on one line
func (e *safeEmitter) checkSimpleKey(n *dumpNode) bool {
	tag := "!!str"
	if n.tag != "" {
		tag = n.tag
	}
	length := len(tag) + len([]rune(n.value))
	return length < safeMaxSimpleKey && !analyzeScalar([]rune(n.value), e.allowUnicode).multiline
}

func isBreakRune(r rune) bool { return r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 }

func isBlankRune(r rune) bool { return r == 0 || r == ' ' || r == '\t' || r == '\r' || isBreakRune(r) }

func printableUnicode(r rune) bool {
	return r == 0x85 || (r >= 0xA0 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

// Styles a scalar may take, the ruamel analyze_scalar rules for an unversioned stream
func analyzeScalar(s []rune, allowUnicode bool) scalarAnalysis {
	if len(s) == 0 {
		return scalarAnalysis{empty: true, allowBlockPlain: true, allowSingle: true, allowDouble: true}
	}
	text := string(s)
	blockInd := strings.HasPrefix(text, "---") || strings.HasPrefix(text, "...")
	flowInd := blockInd
	var lineBreaks, special, leadSpace, leadBreak, trailSpace, trailBreak, breakSpace, spaceBreak, prevSpace, prevBreak bool
	preceded := true
	followed := len(s) == 1 || isBlankRune(s[1])
	for i, ch := range s {
		if i == 0 {
			if strings.ContainsRune("#,[]{}&*!|>'\"%@`", ch) {
				flowInd, blockInd = true, true
			}
			if ch == '?' || ch == ':' {
				if len(s) == 1 {
					flowInd = true
				}
				if followed {
					blockInd = true
				}
			}
			if ch == '-' && followed {
				flowInd, blockInd = true, true
			}
		} else {
			if strings.ContainsRune(",[]{}", ch) {
				flowInd = true
			}
			if ch == ':' && followed {
				flowInd, blockInd = true, true
			}
			if ch == '#' && preceded {
				flowInd, blockInd = true, true
			}
		}
		if isBreakRune(ch) {
			lineBreaks = true
		}
		if !(ch == '\n' || (ch >= 0x20 && ch <= 0x7E)) {
			if printableUnicode(ch) && ch != 0xFEFF {
				if !allowUnicode {
					special = true
				}
			} else {
				special = true
			}
		}
		switch {
		case ch == ' ':
			leadSpace = leadSpace || i == 0
			trailSpace = trailSpace || i == len(s)-1
			breakSpace = breakSpace || prevBreak
			prevSpace, prevBreak = true, false
		case isBreakRune(ch):
			leadBreak = leadBreak || i == 0
			trailBreak = trailBreak || i == len(s)-1
			spaceBreak = spaceBreak || prevSpace
			prevSpace, prevBreak = false, true
		default:
			prevSpace, prevBreak = false, false
		}
		preceded = isBlankRune(ch)
		followed = i+2 >= len(s) || isBlankRune(s[i+2])
	}
	a := scalarAnalysis{multiline: lineBreaks, allowFlowPlain: true, allowBlockPlain: true, allowSingle: true, allowDouble: true}
	if leadSpace || leadBreak || trailSpace || trailBreak {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if breakSpace || special || spaceBreak {
		a.allowFlowPlain, a.allowBlockPlain, a.allowSingle = false, false, false
	}
	if lineBreaks {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if flowInd {
		a.allowFlowPlain = false
	}
	if blockInd {
		a.allowBlockPlain = false
	}
	return a
}

// Picks plain, single or double quoting the way choose_scalar_style does and writes the scalar
func (e *safeEmitter) processScalar(n *dumpNode) {
	s := []rune(n.value)
	a := analyzeScalar(s, e.allowUnicode)
	split := !e.simpleKey
	implicit := !n.isStr || plainResolvesToStr(n.value)
	plainOK := (e.flowLevel() > 0 && a.allowFlowPlain) || (e.flowLevel() == 0 && a.allowBlockPlain)
	if e.seqCtx && e.flowLevel() == 0 {
		e.writeIndent()
	}
	if n.literal {
		if e.flowLevel() == 0 && !e.simpleKey {
			e.writeLiteral(s)
		} else {
			e.writeDouble(s, split)
		}
		return
	}
	switch {
	case implicit && !(e.simpleKey && (a.empty || a.multiline)) && plainOK:
		e.writePlain(s, split)
	case a.allowDouble && (strings.ContainsRune(n.value, '\'') || strings.ContainsRune(n.value, '\n')):
		e.writeDouble(s, split)
	case a.allowSingle && !(e.simpleKey && a.multiline):
		e.writeSingle(s, split)
	default:
		e.writeDouble(s, split)
	}
}

// Chomping and indentation hints of a block scalar with the indent a root scalar needs and its chomping indicator
func (e *safeEmitter) blockHints(s []rune) (string, int, string) {
	if len(s) == 0 {
		return "", 0, ""
	}
	indent, hints, indicator := 0, "", ""
	if s[0] == ' ' || isBreakRune(s[0]) {
		indent, hints = 2, "2"
	} else if e.rootCtx {
		text := string(s)
		position := -1
		for _, end := range []string{"---", "..."} {
			for pos := 0; position == -1; pos++ {
				at := strings.Index(text[pos:], end)
				if at == -1 {
					break
				}
				pos += at
				if pos != 0 && text[pos-1] != '\n' {
					continue
				}
				if next := pos + len(end); next < len(text) && strings.IndexByte(" \r\n", text[next]) >= 0 {
					position = pos
				}
			}
			if position != -1 {
				break
			}
		}
		if position != -1 {
			indent = 2
		}
	}
	if last := s[len(s)-1]; !isBreakRune(last) {
		indicator = "-"
	} else if len(s) == 1 || isBreakRune(s[len(s)-2]) {
		indicator = "+"
	}
	return hints + indicator, indent, indicator
}

// Writes a literal block scalar line by line the way write_literal does
func (e *safeEmitter) writeLiteral(s []rune) {
	hints, indent, indicator := e.blockHints(s)
	e.writeIndicator("|"+hints, true, false, false)
	if indicator == "+" {
		e.openEnded = true
	}
	e.writeLineBreak("")
	breaks, start := true, 0
	for end := 0; end <= len(s); end++ {
		has := end < len(s)
		var ch rune
		if has {
			ch = s[end]
		}
		if breaks {
			if !has || !isBreakRune(ch) {
				for _, br := range s[start:end] {
					if br == '\n' {
						e.writeLineBreak("")
					} else {
						e.writeLineBreak(string(br))
					}
				}
				if has {
					if e.rootCtx {
						pad := indent
						if !e.indentNone {
							pad += e.indent
						}
						e.out.WriteString(strings.Repeat(" ", pad))
					} else {
						e.writeIndent()
					}
				}
				start = end
			}
		} else if !has || isBreakRune(ch) {
			e.out.WriteString(string(s[start:end]))
			if !has {
				e.writeLineBreak("")
			}
			start = end
		}
		if has {
			breaks = isBreakRune(ch)
		}
	}
}

// Base64 text of bytes as a binary tagged literal block node, the 76 column lines base64 encodebytes writes
func representBinaryAsLiteralBlock(data []byte) (*dumpNode, error) {
	var b strings.Builder
	for start := 0; start < len(data); start += 57 {
		b.WriteString(base64.StdEncoding.EncodeToString(data[start:min(start+57, len(data))]))
		b.WriteByte('\n')
	}
	return &dumpNode{kind: 's', value: b.String(), tag: "!!binary", literal: true}, nil
}

func (e *safeEmitter) writePlain(s []rune, split bool) {
	if e.rootCtx {
		e.openEnded = true
	}
	if len(s) == 0 {
		return
	}
	if !e.whitespace {
		e.write(" ")
	}
	e.whitespace, e.indention = false, false
	spaces, breaks, start := false, false, 0
	for end := 0; end <= len(s); end++ {
		var ch rune = -1
		if end < len(s) {
			ch = s[end]
		}
		switch {
		case spaces:
			if ch != ' ' {
				if start+1 == end && e.column >= e.bestWidth && split {
					e.writeIndent()
					e.whitespace, e.indention = false, false
				} else {
					e.write(string(s[start:end]))
				}
				start = end
			}
		case breaks:
			if ch == -1 || !isBreakRune(ch) {
				if s[start] == '\n' {
					e.writeLineBreak("")
				}
				for _, br := range s[start:end] {
					if br == '\n' {
						e.writeLineBreak("")
					} else {
						e.writeLineBreak(string(br))
					}
				}
				e.writeIndent()
				e.whitespace, e.indention = false, false
				start = end
			}
		default:
			if ch == -1 || ch == ' ' || isBreakRune(ch) {
				data := s[start:end]
				if len(data)+e.column > e.bestWidth && !e.indentNone && e.column > e.indent {
					e.writeIndent()
				}
				e.write(string(data))
				start = end
			}
		}
		if ch != -1 {
			spaces, breaks = ch == ' ', isBreakRune(ch)
		}
	}
}

func (e *safeEmitter) writeSingle(s []rune, split bool) {
	e.writeIndicator("'", true, false, false)
	spaces, breaks, start := false, false, 0
	for end := 0; end <= len(s); end++ {
		var ch rune = -1
		if end < len(s) {
			ch = s[end]
		}
		switch {
		case spaces:
			if ch != ' ' {
				if start+1 == end && e.column > e.bestWidth && split && start != 0 && end != len(s) {
					e.writeIndent()
				} else {
					e.write(string(s[start:end]))
				}
				start = end
			}
		case breaks:
			if ch == -1 || !isBreakRune(ch) {
				if s[start] == '\n' {
					e.writeLineBreak("")
				}
				for _, br := range s[start:end] {
					if br == '\n' {
						e.writeLineBreak("")
					} else {
						e.writeLineBreak(string(br))
					}
				}
				e.writeIndent()
				start = end
			}
		default:
			if (ch == -1 || ch == ' ' || isBreakRune(ch) || ch == '\'') && start < end {
				e.write(string(s[start:end]))
				start = end
			}
		}
		if ch == '\'' {
			e.write("''")
			start = end + 1
		}
		if ch != -1 {
			spaces, breaks = ch == ' ', isBreakRune(ch)
		}
	}
	e.writeIndicator("'", false, false, false)
}

func indexRune(s []rune, r rune, from, to int) int {
	for i := from; i < to && i < len(s); i++ {
		if s[i] == r {
			return i
		}
	}
	return -1
}

// Double quoted text with escapes and the ruamel fold that drops the backslash before a plain space break
func (e *safeEmitter) writeDouble(s []rune, split bool) {
	e.writeIndicator("\"", true, false, false)
	start := 0
	for end := 0; end <= len(s); end++ {
		var ch rune = -1
		if end < len(s) {
			ch = s[end]
		}
		printable := (ch >= 0x20 && ch <= 0x7E) || (e.allowUnicode && ((ch >= 0xA0 && ch <= 0xD7FF) || (ch >= 0xE000 && ch <= 0xFFFD) || (ch >= 0x10000 && ch <= 0x10FFFF)))
		if ch == -1 || ch == '"' || ch == '\\' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || ch == 0xFEFF || !printable {
			if start < end {
				e.write(string(s[start:end]))
				start = end
			}
			if ch != -1 {
				if esc, ok := yamlEscapes[ch]; ok {
					e.write("\\" + esc)
				} else if ch <= 0xFF {
					e.write(fmt.Sprintf("\\x%02X", ch))
				} else if ch <= 0xFFFF {
					e.write(fmt.Sprintf("\\u%04X", ch))
				} else {
					e.write(fmt.Sprintf("\\U%08X", ch))
				}
				start = end + 1
			}
		}
		if end > 0 && end < len(s)-1 && (ch == ' ' || start >= end) && e.column+(end-start) > e.bestWidth && split {
			needBackslash := true
			if sp := indexRune(s, ' ', end, len(s)); sp >= 0 {
				if nl := indexRune(s, '\n', end, sp); nl >= 0 {
					sp = nl
				}
				switch {
				case sp+1 >= len(s):
				case s[sp] == '\n' && s[sp+1] != ' ':
				case !strings.ContainsAny(string(s[end:sp]), "\"'") && s[sp+1] != ' ' && s[sp+1] != '\n' && string(s[end-1:end+1]) != "  " && start != end:
					needBackslash = false
				}
			}
			data := ""
			if start < end {
				data = string(s[start:end])
			}
			if needBackslash {
				data += "\\"
			}
			if start < end {
				start = end
			}
			e.write(data)
			e.writeIndent()
			e.whitespace, e.indention = false, false
			if s[start] == ' ' {
				if !needBackslash {
					start++
				} else {
					e.write("\\")
				}
			}
		}
	}
	e.writeIndicator("\"", false, false, false)
}
