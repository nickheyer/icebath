package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"gopkg.in/yaml.v3"
)

// Objects already built per node, so an alias yields the very object its anchor built
type pyConstructor struct {
	v12  bool
	src  []rune
	done map[*yaml.Node]any
	err  error
}

// Document value as the Python safe constructor builds it, aliases shared, keys typed and timestamps as date or datetime
func pythonizeDocument(node *yaml.Node, value any, v12 bool, src []byte) (any, error) {
	for node != nil && node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return value, nil
		}
		node = node.Content[0]
	}
	if node == nil {
		return value, nil
	}
	p := &pyConstructor{v12: v12, src: []rune(string(src)), done: map[*yaml.Node]any{}}
	out := p.walk(node, value)
	if p.err != nil {
		return nil, p.err
	}
	return out, nil
}

// Python object of one node given the value the decoder built for it
func (p *pyConstructor) walk(n *yaml.Node, v any) any {
	if n.Kind == yaml.AliasNode {
		if got, ok := p.done[n.Alias]; ok {
			return got
		}
		return p.walk(n.Alias, v)
	}
	if got, ok := p.done[n]; ok {
		return got
	}
	switch n.Kind {
	case yaml.ScalarNode:
		_, isTime := v.(time.Time)
		_, isStr := v.(string)
		switch {
		case n.Tag == "!!binary" && isStr:
			v = []byte(v.(string))
		case isTime || (isStr && n.Style == 0 && yaml11Timestamp.MatchString(n.Value)):
			ts, ok, err := pydatetime.YAMLTimestamp(n.Value)
			switch {
			case ok && err == nil:
				v = ts
				if n.Anchor != "" {
					v = timestampPointer(ts)
				}
			case !ok && n.Style == 0:
				v = n.Value
			case !ok && p.err == nil:
				p.err = &YAMLError{fmt.Errorf("failed to construct timestamp from %q\n%s", n.Value, p.mark(n.Line, n.Column))}
			}
		}
		p.done[n] = v
		return v
	case yaml.SequenceNode:
		list, ok := v.([]any)
		if !ok {
			return v
		}
		if n.Anchor != "" && cap(list) == 0 {
			list = make([]any, 0, 1)
		}
		p.done[n] = list
		for i := range list {
			if i < len(n.Content) {
				list[i] = p.walk(n.Content[i], list[i])
			}
		}
		return list
	case yaml.MappingNode:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		p.done[n] = m
		p.mapping(n, m)
		return m
	}
	return v
}

// Shared pointer for an anchored timestamp, the one date or datetime object every alias of it refers to
func timestampPointer(ts any) any {
	switch t := ts.(type) {
	case pydatetime.Date:
		return &t
	case pydatetime.DateTime:
		return &t
	}
	return ts
}

// Rebuilds a mapping in the flattened merge order with Python keys and the winning value of each key
func (p *pyConstructor) mapping(n *yaml.Node, m map[string]any) {
	pairs, err := flattenMergePairs(n, p.src)
	if err == nil && !hasMergeKey(n) {
		err = p.duplicateKey(n, pairs)
	}
	if err != nil {
		if p.err == nil {
			p.err = err
		}
		return
	}
	var order []string
	winners := map[string]*yaml.Node{}
	goKeys := map[string]string{}
	pyKeys := map[string]any{}
	for _, kv := range pairs {
		goKey := decodedKeyString(kv[0])
		key := goKey
		if pk, isStr := p.pyKey(kv[0]); isStr {
			key = pk.(string)
		} else {
			key = PyKeyString(pk)
			pyKeys[key] = pk
		}
		if _, seen := winners[key]; !seen {
			order = append(order, key)
		}
		winners[key], goKeys[key] = kv[1], goKey
	}
	built := make(map[string]any, len(order))
	for _, key := range order {
		node := winners[key]
		val, ok := m[goKeys[key]]
		if !ok {
			val = decodeNodeValue(node)
		}
		built[key] = p.walk(node, val)
	}
	for k := range m {
		delete(m, k)
	}
	for k, v := range built {
		m[k] = v
	}
	SetMappingKeys(m, order)
	for k, pk := range pyKeys {
		SetMappingPyKey(m, k, pk)
	}
}

