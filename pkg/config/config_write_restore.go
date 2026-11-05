package config

import (
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Emitted round trip text with every untouched source line restored byte for byte and printable astral escapes raw
func restoreSourceLines(source []byte, doc *yaml.Node, emitted string) string {
	var reparsed yaml.Node
	if yaml.Unmarshal([]byte(emitted), &reparsed) != nil || len(reparsed.Content) == 0 || len(doc.Content) == 0 {
		return emitted
	}
	lines := strings.SplitAfter(emitted, "\n")
	src := strings.SplitAfter(string(source), "\n")
	pairs, quoted := map[int]int{}, map[int]bool{}
	pairSourceLines(doc.Content[0], reparsed.Content[0], pairs, quoted)
	for at, from := range pairs {
		if at < 1 || at > len(lines) || from < 1 || from > len(src) {
			continue
		}
		orig, cur := strings.TrimSuffix(src[from-1], "\n"), strings.TrimSuffix(lines[at-1], "\n")
		if indentOf(orig) == indentOf(cur) && sameYAMLLine(orig, cur) {
			lines[at-1] = orig + "\n"
			delete(quoted, at)
		}
	}
	for at := range quoted {
		cur := strings.TrimSuffix(lines[at-1], "\n")
		if raw := unescapeAstral(cur); raw != cur && sameYAMLLine(cur, raw) {
			lines[at-1] = raw + "\n"
		}
	}
	restored := strings.Join(lines, "")
	if !sameYAMLDocument(emitted, restored) {
		return emitted
	}
	return restored
}

// Maps each emitted structural line to the source line its retained node came from and marks double quoted lines
func pairSourceLines(kept, emitted *yaml.Node, pairs map[int]int, quoted map[int]bool) {
	if kept.Kind != emitted.Kind || len(kept.Content) != len(emitted.Content) {
		return
	}
	block := func(n *yaml.Node) bool {
		return n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0
	}
	note := func(k, e, value, valueEmitted *yaml.Node) {
		for _, n := range []*yaml.Node{e, valueEmitted} {
			if n.Kind == yaml.ScalarNode && n.Style&yaml.DoubleQuotedStyle != 0 && n.Line == e.Line {
				quoted[e.Line] = true
			}
		}
		retained := value.Kind == yaml.ScalarNode || (value.Line > 0 && value.Kind == valueEmitted.Kind)
		if k.Line > 0 && k.Column == e.Column && retained && !block(value) && !block(valueEmitted) {
			pairs[e.Line] = k.Line
		}
	}
	switch kept.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(kept.Content); i += 2 {
			note(kept.Content[i], emitted.Content[i], kept.Content[i+1], emitted.Content[i+1])
			pairSourceLines(kept.Content[i+1], emitted.Content[i+1], pairs, quoted)
		}
	case yaml.SequenceNode:
		for i, item := range kept.Content {
			if item.Kind == yaml.ScalarNode {
				note(item, emitted.Content[i], item, emitted.Content[i])
				continue
			}
			pairSourceLines(item, emitted.Content[i], pairs, quoted)
		}
	}
}

func indentOf(line string) string { return line[:len(line)-len(strings.TrimLeft(line, " "))] }

// Whether two lines parse alone to the same value and carry the same comments
func sameYAMLLine(a, b string) bool {
	va, ca, ok := yamlLineValue(strings.TrimLeft(a, " "))
	if !ok {
		return false
	}
	vb, cb, ok := yamlLineValue(strings.TrimLeft(b, " "))
	return ok && ca == cb && reflect.DeepEqual(va, vb)
}

func yamlLineValue(text string) (any, string, bool) {
	var n yaml.Node
	if yaml.Unmarshal([]byte(text), &n) != nil || len(n.Content) == 0 {
		return nil, "", false
	}
	resolveYAML11(&n, false)
	var v any
	if n.Content[0].Decode(&v) != nil {
		return nil, "", false
	}
	var comments strings.Builder
	var walk func(*yaml.Node)
	walk = func(x *yaml.Node) {
		comments.WriteString(x.HeadComment + "\x00" + x.LineComment + "\x00" + x.FootComment + "\x00")
		for _, c := range x.Content {
			walk(c)
		}
	}
	walk(&n)
	return v, comments.String(), true
}

// Whether two documents decode to the same YAML 1.1 value
func sameYAMLDocument(a, b string) bool {
	va, _, okA := yamlLineValue(a)
	vb, _, okB := yamlLineValue(b)
	return okA && okB && reflect.DeepEqual(va, vb)
}

// Line with every escaped astral code point written raw the way an allow_unicode emitter prints it
func unescapeAstral(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] != '\\' || i+1 >= len(line) {
			out.WriteByte(line[i])
			continue
		}
		if line[i+1] == 'U' && i+10 <= len(line) {
			if r, err := strconv.ParseUint(line[i+2:i+10], 16, 32); err == nil && r >= 0x10000 && r < 0x10FFFF {
				out.WriteRune(rune(r))
				i += 9
				continue
			}
		}
		out.WriteString(line[i : i+2])
		i++
	}
	return out.String()
}
