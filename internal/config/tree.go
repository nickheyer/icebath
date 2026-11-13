package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Reports a mapping value tree
func isMapping(n *yaml.Node) bool { return n != nil && n.Kind == yaml.MappingNode }

// Reports a sequence value tree
func isSequence(n *yaml.Node) bool { return n != nil && n.Kind == yaml.SequenceNode }

// Value under key of a mapping value tree, nil when absent
func childOf(m *yaml.Node, key string) *yaml.Node {
	if i := mappingIndex(m, key); i >= 0 {
		return m.Content[i]
	}
	return nil
}

// Value under a key path of nested mappings, nil on any miss
func treeGet(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		if !isMapping(n) {
			return nil
		}
		if n = childOf(n, k); n == nil {
			return nil
		}
	}
	return n
}

// Copy of base with override merged over it, mappings merging and a null over a mapping leaving the mapping
func deepMerge(base, override *yaml.Node) *yaml.Node {
	out := plainCopy(base)
	if !isMapping(out) {
		out = emptyMapping()
	}
	if !isMapping(override) {
		return out
	}
	for i := 0; i+1 < len(override.Content); i += 2 {
		k, v := override.Content[i].Value, override.Content[i+1]
		cur := childOf(out, k)
		switch {
		case isMapping(cur) && isMapping(v):
			putKey(out, k, deepMerge(cur, v))
		case isMapping(cur) && isNull(v):
		default:
			putKey(out, k, plainCopy(v))
		}
	}
	return out
}

// Override with every section of raw it omits kept and the mappings both name merged, raw underneath
func mergePartialSave(raw, override *yaml.Node) *yaml.Node {
	out := plainCopy(override)
	if !isMapping(out) {
		out = emptyMapping()
	}
	if !isMapping(raw) {
		return out
	}
	for i := 0; i+1 < len(raw.Content); i += 2 {
		k, v := raw.Content[i].Value, raw.Content[i+1]
		cur := childOf(out, k)
		switch {
		case cur == nil && mappingIndex(out, k) < 0:
			putKey(out, k, plainCopy(v))
		case isMapping(cur) && isMapping(v):
			putKey(out, k, deepMerge(v, cur))
		}
	}
	return out
}

// Joined form of a key path used as a set member
func pathKey(parts []string) string { return strings.Join(parts, "\x00") }

// Leaf paths a raw config writes, non-empty mappings walked and every other value, an empty mapping included, a leaf
func explicitPaths(raw *yaml.Node) map[string]bool {
	out := map[string]bool{}
	var walk func(n *yaml.Node, path []string)
	walk = func(n *yaml.Node, path []string) {
		if isMapping(n) && (len(n.Content) > 0 || len(path) == 0) {
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1], append(append([]string(nil), path...), n.Content[i].Value))
			}
			return
		}
		if len(path) > 0 {
			out[pathKey(path)] = true
		}
	}
	walk(raw, nil)
	return out
}

// Config without values equal to the defaults, preserved paths always kept and mappings left empty dropped
func stripDefaults(config, defaults *yaml.Node, preserve map[string]bool) *yaml.Node {
	keep := map[string]bool{pathKey([]string{"_config_version"}): true}
	for k := range preserve {
		keep[k] = true
	}
	var strip func(v, def *yaml.Node, path []string) (*yaml.Node, bool)
	strip = func(v, def *yaml.Node, path []string) (*yaml.Node, bool) {
		if len(path) > 0 && keep[pathKey(path)] {
			return plainCopy(v), true
		}
		if isMapping(v) && len(v.Content) > 0 {
			out := emptyMapping()
			for i := 0; i+1 < len(v.Content); i += 2 {
				k := v.Content[i].Value
				var d *yaml.Node
				if isMapping(def) {
					d = childOf(def, k)
				}
				if c, ok := strip(v.Content[i+1], d, append(append([]string(nil), path...), k)); ok {
					out.Content = append(out.Content, plainCopy(v.Content[i]), c)
				}
			}
			return out, len(out.Content) > 0
		}
		if valueEqual(v, def, false) {
			return nil, false
		}
		return plainCopy(v), true
	}
	if out, ok := strip(config, defaults, nil); ok {
		return out
	}
	return emptyMapping()
}

// Mapping paths proposed would delete by leaving them out of existing, a non-empty mapping replaced by another value included
func omittedPaths(existing, proposed *yaml.Node, prefix []string) []string {
	var out []string
	if !isMapping(existing) {
		return nil
	}
	for i := 0; i+1 < len(existing.Content); i += 2 {
		k, old := existing.Content[i].Value, existing.Content[i+1]
		path := append(append([]string(nil), prefix...), k)
		at := mappingIndex(proposed, k)
		if at < 0 {
			out = append(out, strings.Join(path, "."))
			continue
		}
		next := proposed.Content[at]
		if isMapping(old) {
			if isMapping(next) {
				out = append(out, omittedPaths(old, next, path)...)
			} else if len(old.Content) > 0 {
				out = append(out, strings.Join(path, "."))
			}
		}
	}
	return out
}