// Key and value pairs of a mapping after merge keys, merged pairs first with later merge list entries ahead
func flattenMergePairs(n *yaml.Node, src []rune) ([][2]*yaml.Node, error) {
	var merge, own [][2]*yaml.Node
	merged := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			if merged && len(merge) > 0 {
				return nil, &YAMLError{errors.New(markedYAMLError(src, "while constructing a mapping", n, `found duplicate merge key "<<"`, k,
					"Duplicate merge keys are never allowed, not even when\n`.allow_duplicate_keys` is set to True\n"))}
			}
			merged = true
			target := derefAlias(val)
			switch target.Kind {
			case yaml.MappingNode:
				sub, err := flattenMergePairs(target, src)
				if err != nil {
					return nil, err
				}
				merge = append(merge, sub...)
			case yaml.SequenceNode:
				subs := make([][][2]*yaml.Node, 0, len(target.Content))
				for _, s := range target.Content {
					sub, err := flattenMergePairs(derefAlias(s), src)
					if err != nil {
						return nil, err
					}
					subs = append(subs, sub)
				}
				for j := len(subs) - 1; j >= 0; j-- {
					merge = append(merge, subs[j]...)
				}
			}
			continue
		}
		own = append(own, [2]*yaml.Node{k, val})
	}
	return append(merge, own...), nil
}

// Whether a mapping holds a merge key, which turns the duplicate key check off as the safe constructor does
func hasMergeKey(n *yaml.Node) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			return true
		}
	}
	return false
}

// DuplicateKeyError for the first key equal under Python equality to an earlier key, True equal to 1
func (p *pyConstructor) duplicateKey(n *yaml.Node, pairs [][2]*yaml.Node) error {
	type first struct {
		node *yaml.Node
		val  any
	}
	seen := map[string]first{}
	for _, kv := range pairs {
		pk, _ := p.pyKey(kv[0])
		id, ok := pyKeyEqualityID(pk)
		if !ok {
			continue
		}
		if prev, dup := seen[id]; dup {
			problem := fmt.Sprintf("found duplicate key \"%s\" with value \"%s\" (original value: \"%s\")", pyrepr.Str(pk), pyrepr.Str(decodeNodeValue(kv[1])), pyrepr.Str(prev.val))
			return &YAMLError{errors.New(markedYAMLError(p.src, "while constructing a mapping", n, problem, kv[0],
				"\nTo suppress this check see:\n    http://yaml.readthedocs.io/en/latest/api.html#duplicate-keys\n",
				"Duplicate keys will become an error in future releases, and are errors\nby default when using the new API.\n"))}
		}
		seen[id] = first{kv[0], decodeNodeValue(kv[1])}
	}
	return nil
}

// Hashable equality class of a Python key, false for NaN which equals no other key
func pyKeyEqualityID(k any) (string, bool) {
	switch family := pyKeyFamily(k); family {
	case "str":
		return "s" + k.(string), true
	case "none":
		return "none", true
	case "bytes":
		return "b" + string(k.([]byte)), true
	case "num":
		if f, isFloat := pyFloatOf(k); isFloat {
			if math.IsNaN(f) {
				return "", false
			}
			if !math.IsInf(f, 0) && f == math.Trunc(f) {
				n, _ := new(big.Float).SetFloat64(f).Int(nil)
				return "n" + n.String(), true
			}
			return "f" + strconv.FormatFloat(f, 'g', -1, 64), true
		}
		n, _ := keyBigInt(k)
		return "n" + n.String(), true
	case "date", "naive", "aware":
		return family + pyTimeKey(k).Format(time.RFC3339Nano), true
	default:
		return family + pyrepr.Repr(k), true
	}
}

// Mark text of a ruamel StringMark, the position line plus the snippet with its caret line
func (p *pyConstructor) mark(line, col int) string { return yamlMark(p.src, line, col) }

