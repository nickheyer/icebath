package agentimport

import (
	"sort"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	tomlutil "github.com/nickheyer/icebath/pkg/utils/toml"
	"github.com/pelletier/go-toml/v2/unstable"
)

// Parses TOML like tomllib, every table an ordered object keyed in document order
func loadsTOMLOrdered(text string) (any, error) {
	doc, err := tomlutil.Decode(text)
	if err != nil {
		return nil, err
	}
	order := tomlKeyOrder(text)
	return orderTOML(doc, "", order), nil
}

// First appearance index of every dotted key path of a document, array indexes left out
func tomlKeyOrder(text string) map[string]int {
	order := map[string]int{}
	note := func(path []string) {
		for i := 1; i <= len(path); i++ {
			k := strings.Join(path[:i], "\x00")
			if _, ok := order[k]; !ok {
				order[k] = len(order)
			}
		}
	}
	keyOf := func(n *unstable.Node) []string {
		var parts []string
		it := n.Key()
		for it.Next() {
			parts = append(parts, string(it.Node().Data))
		}
		return parts
	}
	var walkValue func(prefix []string, v *unstable.Node)
	walkValue = func(prefix []string, v *unstable.Node) {
		if v == nil || v.Kind != unstable.InlineTable {
			return
		}
		it := v.Children()
		for it.Next() {
			kv := it.Node()
			if kv.Kind != unstable.KeyValue {
				continue
			}
			path := append(append([]string(nil), prefix...), keyOf(kv)...)
			note(path)
			walkValue(path, kv.Value())
		}
	}
	var p unstable.Parser
	p.Reset([]byte(text))
	var current []string
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			current = keyOf(e)
			note(current)
		case unstable.KeyValue:
			path := append(append([]string(nil), current...), keyOf(e)...)
			note(path)
			walkValue(path, e.Value())
		}
	}
	return order
}

// Converts decoded TOML maps to ordered objects using the recorded key order
func orderTOML(v any, path string, order map[string]int) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		pos := func(k string) (int, bool) {
			full := k
			if path != "" {
				full = path + "\x00" + k
			}
			i, ok := order[full]
			return i, ok
		}
		sort.SliceStable(keys, func(i, j int) bool {
			pi, oi := pos(keys[i])
			pj, oj := pos(keys[j])
			if oi != oj {
				return oi
			}
			if oi {
				return pi < pj
			}
			return keys[i] < keys[j]
		})
		out := jsonutil.NewObject()
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "\x00" + k
			}
			out.Set(k, orderTOML(t[k], child, order))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderTOML(item, path, order)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderTOML(item, path, order)
		}
		return out
	}
	return v
}