// Removes the dotted leaves present in a copy of cfg, splitting at every dot, and returns the leaves it removed
func stripDottedKeys(cfg *yaml.Node, dotted []string) (*yaml.Node, []string) {
	out := plainCopy(cfg)
	var stripped []string
	for _, d := range dotted {
		parts := strings.Split(d, ".")
		node := out
		if len(parts) > 1 {
			node = treeGet(out, parts[:len(parts)-1]...)
		}
		leaf := parts[len(parts)-1]
		if at := mappingIndex(node, leaf); at >= 0 {
			removeSlot(pathHop{node, at})
			stripped = append(stripped, d)
		}
	}
	return out, stripped
}

// Dotted leaf keys of a mapping, a non-empty mapping walked and any other value a leaf
func flattenKeys(n *yaml.Node, prefix string, out map[string]bool) {
	if !isMapping(n) {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		dotted := n.Content[i].Value
		if prefix != "" {
			dotted = prefix + "." + dotted
		}
		if v := n.Content[i+1]; isMapping(v) && len(v.Content) > 0 {
			flattenKeys(v, dotted, out)
			continue
		}
		out[dotted] = true
	}
}

// Reports an item equal to one of items
func containsValue(items []*yaml.Node, v *yaml.Node) bool {
	for _, it := range items {
		if valueEqual(it, v, false) {
			return true
		}
	}
	return false
}

// Live with the edits desired makes to base, a branch live dropped or retyped since then kept as is
func threeWay(live, desired, base, frozen *yaml.Node) *yaml.Node {
	if valueEqual(desired, base, false) {
		return plainCopy(live)
	}
	switch {
	case isMapping(desired) && isMapping(base) && isMapping(live):
		frozenMap := frozen
		if !isMapping(frozenMap) {
			frozenMap = emptyMapping()
		}
		merged := plainCopy(live)
		for i := 0; i+1 < len(desired.Content); i += 2 {
			k, v := desired.Content[i].Value, desired.Content[i+1]
			b := childOf(base, k)
			if mappingIndex(base, k) < 0 {
				putKey(merged, k, plainCopy(v))
				continue
			}
			if valueEqual(v, b, false) {
				continue
			}
			cur := childOf(live, k)
			if (isMapping(b) && !isMapping(cur)) || (isSequence(b) && !isSequence(cur)) {
				if mappingIndex(live, k) >= 0 || mappingIndex(frozenMap, k) >= 0 {
					continue
				}
				cur = plainCopy(b)
			}
			f := childOf(frozenMap, k)
			if f == nil {
				f = emptyMapping()
			}
			putKey(merged, k, threeWay(cur, v, b, f))
		}
		return merged
	case isSequence(desired) && isSequence(base) && isSequence(live):
		var removed []*yaml.Node
		for _, it := range base.Content {
			if !containsValue(desired.Content, it) {
				removed = append(removed, it)
			}
		}
		out := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
		for _, it := range live.Content {
			if !containsValue(removed, it) {
				out.Content = append(out.Content, plainCopy(it))
			}
		}
		for _, it := range desired.Content {
			if !containsValue(base.Content, it) && !containsValue(out.Content, it) {
				out.Content = append(out.Content, plainCopy(it))
			}
		}
		return out
	}
	return plainCopy(desired)
}

// Snapshot without the branches still equal to the schema defaults
func schemaDefaultBranch(snapshot, defaults *yaml.Node) *yaml.Node {
	if !isMapping(snapshot) || !isMapping(defaults) {
		return plainCopy(snapshot)
	}
	kept := emptyMapping()
	for i := 0; i+1 < len(snapshot.Content); i += 2 {
		k, v := snapshot.Content[i].Value, snapshot.Content[i+1]
		if mappingIndex(defaults, k) < 0 {
			putKey(kept, k, plainCopy(v))
			continue
		}
		def := childOf(defaults, k)
		switch {
		case valueEqual(v, def, false):
		case isMapping(v) && isMapping(def):
			if nested := schemaDefaultBranch(v, def); len(nested.Content) > 0 {
				putKey(kept, k, nested)
			}
		default:
			putKey(kept, k, plainCopy(v))
		}
	}
	return kept
}

// Typed changes between two encodings of a config applied onto the user tree they were decoded from
func applyDelta(base, before, after *yaml.Node) *yaml.Node {
	if valueEqual(before, after, true) {
		return base
	}
	if !isMapping(before) || !isMapping(after) {
		return plainCopy(after)
	}
	out := plainCopy(base)
	if !isMapping(out) {
		out = emptyMapping()
	}
	for i := 0; i+1 < len(after.Content); i += 2 {
		k, a := after.Content[i].Value, after.Content[i+1]
		if mappingIndex(before, k) < 0 {
			putKey(out, k, plainCopy(a))
			continue
		}
		b := childOf(before, k)
		if valueEqual(b, a, true) {
			continue
		}
		putKey(out, k, applyDelta(childOf(base, k), b, a))
	}
	for i := 0; i+1 < len(before.Content); i += 2 {
		k := before.Content[i].Value
		if mappingIndex(after, k) < 0 {
			if at := mappingIndex(out, k); at >= 0 {
				removeSlot(pathHop{out, at})
			}
		}
	}
	return out
}
