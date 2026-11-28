package config

import (
	"bytes"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A config.yaml as YAML nodes, changed in place so that what a person wrote around a change stays: the comments, the order of the names, the
// spelling of the names a person gave and the indentation

// Value at the top of the document, nil when it holds nothing
func content(doc *yaml.Node) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	if node := doc.Content[0]; node.Kind != yaml.ScalarNode || node.ShortTag() != "!!null" {
		return node
	}
	return nil
}

// Names of every mapping of the document in the order it writes them, by the names on the way to it joined with a zero byte. A mapping an
// alias stands for is not walked again where the alias is
func orderOf(doc *yaml.Node) map[string][]string {
	out := map[string][]string{}
	var walk func(node *yaml.Node, at []string)
	walk = func(node *yaml.Node, at []string) {
		if node.Kind != yaml.MappingNode {
			return
		}
		joined := strings.Join(at, "\x00")
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i]; !merging(key) {
				out[joined] = append(out[joined], key.Value)
				walk(node.Content[i+1], append(slices.Clip(at), key.Value))
			}
		}
	}
	if node := content(doc); node != nil {
		walk(node, nil)
	}
	return out
}

// Mapping at the top of the document, made when the document holds none, taking the comments of the null it held in its place
func top(doc *yaml.Node) *yaml.Node {
	if doc.Kind != yaml.DocumentNode {
		*doc = yaml.Node{Kind: yaml.DocumentNode}
	}
	if node := content(doc); node != nil {
		return node
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if len(doc.Content) > 0 {
		held := doc.Content[0]
		mapping.HeadComment, mapping.LineComment, mapping.FootComment = held.HeadComment, held.LineComment, held.FootComment
	}
	doc.Content = []*yaml.Node{mapping}
	return mapping
}

// Whether the name is the << that takes the names of other mappings into this one
func merging(name *yaml.Node) bool {
	return name.Kind == yaml.ScalarNode && name.ShortTag() == "!!merge"
}

// Place in the mapping of the name the part answers to, -1 when none does
func keyAt(mapping *yaml.Node, part string, how match) int {
	var names []string
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if !merging(mapping.Content[i]) {
			names = append(names, mapping.Content[i].Value)
		}
	}
	name, ok := pick(names, part, how)
	if !ok {
		return -1
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if key := mapping.Content[i]; !merging(key) && key.Value == name {
			return i
		}
	}
	return -1
}

// Copy of a node and everything in it, without the anchors that name them, an alias in it still standing for what it stands for
func cloneNode(node *yaml.Node) *yaml.Node {
	out := *node
	out.Anchor = ""
	if node.Kind == yaml.AliasNode {
		return &out
	}
	out.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		out.Content[i] = cloneNode(child)
	}
	return &out
}

// Value at i in the mapping that a change can be made in: an alias is replaced by a copy of what it stands for, so that the change stays here
func own(mapping *yaml.Node, i int) *yaml.Node {
	if value := mapping.Content[i]; value.Kind == yaml.AliasNode {
		mapping.Content[i] = cloneNode(value.Alias)
	}
	return mapping.Content[i]
}

// Mappings a << takes names from: one mapping, or a list of them, each perhaps an alias
func sources(value *yaml.Node) []*yaml.Node {
	if value.Kind == yaml.AliasNode {
		value = value.Alias
	}
	if value.Kind != yaml.SequenceNode {
		return []*yaml.Node{value}
	}
	var out []*yaml.Node
	for _, item := range value.Content {
		out = append(out, sources(item)...)
	}
	return out
}

// Writes the names the mapping takes from others through << into it, those of its own and of the first source winning as they do when the
// file is read, so that a change of one of them stays in this mapping
func unmerged(mapping *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); {
		if !merging(mapping.Content[i]) {
			i += 2
			continue
		}
		taken := &yaml.Node{Kind: yaml.MappingNode}
		for _, source := range sources(mapping.Content[i+1]) {
			if source.Kind != yaml.MappingNode {
				continue
			}
			source = cloneNode(source)
			unmerged(source)
			for j := 0; j+1 < len(source.Content); j += 2 {
				if name := source.Content[j].Value; keyAt(taken, name, exact) < 0 && keyAt(mapping, name, exact) < 0 {
					taken.Content = append(taken.Content, source.Content[j], source.Content[j+1])
				}
			}
		}
		mapping.Content = slices.Concat(mapping.Content[:i], taken.Content, mapping.Content[i+2:])
		i += len(taken.Content)
	}
}

