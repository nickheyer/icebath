package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type jkind uint8

const (
	jNull jkind = iota
	jBool
	jNum
	jStr
	jArr
	jObj
)

type jnode struct {
	kind jkind
	b    bool
	num  string
	str  string
	arr  []*jnode
	keys []string
	vals []*jnode
}

func parseNode(data []byte) (*jnode, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := decodeNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return n, nil
}

func decodeNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{kind: jObj}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &jnode{kind: jArr, arr: []*jnode{}}
			for dec.More() {
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.arr = append(n.arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return &jnode{kind: jStr, str: t}, nil
	case json.Number:
		return &jnode{kind: jNum, num: string(t)}, nil
	case bool:
		return &jnode{kind: jBool, b: t}, nil
	case nil:
		return &jnode{kind: jNull}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

func newObj() *jnode { return &jnode{kind: jObj} }

func newArr(items ...*jnode) *jnode {
	if items == nil {
		items = []*jnode{}
	}
	return &jnode{kind: jArr, arr: items}
}

func newStr(s string) *jnode { return &jnode{kind: jStr, str: s} }

func newBool(b bool) *jnode { return &jnode{kind: jBool, b: b} }

func newNum(s string) *jnode { return &jnode{kind: jNum, num: s} }

func emptyObjectSchema() *jnode {
	return newObj().with("type", newStr("object")).with("properties", newObj()).with("required", newArr())
}

func (n *jnode) isObj() bool { return n != nil && n.kind == jObj }

func (n *jnode) isArr() bool { return n != nil && n.kind == jArr }

func (n *jnode) isStr() bool { return n != nil && n.kind == jStr }

func (n *jnode) index(key string) int {
	if !n.isObj() {
		return -1
	}
	for i, k := range n.keys {
		if k == key {
			return i
		}
	}
	return -1
}

func (n *jnode) get(key string) *jnode {
	if i := n.index(key); i >= 0 {
		return n.vals[i]
	}
	return nil
}

func (n *jnode) has(key string) bool { return n.index(key) >= 0 }

func (n *jnode) set(key string, v *jnode) {
	if i := n.index(key); i >= 0 {
		n.vals[i] = v
		return
	}
	n.keys = append(n.keys, key)
	n.vals = append(n.vals, v)
}

func (n *jnode) with(key string, v *jnode) *jnode {
	n.set(key, v)
	return n
}

func (n *jnode) setDefault(key string, v *jnode) {
	if !n.has(key) {
		n.set(key, v)
	}
}

func (n *jnode) del(key string) bool {
	i := n.index(key)
	if i < 0 {
		return false
	}
	n.keys = append(n.keys[:i:i], n.keys[i+1:]...)
	n.vals = append(n.vals[:i:i], n.vals[i+1:]...)
	return true
}

func (n *jnode) text() (string, bool) {
	if n.isStr() {
		return n.str, true
	}
	return "", false
}

func (n *jnode) strEq(s string) bool { return n.isStr() && n.str == s }

func (n *jnode) truthy() bool {
	if n == nil {
		return false
	}
	switch n.kind {
	case jBool:
		return n.b
	case jNum:
		f, ok := new(big.Float).SetString(n.num)
		return !ok || f.Sign() != 0
	case jStr:
		return n.str != ""
	case jArr:
		return len(n.arr) > 0
	case jObj:
		return len(n.keys) > 0
	}
	return false
}

func (n *jnode) shallow() *jnode {
	c := *n
	c.arr = append([]*jnode(nil), n.arr...)
	c.keys = append([]string(nil), n.keys...)
	c.vals = append([]*jnode(nil), n.vals...)
	if n.kind == jArr && c.arr == nil {
		c.arr = []*jnode{}
	}
	return &c
}

func (n *jnode) clone() *jnode {
	if n == nil {
		return nil
	}
	c := n.shallow()
	for i, v := range c.arr {
		c.arr[i] = v.clone()
	}
	for i, v := range c.vals {
		c.vals[i] = v.clone()
	}
	return c
}

func nodesEqual(a, b *jnode) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.kind == jBool && b.kind == jNum || a.kind == jNum && b.kind == jBool {
		x, y := a, b
		if x.kind == jBool {
			x, y = y, x
		}
		v := "0"
		if y.b {
			v = "1"
		}
		return numbersEqual(x.num, v)
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case jNull:
		return true
	case jBool:
		return a.b == b.b
	case jNum:
		return numbersEqual(a.num, b.num)
	case jStr:
		return a.str == b.str
	case jArr:
		if len(a.arr) != len(b.arr) {
			return false
		}
		for i := range a.arr {
			if !nodesEqual(a.arr[i], b.arr[i]) {
				return false
			}
		}
		return true
	}
	if len(a.keys) != len(b.keys) {
		return false
	}
	for i, k := range a.keys {
		if !nodesEqual(a.vals[i], b.get(k)) {
			return false
		}
	}
	return true
}

func numbersEqual(x, y string) bool {
	a, okA := new(big.Rat).SetString(x)
	b, okB := new(big.Rat).SetString(y)
	if !okA || !okB {
		return x == y
	}
	return a.Cmp(b) == 0
}

type jsonLayout uint8

const (
	layoutCompact jsonLayout = iota
	layoutSpaced
)

func (n *jnode) appendJSON(dst []byte, l jsonLayout) []byte {
	if n == nil {
		return append(dst, "null"...)
	}
	comma, colon := ",", ":"
	if l == layoutSpaced {
		comma, colon = ", ", ": "
	}
	switch n.kind {
	case jNull:
		return append(dst, "null"...)
	case jBool:
		if n.b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case jNum:
		return append(dst, n.num...)
	case jStr:
		return append(dst, wire.Quote(n.str)...)
	case jArr:
		dst = append(dst, '[')
		for i, v := range n.arr {
			if i > 0 {
				dst = append(dst, comma...)
			}
			dst = v.appendJSON(dst, l)
		}
		return append(dst, ']')
	}
	dst = append(dst, '{')
	for i, k := range n.keys {
		if i > 0 {
			dst = append(dst, comma...)
		}
		dst = append(dst, wire.Quote(k)...)
		dst = append(dst, colon...)
		dst = n.vals[i].appendJSON(dst, l)
	}
	return append(dst, '}')
}

func (n *jnode) compact() json.RawMessage { return n.appendJSON(nil, layoutCompact) }

func (n *jnode) spaced() string { return string(n.appendJSON(nil, layoutSpaced)) }

func (n *jnode) compactLen() int { return utf8.RuneCount(n.compact()) }

func (n *jnode) value() any {
	if n == nil {
		return nil
	}
	switch n.kind {
	case jBool:
		return n.b
	case jNum:
		return json.Number(n.num)
	case jStr:
		return n.str
	case jArr:
		out := make([]any, len(n.arr))
		for i, v := range n.arr {
			out[i] = v.value()
		}
		return out
	case jObj:
		out := make(map[string]any, len(n.keys))
		for i, k := range n.keys {
			out[k] = n.vals[i].value()
		}
		return out
	}
	return nil
}

func nodeOf(v any) *jnode {
	switch t := v.(type) {
	case nil:
		return &jnode{kind: jNull}
	case *jnode:
		return t
	case bool:
		return newBool(t)
	case string:
		return newStr(t)
	case json.Number:
		return newNum(string(t))
	case float64, float32, int, int64, int32, uint, uint64, uint32:
		raw, err := json.Marshal(t)
		if err != nil {
			return &jnode{kind: jNull}
		}
		return newNum(string(raw))
	case []any:
		items := make([]*jnode, len(t))
		for i, item := range t {
			items[i] = nodeOf(item)
		}
		return newArr(items...)
	case []string:
		items := make([]*jnode, len(t))
		for i, item := range t {
			items[i] = newStr(item)
		}
		return newArr(items...)
	case Args:
		return nodeOf(map[string]any(t))
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := newObj()
		for _, k := range keys {
			n.set(k, nodeOf(t[k]))
		}
		return n
	case json.RawMessage:
		if n, err := parseNode(t); err == nil {
			return n
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return &jnode{kind: jNull}
	}
	n, err := parseNode(raw)
	if err != nil {
		return &jnode{kind: jNull}
	}
	return n
}

func parseArgs(raw string) (Args, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrArgsNotObject
	}
	return Args(m), nil
}

var ErrArgsNotObject = errors.New("tool arguments must be a JSON object")

func ParseArgs(raw string) (Args, error) {
	return parseArgs(raw)
}