func yamlMark(src []rune, line, col int) string {
	pointer, l := 0, 1
	for pointer < len(src) && l < line {
		if r := src[pointer]; r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 || (r == '\r' && (pointer+1 >= len(src) || src[pointer+1] != '\n')) {
			l++
		}
		pointer++
	}
	pointer += col - 1
	if pointer > len(src) {
		pointer = len(src)
	}
	isBreak := func(r rune) bool { return r == 0 || r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 }
	head, start := "", pointer
	for start > 0 && !isBreak(src[start-1]) {
		start--
		if float64(pointer-start) > 75.0/2-1 {
			head, start = " ... ", start+5
			break
		}
	}
	tail, end := "", pointer
	for end < len(src) && !isBreak(src[end]) {
		end++
		if float64(end-pointer) > 75.0/2-1 {
			tail, end = " ... ", end-5
			break
		}
	}
	return fmt.Sprintf("  in \"<unicode string>\", line %d, column %d:\n    %s%s%s\n%s^ (line: %d)", line, col, head, string(src[start:end]), tail,
		strings.Repeat(" ", 4+pointer-start+len(head)), line)
}

// Text of a ruamel MarkedYAMLError, the context mark left out when it equals the problem mark
func markedYAMLError(src []rune, context string, contextNode *yaml.Node, problem string, problemNode *yaml.Node, notes ...string) string {
	lines := []string{context}
	if contextNode.Line != problemNode.Line || contextNode.Column != problemNode.Column {
		lines = append(lines, yamlMark(src, contextNode.Line, contextNode.Column))
	}
	lines = append(lines, problem, yamlMark(src, problemNode.Line, problemNode.Column))
	for _, note := range notes {
		lines = append(lines, note)
	}
	return strings.Join(lines, "\n")
}

func derefAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// Map key the decoder stored a key node under, the text a stringified decoded key takes
func decodedKeyString(k *yaml.Node) string {
	if k.Kind == yaml.ScalarNode && k.Tag == "!!str" {
		return k.Value
	}
	var x any
	if err := k.Decode(&x); err != nil {
		return k.Value
	}
	return fmt.Sprint(x)
}

// Value of a lone node decoded the way the whole document was
func decodeNodeValue(n *yaml.Node) any {
	var x any
	if err := n.Decode(&x); err != nil {
		return nil
	}
	x = restoreYAMLBigInts(normalizeMaps(x))
	recordNodeOrder(n, x)
	return x
}

// Python key a key node constructs and whether it is a str
func (p *pyConstructor) pyKey(k *yaml.Node) (any, bool) {
	k = derefAlias(k)
	if k.Kind != yaml.ScalarNode {
		return decodedKeyString(k), true
	}
	if k.Style != 0 && k.Style&yaml.TaggedStyle == 0 {
		return k.Value, true
	}
	if k.Style == 0 && yaml11Timestamp.MatchString(k.Value) {
		if ts, ok, err := pydatetime.YAMLTimestamp(k.Value); ok && err == nil {
			return ts, false
		}
	}
	probe := &yaml.Node{Kind: yaml.ScalarNode, Tag: k.Tag, Value: k.Value, Style: k.Style}
	resolveYAMLVersion(probe, false, p.v12)
	switch probe.Tag {
	case "!!bool":
		return probe.Value == "true", false
	case "!!int":
		if n, ok := new(big.Int).SetString(probe.Value, 10); ok {
			if n.IsInt64() {
				return int(n.Int64()), false
			}
			return n, false
		}
	case "!!float":
		var f float64
		if probe.Decode(&f) == nil {
			return f, false
		}
	case "!!null":
		return nil, false
	case "!!timestamp":
		if ts, ok, err := pydatetime.YAMLTimestamp(k.Value); ok && err == nil {
			return ts, false
		}
	case "!!binary":
		if b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(k.Value), "")); err == nil {
			return b, false
		}
	}
	return k.Value, true
}

// Comparison family of a key, keys of two families raising TypeError in a Python sort
func pyKeyFamily(k any) string {
	switch t := k.(type) {
	case string:
		return "str"
	case bool, float32, float64:
		return "num"
	case []byte:
		return "bytes"
	case pydatetime.Date, *pydatetime.Date:
		return "date"
	case pydatetime.DateTime:
		if t.Zone == nil {
			return "naive"
		}
		return "aware"
	case *pydatetime.DateTime:
		return pyKeyFamily(*t)
	case time.Time:
		switch {
		case t.Location() != time.UTC:
			return "aware"
		case t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0:
			return "date"
		}
		return "naive"
	case nil:
		return "none"
	}
	if _, ok := keyBigInt(k); ok {
		return "num"
	}
	return fmt.Sprintf("%T", k)
}