// Makes the document one that reads again after a change took away what an alias stood for: an alias whose anchor the document no longer
// holds becomes a copy of what it stood for, and a << loses the sources that are mappings no more, and goes with the last of them
func settle(doc *yaml.Node) {
	unalias(doc, true)
}

// Deals with every alias whose anchor the document no longer holds: with copies it becomes a copy of what it stood for, without it goes
// too, with the name it was the value of, as an alias of something taken out on purpose must. A << loses the sources that are gone or are
// mappings no more, and goes with the last of them
func unalias(doc *yaml.Node, copies bool) {
	anchored := map[*yaml.Node]bool{}
	var mark func(node *yaml.Node)
	mark = func(node *yaml.Node) {
		if node.Anchor != "" {
			anchored[node] = true
		}
		for _, child := range node.Content {
			mark(child)
		}
	}
	mark(doc)
	gone := func(node *yaml.Node) bool { return node.Kind == yaml.AliasNode && !anchored[node.Alias] }
	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		switch {
		case copies:
			for i, child := range node.Content {
				if gone(child) {
					node.Content[i] = cloneNode(child.Alias)
				}
			}
		case node.Kind == yaml.MappingNode:
			for i := 0; i+1 < len(node.Content); {
				if gone(node.Content[i]) || gone(node.Content[i+1]) {
					node.Content = slices.Delete(node.Content, i, i+2)
					continue
				}
				i += 2
			}
		default:
			node.Content = slices.DeleteFunc(node.Content, gone)
		}
		for _, child := range node.Content {
			walk(child)
		}
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); {
			if merging(node.Content[i]) && !merges(node.Content[i+1]) {
				node.Content = slices.Delete(node.Content, i, i+2)
				continue
			}
			i += 2
		}
	}
	walk(doc)
}

// Text of a config file once things are taken out of the mapping of its settings, everything else as the person wrote it: the comments, the
// order and spelling of the names and the indentation. An alias of something taken out goes with it, so nothing taken out comes back
// under another name. The text as it is when it holds no mapping of settings or does not parse
func Without(data []byte, take func(settings *yaml.Node)) ([]byte, error) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return data, nil
	}
	settings := content(&doc)
	if settings == nil || settings.Kind != yaml.MappingNode {
		return data, nil
	}
	take(settings)
	unalias(&doc, false)
	return render(&doc, data, false)
}

// Whether a << still takes names from a mapping, the sources of a list of them that are mappings no more dropped from it
func merges(value *yaml.Node) bool {
	mapping := func(node *yaml.Node) bool {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		return node.Kind == yaml.MappingNode
	}
	if value.Kind != yaml.SequenceNode {
		return mapping(value)
	}
	value.Content = slices.DeleteFunc(value.Content, func(item *yaml.Node) bool { return !mapping(item) })
	return len(value.Content) > 0
}

// Writes every << the text wrote plainly as plainly again, since the encoder spells out the tag that reading gave it
func plainMerges(node *yaml.Node) {
	for i, child := range node.Content {
		if node.Kind == yaml.MappingNode && i%2 == 0 && merging(child) && child.Style&yaml.TaggedStyle == 0 {
			child.Tag = ""
		}
		plainMerges(child)
	}
}

// Name of a mapping as a new node
func name(text string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text}
}

// Makes an empty mapping or list written inline, such as {}, written in lines once it holds something
func opened(node *yaml.Node) {
	if len(node.Content) == 0 {
		node.Style &^= yaml.FlowStyle
	}
}

