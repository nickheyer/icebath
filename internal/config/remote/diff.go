package remote

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func clone(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, sub := range n.Content {
			c.Content[i] = clone(sub)
		}
	}
	return &c
}

func newMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

func strValue(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func intValue(v int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(v, 10)}
}

func isMap(n *yaml.Node) bool { return n != nil && n.Kind == yaml.MappingNode }

func isEmptyMap(n *yaml.Node) bool { return isMap(n) && len(n.Content) == 0 }

func keyIndex(m *yaml.Node, key string) int {
	if !isMap(m) {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

func child(m *yaml.Node, key string) (*yaml.Node, bool) {
	if i := keyIndex(m, key); i >= 0 {
		return m.Content[i], true
	}
	return nil, false
}

func putChild(m *yaml.Node, key string, v *yaml.Node) {
	if i := keyIndex(m, key); i >= 0 {
		m.Content[i] = v
		return
	}
	m.Content = append(m.Content, strValue(key), v)
}

func dropChild(m *yaml.Node, key string) bool {
	if i := keyIndex(m, key); i >= 0 {
		m.Content = append(m.Content[:i-1], m.Content[i+1:]...)
		return true
	}
	return false
}

func mapKeys(m *yaml.Node) []string {
	var out []string
	if isMap(m) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			out = append(out, m.Content[i].Value)
		}
	}
	return out
}

func numberOf(n *yaml.Node) (*big.Float, bool) {
	switch n.Tag {
	case "!!int":
		i, ok := new(big.Int).SetString(n.Value, 10)
		if !ok {
			return nil, false
		}
		return new(big.Float).SetInt(i), true
	case "!!float":
		switch n.Value {
		case ".inf", "+.inf":
			return new(big.Float).SetInf(false), true
		case "-.inf":
			return new(big.Float).SetInf(true), true
		}
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil || math.IsNaN(f) {
			return nil, false
		}
		return new(big.Float).SetFloat64(f), true
	}
	return nil, false
}

func jsonEqual(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case yaml.MappingNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := 0; i+1 < len(a.Content); i += 2 {
			bv, ok := child(b, a.Content[i].Value)
			if !ok || !jsonEqual(a.Content[i+1], bv) {
				return false
			}
		}
		return true
	case yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !jsonEqual(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	}
	if an, ok := numberOf(a); ok {
		bn, ok := numberOf(b)
		return ok && an.Cmp(bn) == 0
	}
	return a.Tag == b.Tag && a.Value == b.Value
}

type change struct {
	path  []string
	value *yaml.Node
}

type changeSet struct {
	list []change
	at   map[string]int
}

func (c *changeSet) put(p []string, v *yaml.Node) {
	if c.at == nil {
		c.at = map[string]int{}
	}
	k := pathKey(p)
	if i, ok := c.at[k]; ok {
		c.list[i].value = v
		return
	}
	c.at[k] = len(c.list)
	c.list = append(c.list, change{path: p, value: v})
}

func (c *changeSet) has(p []string) bool {
	_, ok := c.at[pathKey(p)]
	return ok
}

func (c *changeSet) get(p []string) *yaml.Node {
	if i, ok := c.at[pathKey(p)]; ok {
		return c.list[i].value
	}
	return nil
}

func (c *changeSet) merge(o changeSet) {
	for _, ch := range o.list {
		c.put(ch.path, ch.value)
	}
}

func (c *changeSet) empty() bool { return len(c.list) == 0 }

func extend(prefix []string, key string) []string {
	return append(append([]string(nil), prefix...), key)
}

func diffDocs(base, next *yaml.Node, prefix []string) (changeSet, [][]string) {
	var sets changeSet
	var unsets [][]string
	for _, key := range mapKeys(next) {
		value, _ := child(next, key)
		p := extend(prefix, key)
		old, inBase := child(base, key)
		switch {
		case !inBase:
			sets.put(p, value)
		case isMap(old) && isMap(value):
			s, u := diffDocs(old, value, p)
			sets.merge(s)
			unsets = append(unsets, u...)
		case !jsonEqual(old, value):
			sets.put(p, value)
		}
	}
	for _, key := range mapKeys(base) {
		if _, ok := child(next, key); !ok {
			unsets = append(unsets, extend(prefix, key))
		}
	}
	return sets, unsets
}