func keyBigInt(k any) (*big.Int, bool) {
	switch t := k.(type) {
	case int:
		return big.NewInt(int64(t)), true
	case int64:
		return big.NewInt(t), true
	case int32:
		return big.NewInt(int64(t)), true
	case int16:
		return big.NewInt(int64(t)), true
	case int8:
		return big.NewInt(int64(t)), true
	case uint:
		return new(big.Int).SetUint64(uint64(t)), true
	case uint64:
		return new(big.Int).SetUint64(t), true
	case uint32:
		return big.NewInt(int64(t)), true
	case uint16:
		return big.NewInt(int64(t)), true
	case uint8:
		return big.NewInt(int64(t)), true
	case *big.Int:
		return t, true
	case bool:
		if t {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return nil, false
}

// Python less than between two keys of one family, NaN comparing false both ways
func pyKeyLess(a, b any) bool {
	switch x := a.(type) {
	case string:
		return x < b.(string)
	case []byte:
		return bytes.Compare(x, b.([]byte)) < 0
	}
	if fa, ok := pyFloatOf(a); ok {
		return pyNumLess(fa, b, false)
	}
	if fb, ok := pyFloatOf(b); ok {
		return pyNumLess(fb, a, true)
	}
	if ia, ok := keyBigInt(a); ok {
		ib, _ := keyBigInt(b)
		return ia.Cmp(ib) < 0
	}
	ta, tb := pyTimeKey(a), pyTimeKey(b)
	return ta.Before(tb)
}

func pyFloatOf(k any) (float64, bool) {
	switch t := k.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	}
	return 0, false
}

// Float f against a number other, f on the left unless swapped, exact for big ints
func pyNumLess(f float64, other any, swapped bool) bool {
	if math.IsNaN(f) {
		return false
	}
	var cmp int
	if g, ok := pyFloatOf(other); ok {
		if math.IsNaN(g) {
			return false
		}
		switch {
		case f < g:
			cmp = -1
		case f > g:
			cmp = 1
		}
	} else {
		n, _ := keyBigInt(other)
		switch {
		case math.IsInf(f, 1):
			cmp = 1
		case math.IsInf(f, -1):
			cmp = -1
		default:
			cmp = new(big.Float).SetFloat64(f).Cmp(new(big.Float).SetInt(n))
		}
	}
	if swapped {
		return cmp > 0
	}
	return cmp < 0
}

// Instant ordering of a date or datetime key, the wall clock for naive values and the UTC instant for aware ones
func pyTimeKey(k any) time.Time {
	switch t := k.(type) {
	case pydatetime.Date:
		return time.Date(t.Year, time.Month(t.Month), t.Day, 0, 0, 0, 0, time.UTC)
	case *pydatetime.Date:
		return pyTimeKey(*t)
	case pydatetime.DateTime:
		wall := time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second, t.Microsecond*1000, time.UTC)
		if t.Zone != nil {
			wall = wall.Add(-t.Zone.Offset)
		}
		return wall
	case *pydatetime.DateTime:
		return pyTimeKey(*t)
	case time.Time:
		return t
	}
	return time.Time{}
}

// Order a Python sorted call gives keys, the input order when two keys cannot be compared
func pySortedOrder(keys []any) []int {
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	if len(keys) < 2 {
		return idx
	}
	family := pyKeyFamily(keys[0])
	for _, k := range keys[1:] {
		if f := pyKeyFamily(k); f != family || f == "none" {
			return idx
		}
	}
	if family == "none" {
		return idx
	}
	for i := 1; i < len(idx); i++ {
		pivot := idx[i]
		lo, hi := 0, i
		for lo < hi {
			mid := (lo + hi) / 2
			if pyKeyLess(keys[pivot], keys[idx[mid]]) {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		copy(idx[lo+1:i+1], idx[lo:i])
		idx[lo] = pivot
	}
	return idx
}