// Puts the value under the path, making the mappings on the way, a value on the way that is no mapping replaced by one, and the value put
// into what is there as assign keeps it
func put(mapping *yaml.Node, path keyPath, value *yaml.Node, place *declared) {
	for i, part := range path.parts {
		at := keyAt(mapping, part, path.match[i])
		if at < 0 {
			// A name the mapping takes through << is written into it first, so the change stays here
			unmerged(mapping)
			at = keyAt(mapping, part, path.match[i])
		}
		if at < 0 {
			opened(mapping)
			mapping.Content = append(mapping.Content, name(part), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			at = len(mapping.Content) - 2
		} else if path.match[i] == spelled {
			mapping.Content[at].Value = part
		}
		next := own(mapping, at+1)
		if i == len(path.parts)-1 {
			assign(next, value, place)
			return
		}
		if next.Kind != yaml.MappingNode {
			*next = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Anchor: next.Anchor, HeadComment: next.HeadComment,
				LineComment: next.LineComment, FootComment: next.FootComment}
		}
		mapping = next
	}
}

// Removes the name at the end of the path, the mappings on the way kept even when it leaves them empty; whether there was one
func remove(mapping *yaml.Node, path keyPath) bool {
	for i, part := range path.parts {
		at := keyAt(mapping, part, path.match[i])
		if at < 0 {
			unmerged(mapping)
			if at = keyAt(mapping, part, path.match[i]); at < 0 {
				return false
			}
		}
		if i == len(path.parts)-1 {
			mapping.Content = slices.Delete(mapping.Content, at, at+2)
			return true
		}
		next := mapping.Content[at+1]
		if next.Kind == yaml.AliasNode {
			next = next.Alias
		}
		if next.Kind != yaml.MappingNode {
			return false
		}
		mapping = own(mapping, at+1)
	}
	return false
}

// Makes the node hold what the new one holds, keeping what a person wrote around it: its comments and its anchor, so an alias of it goes on
// standing for it; in a mapping the order of the names that stay, their comments, and their spelling except where the place takes names in
// any case and the new one spells one another way; in a list the comments of each item that stays
func assign(node, fresh *yaml.Node, place *declared) {
	switch {
	case node.Kind == yaml.MappingNode && fresh.Kind == yaml.MappingNode:
		unmerged(node)
		opened(node)
		keepMapping(node, fresh, place)
	case node.Kind == yaml.SequenceNode && fresh.Kind == yaml.SequenceNode:
		opened(node)
		record := place.record()
		for i, item := range fresh.Content {
			switch {
			case i >= len(node.Content):
				node.Content = append(node.Content, item)
			case node.Content[i].Kind == yaml.AliasNode:
				node.Content[i] = item
			default:
				assign(node.Content[i], item, record)
			}
		}
		node.Content = node.Content[:len(fresh.Content)]
	case node.Kind == yaml.ScalarNode && fresh.Kind == yaml.ScalarNode && node.Value == fresh.Value && node.ShortTag() == fresh.ShortTag():
	default:
		anchor, head, line, foot := node.Anchor, node.HeadComment, node.LineComment, node.FootComment
		*node = *fresh
		node.Anchor, node.HeadComment, node.LineComment, node.FootComment = anchor, head, line, foot
	}
}

// Names of the mapping made those of the new one, each name that stays in its place with its comments and its value kept as assign keeps
// it, the new ones after them
func keepMapping(node, fresh *yaml.Node, place *declared) {
	caseless := place.caseless()
	used := make([]bool, len(fresh.Content)/2)
	at := func(text string) int {
		var names []string
		for j := 0; j+1 < len(fresh.Content); j += 2 {
			if !used[j/2] {
				names = append(names, fresh.Content[j].Value)
			}
		}
		how := exact
		if caseless {
			how = spelled
		}
		found, ok := pick(names, text, how)
		if !ok {
			return -1
		}
		for j := 0; j+1 < len(fresh.Content); j += 2 {
			if !used[j/2] && fresh.Content[j].Value == found {
				return j
			}
		}
		return -1
	}
	var kept []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		j := at(key.Value)
		if j < 0 {
			continue
		}
		used[j/2] = true
		key.Value = fresh.Content[j].Value
		value := own(node, i+1)
		assign(value, fresh.Content[j+1], place.step(key.Value))
		kept = append(kept, key, value)
	}
	for j := 0; j+1 < len(fresh.Content); j += 2 {
		if !used[j/2] {
			kept = append(kept, fresh.Content[j], fresh.Content[j+1])
		}
	}
	node.Content = kept
}