func intentDiff(base, next *yaml.Node, prefix []string) (changeSet, [][]string) {
	var sets changeSet
	var unsets [][]string
	for _, key := range mapKeys(next) {
		value, _ := child(next, key)
		p := extend(prefix, key)
		old, inBase := child(base, key)
		switch {
		case isMap(value) && len(value.Content) > 0 && !isMap(old):
			s, _ := intentDiff(newMap(), value, p)
			sets.merge(s)
		case !inBase:
			sets.put(p, value)
		case isMap(old) && isMap(value):
			s, u := intentDiff(old, value, p)
			sets.merge(s)
			unsets = append(unsets, u...)
		case !jsonEqual(old, value):
			sets.put(p, value)
		}
	}
	for _, key := range mapKeys(base) {
		if _, ok := child(next, key); !ok {
			unsets = append(unsets, extend(prefix, key))
		}
	}
	return sets, unsets
}

func applyIntent(doc *yaml.Node, sets changeSet, unsets [][]string) {
	for _, ch := range sets.list {
		if isEmptyMap(ch.value) {
			if found, cur := getPath(doc, ch.path); found && isMap(cur) {
				continue
			}
		}
		setPath(doc, ch.path, clone(ch.value))
	}
	for _, p := range unsets {
		popPath(doc, p)
	}
}

func setPath(doc *yaml.Node, path []string, value *yaml.Node) {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, _ := child(node, seg)
		if !isMap(next) {
			next = newMap()
			putChild(node, seg, next)
		}
		node = next
	}
	putChild(node, path[len(path)-1], value)
}

func popPath(doc *yaml.Node, path []string) bool {
	node := doc
	for _, seg := range path[:len(path)-1] {
		next, _ := child(node, seg)
		if !isMap(node) || !isMap(next) {
			return false
		}
		node = next
	}
	return isMap(node) && dropChild(node, path[len(path)-1])
}

func getPath(doc *yaml.Node, path []string) (bool, *yaml.Node) {
	node := doc
	for _, seg := range path {
		next, ok := child(node, seg)
		if !isMap(node) || !ok {
			return false, nil
		}
		node = next
	}
	return true, node
}

func stripLocked(base, next *yaml.Node, locks []lockEntry) (*yaml.Node, *yaml.Node, [][]string) {
	base, next = clone(base), clone(next)
	var changed [][]string
	for _, l := range locks {
		inBase, bv := getPath(base, l.path)
		inNew, nv := getPath(next, l.path)
		if inBase != inNew || (inNew && !jsonEqual(bv, nv)) {
			changed = append(changed, l.path)
		}
		popPath(base, l.path)
		popPath(next, l.path)
	}
	return base, next, changed
}

func touched(path []string, value *yaml.Node, visit func(p []string, v *yaml.Node) bool) bool {
	if visit(path, value) {
		return true
	}
	if isMap(value) {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if touched(extend(path, value.Content[i].Value), value.Content[i+1], visit) {
				return true
			}
		}
	}
	return false
}

func writeCheck(sets changeSet, unsets [][]string, locks []lockEntry) (lockEntry, bool) {
	for _, p := range unsets {
		for _, l := range locks {
			if covers(l.path, p) {
				return l, true
			}
		}
	}
	var refused lockEntry
	for _, ch := range sets.list {
		hit := touched(ch.path, ch.value, func(t []string, tv *yaml.Node) bool {
			for _, l := range locks {
				if covers(l.path, t) || (len(t) < len(l.path) && covers(t, l.path) && !isMap(tv)) {
					refused = l
					return true
				}
			}
			return false
		})
		if hit {
			return refused, true
		}
	}
	return lockEntry{}, false
}

func jsonText(n *yaml.Node) string {
	var b strings.Builder
	writeJSON(&b, n)
	return b.String()
}

func writeJSON(b *strings.Builder, n *yaml.Node) {
	switch {
	case n == nil:
		b.WriteString("null")
	case n.Kind == yaml.MappingNode:
		b.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, n.Content[i].Value)
			b.WriteByte(':')
			writeJSON(b, n.Content[i+1])
		}
		b.WriteByte('}')
	case n.Kind == yaml.SequenceNode:
		b.WriteByte('[')
		for i, it := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, it)
		}
		b.WriteByte(']')
	default:
		switch n.Tag {
		case "!!null":
			b.WriteString("null")
		case "!!bool", "!!int", "!!float":
			b.WriteString(n.Value)
		default:
			writeJSONString(b, n.Value)
		}
	}
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
				b.WriteString(strconv.FormatInt(int64(r)&0xf, 16))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