// Spells the names icebath declares in the node as declared, all the way down, merging two that then share a spelling: the one spelled so
// already wins, else the first in order. Names of entries and names inside groups of values stay as written. Whether it changed anything
func (d *declared) respell(node *yaml.Node) bool {
	if d == nil {
		return false
	}
	changed := false
	switch {
	case node.Kind == yaml.SequenceNode:
		if record := d.record(); record != nil {
			for _, item := range node.Content {
				changed = record.respell(item) || changed
			}
		}
		return changed
	case node.Kind != yaml.MappingNode:
		return false
	case d.setting != nil:
		if d.setting.Kind() == Table {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if !merging(node.Content[i]) {
					changed = d.entry(node.Content[i].Value).respell(node.Content[i+1]) || changed
				}
			}
		}
		return changed
	}
	written := make([]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		written[i/2] = key.Value
		if merging(key) {
			continue
		}
		spelling, next := d.child(key.Value)
		if spelling != key.Value {
			key.Value, changed = spelling, true
		}
		changed = next.respell(node.Content[i+1]) || changed
	}
	return dedupe(node, written) || changed
}

// Merges the names of the mapping that share a spelling into the place of the first: the one that was written so wins, else the one written
// first in order; whether it merged any
func dedupe(node *yaml.Node, written []string) bool {
	changed := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		for j := i + 2; j+1 < len(node.Content); {
			a, b := node.Content[i], node.Content[j]
			if merging(a) || merging(b) || a.Value != b.Value {
				j += 2
				continue
			}
			wa, wb := written[i/2], written[j/2]
			if wa != a.Value && (wb == b.Value || wb < wa) {
				node.Content[i+1], node.Content[j+1] = node.Content[j+1], node.Content[i+1]
				written[i/2] = wb
			}
			mergeNodes(node.Content[i+1], node.Content[j+1])
			node.Content = slices.Delete(node.Content, j, j+2)
			written = slices.Delete(written, j/2, j/2+1)
			changed = true
		}
	}
	return changed
}

// Adds to the winner the names only the other holds, mappings merged all the way down
func mergeNodes(winner, other *yaml.Node) {
	if winner.Kind != yaml.MappingNode || other.Kind != yaml.MappingNode {
		return
	}
	for j := 0; j+1 < len(other.Content); j += 2 {
		if merging(other.Content[j]) {
			continue
		}
		if at := keyAt(winner, other.Content[j].Value, exact); at >= 0 {
			mergeNodes(winner.Content[at+1], other.Content[j+1])
		} else {
			winner.Content = append(winner.Content, other.Content[j], other.Content[j+1])
		}
	}
}

// Whether text holds comments and nothing else
func commentsOnly(text []byte) bool {
	found := false
	for line := range strings.SplitSeq(string(text), "\n") {
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			found = true
		default:
			return false
		}
	}
	return found
}

// Spaces a file indents with: the fewest that a line of a setting starts with, two for a file that indents nothing
func indentOf(text []byte) int {
	least := 0
	for line := range strings.SplitSeq(string(text), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == line {
			continue
		}
		if n := len(line) - len(trimmed); least == 0 || n < least {
			least = n
		}
	}
	return min(max(least, 2), 8)
}

// Text of the document indented as the text it was read from, after the comments that text held when it held nothing else
func render(doc *yaml.Node, before []byte, empty bool) ([]byte, error) {
	plainMerges(doc)
	var b bytes.Buffer
	if empty && commentsOnly(before) {
		b.Write(bytes.TrimRight(before, "\n"))
		b.WriteString("\n\n")
		doc.HeadComment, doc.FootComment = "", ""
	}
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(indentOf(before))
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
