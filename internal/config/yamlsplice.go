package config

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Raised when spliced text would not read back as the value it was meant to hold, nothing being written
var ErrSpliceMismatch = errors.New("rewritten document does not read back as the value written")

// Source text indexed by line
type srcText struct {
	text   string
	starts []int
}

// Indexes text, which always ends with a line break once non-empty
func newSrcText(text string) *srcText {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	t := &srcText{text: text, starts: []int{0}}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			t.starts = append(t.starts, i+1)
		}
	}
	if t.starts[len(t.starts)-1] != len(text) {
		t.starts = append(t.starts, len(text))
	}
	return t
}

// Count of indexed text rows
func (t *srcText) lines() int { return len(t.starts) - 1 }

// Text of the zero based row i without its break
func (t *srcText) line(i int) string {
	if i < 0 || i >= t.lines() {
		return ""
	}
	return strings.TrimSuffix(t.text[t.starts[i]:t.starts[i+1]], "\n")
}

// Byte position of a one based row and rune column
func (t *srcText) offset(line, col int) int {
	i := line - 1
	if i < 0 {
		return 0
	}
	if i >= t.lines() {
		return len(t.text)
	}
	l := t.line(i)
	off := t.starts[i]
	for n := 1; n < col && off-t.starts[i] < len(l); n++ {
		_, size := utf8.DecodeRuneInString(t.text[off:])
		off += size
	}
	return off
}

// Zero based line holding the byte at off
func (t *srcText) lineAt(off int) int {
	i := sort.SearchInts(t.starts, off+1) - 1
	return max(0, min(i, t.lines()-1))
}

// Byte offset where line i ends, before its break
func (t *srcText) lineEnd(i int) int { return t.starts[i] + len(t.line(i)) }

// Leading spaces of line i
func (t *srcText) indent(i int) int {
	l := t.line(i)
	return len(l) - len(strings.TrimLeft(l, " "))
}

// Reports a line holding only spaces and tabs
func (t *srcText) blank(i int) bool { return strings.TrimSpace(t.line(i)) == "" }

// Reports a row whose first non space character is a hash mark
func (t *srcText) comment(i int) bool {
	return strings.HasPrefix(strings.TrimLeft(t.line(i), " \t"), "#")
}

// Zero based rune position within its row of a byte position
func (t *srcText) column(off int) int {
	return utf8.RuneCountInString(t.text[t.starts[t.lineAt(off)]:off])
}

// One replacement of a byte range of the source, applied in creation order among equal positions
type textEdit struct {
	start, end int
	text       string
	seq        int
}

// Unit left in place whose value reads anchors elsewhere, rewritten whole when one changes after it was weighed
type aliasDependent struct {
	start, end int
	refs       []string
	weighed    map[string]bool
	stack      []string
	edits      [2]int
	fix        func()
}

// Raw bytes behind the indexed text, the byte position of every text offset and the line break new lines get
type rawText struct {
	raw       string
	at        []int
	nl        string
	virtualNL bool
}

// Maps the normalized text back onto raw, a final line break the text adds standing at the raw end
func newRawText(raw, text string) rawText {
	r := rawText{raw: raw, at: make([]int, len(text)+1), nl: lineBreakOf(raw)}
	p := len(raw) - len(strings.TrimPrefix(raw, "\xef\xbb\xbf"))
	for j := 0; j < len(text); j++ {
		r.at[j] = min(p, len(raw))
		switch {
		case p >= len(raw):
			r.virtualNL = true
		case raw[p] == '\r' && p+1 < len(raw) && raw[p+1] == '\n':
			p += 2
		default:
			p++
		}
	}
	r.at[len(text)] = len(raw)
	return r
}

// Line break raw uses first, the platform one for text without any
func lineBreakOf(raw string) string {
	i := strings.IndexAny(raw, "\r\n")
	switch {
	case i < 0:
		return nativeNewline
	case strings.HasPrefix(raw[i:], "\r\n"):
		return "\r\n"
	case raw[i] == '\r':
		return "\r"
	}
	return "\n"
}

// Reconciler of a desired value tree into the source text it came from
type splicer struct {
	t         *srcText
	r         rawText
	style     yamlStyle
	edits     []textEdit
	deps      []aliasDependent
	anchors   map[string]*yaml.Node
	desiredAt map[string]*yaml.Node
	dirty     map[string]bool
	gone      map[string]bool
	stack     []string
	values    map[*yaml.Node]*yaml.Node
	seq       int
	err       error
}

// Raw source with the document holding desired, untouched bytes kept and changed values written in its line breaks
func spliceDocument(raw, src string, doc *yaml.Node, desired *yaml.Node, style yamlStyle) (string, error) {
	t := newSrcText(src)
	sp := &splicer{t: t, r: newRawText(raw, t.text), style: style, anchors: map[string]*yaml.Node{}, desiredAt: map[string]*yaml.Node{},
		dirty: map[string]bool{}, gone: map[string]bool{}, values: map[*yaml.Node]*yaml.Node{}}
	var root *yaml.Node
	if doc != nil && len(doc.Content) > 0 {
		root = doc.Content[0]
		sp.collectAnchors(root)
	}
	if desired != nil && desired.Kind == yaml.DocumentNode {
		if len(desired.Content) == 0 {
			desired = nil
		} else {
			desired = desired.Content[0]
		}
	}
	sp.renderDocument(root, desired)
	sp.settleAliases()
	if sp.err != nil {
		return "", sp.err
	}
	out, err := sp.apply()
	if err != nil {
		return "", err
	}
	if err := verifySplice(out, desired); err != nil {
		return "", err
	}
	return out, nil
}

// Reports whether the raw bytes read back as want
func verifySplice(raw string, want *yaml.Node) error {
	text, err := yamlText([]byte(raw))
	if err != nil {
		return errors.Join(ErrSpliceMismatch, err)
	}
	doc, err := parseYAMLText(text)
	if err != nil {
		return errors.Join(ErrSpliceMismatch, err)
	}
	var got *yaml.Node
	if doc != nil {
		if got, err = valueTree(doc.Content[0]); err != nil {
			return errors.Join(ErrSpliceMismatch, err)
		}
	}
	if (want == nil || isNull(want)) && (got == nil || isNull(got)) {
		return nil
	}
	if !valueEqual(got, want, true) {
		return ErrSpliceMismatch
	}
	return nil
}

// Anchors defined below n by name
func (sp *splicer) collectAnchors(n *yaml.Node) {
	if n == nil || n.Kind == yaml.AliasNode {
		return
	}
	if n.Anchor != "" {
		sp.anchors[n.Anchor] = n
	}
	for _, c := range n.Content {
		sp.collectAnchors(c)
	}
}

// Value tree of a source node, memoized
func (sp *splicer) value(n *yaml.Node) *yaml.Node {
	if v, ok := sp.values[n]; ok {
		return v
	}
	v, err := valueTree(n)
	if err != nil {
		sp.fail(err)
		v = nullNode()
	}
	sp.values[n] = v
	return v
}

// Records the first failure
func (sp *splicer) fail(err error) {
	if sp.err == nil {
		sp.err = err
	}
}

// Records a replacement and marks every anchor whose definition encloses it as changed
func (sp *splicer) edit(start, end int, text string) {
	sp.edits = append(sp.edits, textEdit{start: start, end: end, text: text, seq: sp.seq})
	sp.seq++
	for _, a := range sp.stack {
		sp.dirty[a] = true
	}
}

// Marks every anchor defined inside a subtree that is being removed or rewritten as changed and no longer defined
func (sp *splicer) dropAnchorsIn(n *yaml.Node) {
	if n == nil || n.Kind == yaml.AliasNode {
		return
	}
	if n.Anchor != "" {
		sp.dirty[n.Anchor] = true
		sp.gone[n.Anchor] = true
	}
	for _, c := range n.Content {
		sp.dropAnchorsIn(c)
	}
}

// Reconciles the root, a source without one receiving the desired text appended
func (sp *splicer) renderDocument(root, desired *yaml.Node) {
	if root == nil {
		if desired == nil || isNull(desired) {
			return
		}
		text := sp.style.renderDocument(desired)
		sp.edit(len(sp.t.text), len(sp.t.text), text)
		return
	}
	replace := func(d *yaml.Node) {
		sp.dropAnchorsIn(root)
		start := sp.t.offset(root.Line, root.Column)
		end := max(start, sp.endOf(root, -1))
		text := strings.TrimSuffix(sp.style.renderDocument(d), "\n")
		sp.edit(start, end, text)
	}
	if desired == nil {
		desired = nullNode()
	}
	sp.merge(root, desired, -1, replace)
}

// Reconciles one source value with its desired value, calling replace when the source cannot be kept or edited in place
func (sp *splicer) merge(s, d *yaml.Node, parentIndent int, replace func(d *yaml.Node)) {
	if s.Anchor != "" {
		sp.desiredAt[s.Anchor] = d
		sp.stack = append(sp.stack, s.Anchor)
		defer func() { sp.stack = sp.stack[:len(sp.stack)-1] }()
	}
	block := s.Style&yaml.FlowStyle == 0
	blockMapping := s.Kind == yaml.MappingNode && d.Kind == yaml.MappingNode && block && len(s.Content) > 0 && !sp.atomicMapping(s)
	switch {
	case blockMapping && !hasMergeKey(s):
		if len(d.Content) == 0 || !sp.mergeMapping(s, d) {
			replace(d)
		}
		return
	case blockMapping:
		if len(d.Content) == 0 || !sp.mergeInherited(s, d, parentIndent, replace) {
			replace(d)
		}
		return
	case s.Kind == yaml.SequenceNode && d.Kind == yaml.SequenceNode && block && len(s.Content) > 0:
		if len(d.Content) == 0 || !sp.mergeSequence(s, d) {
			replace(d)
		}
		return
	}
	if !valueEqual(sp.value(s), d, true) {
		if !block && s.Kind == d.Kind && len(d.Content) > 0 {
			if sp.mergeFlow(s, d) {
				return
			}
			flow := *d
			flow.Style |= yaml.FlowStyle
			d = &flow
		}
		replace(d)
		return
	}
	if refs := aliasRefs(s, nil); len(refs) > 0 {
		start := sp.t.offset(s.Line, s.Column)
		sp.deps = append(sp.deps, aliasDependent{start: start, end: sp.endOf(s, parentIndent), refs: refs, stack: slices.Clone(sp.stack), fix: func() { replace(d) }})
	}
}

// Anchor names the aliases below n read
func aliasRefs(n *yaml.Node, out []string) []string {
	if n == nil {
		return out
	}
	if n.Kind == yaml.AliasNode {
		return append(out, n.Value)
	}
	for _, c := range n.Content {
		out = aliasRefs(c, out)
	}
	return out
}

// Reports a mapping edited only as a whole, one with alias or collection keys, or explicit keys
func (sp *splicer) atomicMapping(m *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind != yaml.ScalarNode || (!isMergeKey(k) && sp.explicitKey(k)) {
			return true
		}
	}
	return false
}

// Reports a merge key, the key whose value lends its entries to the mapping
func isMergeKey(k *yaml.Node) bool { return k.Kind == yaml.ScalarNode && k.ShortTag() == mergeTag }

// Reports a mapping holding a merge key
func hasMergeKey(m *yaml.Node) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isMergeKey(m.Content[i]) {
			return true
		}
	}
	return false
}

// Entries a mapping inherits through its merge keys, an anchor edited earlier in this splice read at its new value
func (sp *splicer) inheritedPairs(m *yaml.Node) ([]pair, map[string]bool, bool) {
	weighed := map[string]bool{}
	source := func(n *yaml.Node) (*yaml.Node, bool) {
		if n.Kind == yaml.AliasNode {
			if sp.gone[n.Value] {
				return nil, false
			}
			if sp.dirty[n.Value] {
				weighed[n.Value] = true
				v, ok := sp.desiredAt[n.Value]
				return v, ok && isMapping(v)
			}
		}
		v := sp.value(n)
		return v, isMapping(v)
	}
	var merged []pair
	for i := 0; i+1 < len(m.Content); i += 2 {
		if !isMergeKey(m.Content[i]) {
			continue
		}
		v := m.Content[i+1]
		items := []*yaml.Node{v}
		if v.Kind == yaml.SequenceNode {
			items = slices.Clone(v.Content)
			slices.Reverse(items)
		}
		for _, it := range items {
			src, ok := source(it)
			if !ok {
				return nil, nil, false
			}
			merged = overlayPairs(merged, mappingPairs(src))
		}
	}
	return merged, weighed, true
}

// Reconciliation of a block mapping with merge keys, own keys edited and inherited values overridden by new own keys
func (sp *splicer) mergeInherited(s, d *yaml.Node, parentIndent int, replace func(d *yaml.Node)) bool {
	inherited, weighed, ok := sp.inheritedPairs(s)
	if !ok {
		return false
	}
	indent := s.Content[0].Column - 1
	want := map[string]*yaml.Node{}
	for i := 0; i+1 < len(d.Content); i += 2 {
		want[d.Content[i].Value] = d.Content[i+1]
	}
	lent := map[string]*yaml.Node{}
	for _, p := range inherited {
		lent[p.key.Value] = p.value
	}
	owned := map[string]bool{}
	for i := 0; i+1 < len(s.Content); i += 2 {
		k := s.Content[i]
		if isMergeKey(k) {
			continue
		}
		owned[k.Value] = true
		if _, kept := want[k.Value]; !kept {
			if _, shadows := lent[k.Value]; shadows || (i == 0 && sp.sharesLine(k)) {
				return false
			}
		}
	}
	for k := range lent {
		if _, kept := want[k]; !kept && !owned[k] {
			return false
		}
	}
	first := sp.seq
	for i := 0; i+1 < len(s.Content); i += 2 {
		k, v := s.Content[i], s.Content[i+1]
		if isMergeKey(k) {
			continue
		}
		dv, kept := want[k.Value]
		if !kept {
			sp.deleteLines(sp.t.offset(k.Line, k.Column), sp.entryEnd(k, v, indent), v, i+2 < len(s.Content))
			continue
		}
		sp.merge(v, dv, indent, func(nd *yaml.Node) { sp.replaceEntryValue(k, v, indent, nd) })
	}
	sp.appendEntries(s, d, indent, func(key string) bool {
		if owned[key] {
			return true
		}
		v, ok := lent[key]
		return ok && valueEqual(v, want[key], true)
	})
	var refs []string
	for i := 0; i+1 < len(s.Content); i += 2 {
		if isMergeKey(s.Content[i]) {
			refs = aliasRefs(s.Content[i+1], refs)
		}
	}
	if len(refs) > 0 {
		start := sp.t.offset(s.Line, s.Column)
		sp.deps = append(sp.deps, aliasDependent{start: start, end: sp.endOf(s, parentIndent), refs: refs, weighed: weighed,
			stack: slices.Clone(sp.stack), edits: [2]int{first, sp.seq}, fix: func() { replace(d) }})
	}
	return true
}

// Writes the desired entries skip does not name after the last entry of a block mapping
func (sp *splicer) appendEntries(s, d *yaml.Node, indent int, skip func(key string) bool) {
	w := newYAMLWriter(sp.style)
	added := false
	for i := 0; i+1 < len(d.Content); i += 2 {
		if skip(d.Content[i].Value) {
			continue
		}
		added = true
		w.indentTo(indent)
		w.writeEntry(d.Content[i], d.Content[i+1], indent)
	}
	if added {
		last := s.Content[len(s.Content)-2]
		at := sp.insertionPoint(sp.entryEnd(last, s.Content[len(s.Content)-1], indent), indent)
		sp.edit(at, at, w.b.String())
	}
}

// Same shape reconciliation of a flow collection, scalars rewritten in place, false when it must be rewritten whole
func (sp *splicer) mergeFlow(s, d *yaml.Node) bool {
	if len(aliasRefs(s, nil)) > 0 {
		return false
	}
	var plan []textEdit
	if !sp.planFlow(s, d, &plan) {
		return false
	}
	for _, e := range plan {
		sp.edit(e.start, e.end, e.text)
	}
	return true
}

// Collects the in place scalar rewrites turning flow node s into d, false when the shapes differ
func (sp *splicer) planFlow(s, d *yaml.Node, plan *[]textEdit) bool {
	switch {
	case s.Kind == yaml.MappingNode && d.Kind == yaml.MappingNode:
		if len(s.Content) != len(d.Content) || sp.atomicMapping(s) || hasMergeKey(s) {
			return false
		}
		for i := 0; i+1 < len(s.Content); i += 2 {
			at := mappingIndex(d, s.Content[i].Value)
			if at < 0 || !sp.planFlow(s.Content[i+1], d.Content[at], plan) {
				return false
			}
		}
		return true
	case s.Kind == yaml.SequenceNode && d.Kind == yaml.SequenceNode:
		if len(s.Content) != len(d.Content) {
			return false
		}
		for i := range s.Content {
			if !sp.planFlow(s.Content[i], d.Content[i], plan) {
				return false
			}
		}
		return true
	}
	if valueEqual(sp.value(s), d, true) {
		return true
	}
	if s.Kind != yaml.ScalarNode || d.Kind != yaml.ScalarNode || emptyScalar(s) || s.Anchor != "" {
		return false
	}
	start := sp.t.offset(s.Line, s.Column)
	end := sp.flowScalarEnd(s)
	if end <= start || sp.t.lineAt(start) != sp.t.lineAt(end-1) {
		return false
	}
	w := newYAMLWriter(sp.style)
	w.writeScalar(d, 0, false, true, false)
	if strings.Contains(w.b.String(), "\n") {
		return false
	}
	*plan = append(*plan, textEdit{start: start, end: end, text: w.b.String()})
	return true
}

// Offset past a scalar inside a flow collection, before the indicator, comment or line end that closes it
func (sp *splicer) flowScalarEnd(n *yaml.Node) int {
	text := sp.t.text
	p := sp.skipProps(sp.t.offset(n.Line, n.Column))
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		return sp.quotedEnd(p)
	}
	closes := func(i int) bool {
		switch c := text[i]; {
		case strings.ContainsRune(",]}\n", rune(c)):
			return true
		case c == '#':
			return i > p && (text[i-1] == ' ' || text[i-1] == '\t')
		case c == ':':
			return i+1 >= len(text) || strings.ContainsRune(" \t\n,]}", rune(text[i+1]))
		}
		return false
	}
	i := p
	for i < len(text) && !closes(i) {
		i++
	}
	for i > p && (text[i-1] == ' ' || text[i-1] == '\t') {
		i--
	}
	return i
}

// Reports a key written after a question mark indicator
func (sp *splicer) explicitKey(k *yaml.Node) bool {
	start := sp.t.offset(k.Line, k.Column)
	before := strings.TrimRight(sp.t.text[sp.t.starts[sp.t.lineAt(start)]:start], " \t")
	return strings.HasSuffix(before, "?")
}

// Reports a node whose line holds other text before it, such as the dash of the item it opens
func (sp *splicer) sharesLine(n *yaml.Node) bool {
	start := sp.t.offset(n.Line, n.Column)
	return strings.TrimLeft(sp.t.text[sp.t.starts[sp.t.lineAt(start)]:start], " ") != ""
}

// Key by key reconciliation of a block mapping, false when the mapping must be rewritten whole
func (sp *splicer) mergeMapping(s, d *yaml.Node) bool {
	indent := s.Content[0].Column - 1
	want := map[string]*yaml.Node{}
	for i := 0; i+1 < len(d.Content); i += 2 {
		want[d.Content[i].Value] = d.Content[i+1]
	}
	if _, kept := want[s.Content[0].Value]; !kept && sp.sharesLine(s.Content[0]) {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(s.Content); i += 2 {
		k, v := s.Content[i], s.Content[i+1]
		dv, kept := want[k.Value]
		if !kept {
			sp.deleteLines(sp.t.offset(k.Line, k.Column), sp.entryEnd(k, v, indent), v, i+2 < len(s.Content))
			continue
		}
		seen[k.Value] = true
		sp.merge(v, dv, indent, func(nd *yaml.Node) { sp.replaceEntryValue(k, v, indent, nd) })
	}
	sp.appendEntries(s, d, indent, func(key string) bool { return seen[key] })
	return true
}

// Item by item reconciliation of a block sequence, extra items appended and missing ones removed from the end
func (sp *splicer) mergeSequence(s, d *yaml.Node) bool {
	dash := s.Column - 1
	for i := range min(len(s.Content), len(d.Content)) {
		item, idx := s.Content[i], i
		sp.merge(item, d.Content[i], dash, func(nd *yaml.Node) { sp.replaceItem(s, idx, nd) })
	}
	if len(d.Content) < len(s.Content) {
		first := sp.dashOffset(s, len(d.Content))
		for _, item := range s.Content[len(d.Content):] {
			sp.dropAnchorsIn(item)
		}
		sp.deleteLines(first, sp.itemEnd(s, len(s.Content)-1), nil, false)
	}
	if len(d.Content) > len(s.Content) {
		w := newYAMLWriter(sp.style)
		for _, item := range d.Content[len(s.Content):] {
			w.indentTo(dash)
			w.write("-")
			w.writeItem(item, dash)
		}
		at := sp.insertionPoint(sp.itemEnd(s, len(s.Content)-1), dash)
		sp.edit(at, at, w.b.String())
	}
	return true
}

// Removes the lines from start through end, between blank lines also one blank when a sibling follows
func (sp *splicer) deleteLines(start, end int, removed *yaml.Node, followed bool) {
	sp.dropAnchorsIn(removed)
	first, last := sp.t.lineAt(start), sp.t.lineAt(max(start, end-1))
	if followed && (first == 0 || sp.t.blank(first-1)) && last+1 < sp.t.lines() && sp.t.blank(last+1) {
		last++
	}
	sp.edit(sp.t.starts[first], sp.t.starts[last+1], "")
}

// Offset where new children of a container go, after its last child and the deeper comments that follow it
func (sp *splicer) insertionPoint(lastEnd, containerIndent int) int {
	l := sp.t.lineAt(max(0, lastEnd-1))
	for l+1 < sp.t.lines() && !sp.t.blank(l+1) && sp.t.comment(l+1) && sp.t.indent(l+1) > containerIndent {
		l++
	}
	return sp.t.starts[l+1]
}

// Rewrites the value of a mapping entry, keeping the key text and the comment after the old value
func (sp *splicer) replaceEntryValue(k, s *yaml.Node, indent int, d *yaml.Node) {
	sp.dropAnchorsIn(s)
	colon := sp.colonEnd(k)
	keyLine := sp.t.lineAt(colon - 1)
	sStart, sEnd := colon, colon
	if !emptyScalar(s) {
		sStart = sp.t.offset(s.Line, s.Column)
		sEnd = max(colon, sp.endOf(s, indent))
	}
	w := newYAMLWriter(sp.style)
	w.col, w.whitespace = sp.t.column(colon), false
	w.value(d, indent)
	text := w.b.String()
	block := strings.HasPrefix(text, "\n") && text != "\n"
	inline := strings.TrimSuffix(text, "\n")
	switch {
	case sp.t.lineAt(sStart) == keyLine && !block && sStart > colon && strings.HasPrefix(inline, " "):
		sp.edit(sStart, sEnd, inline[1:])
	case sp.t.lineAt(sStart) == keyLine && !block:
		sp.edit(colon, sEnd, inline)
	case sp.t.lineAt(sStart) == keyLine:
		sp.edit(colon, sEnd, "")
		at := sp.t.starts[sp.t.lineAt(max(colon, sEnd-1))+1]
		sp.edit(at, at, text[1:])
	default:
		first, last := sp.t.lineAt(sStart), sp.t.lineAt(sEnd-1)
		sp.edit(sp.t.starts[first], sp.t.starts[last+1], "")
		if block {
			sp.edit(sp.t.starts[first], sp.t.starts[first], text[1:])
		} else {
			sp.edit(colon, colon, inline)
		}
	}
}

// Rewrites one sequence item after its dash, the dash and any text before it kept
func (sp *splicer) replaceItem(seq *yaml.Node, idx int, d *yaml.Node) {
	s := seq.Content[idx]
	sp.dropAnchorsIn(s)
	dash := sp.dashOffset(seq, idx)
	dashCol := sp.t.column(dash)
	end := sp.itemEnd(seq, idx)
	w := newYAMLWriter(sp.style)
	w.col, w.whitespace = dashCol+1, false
	w.writeItem(d, dashCol)
	text := strings.TrimSuffix(w.b.String(), "\n")
	start := dash + 1
	if !emptyScalar(s) {
		cs := sp.t.offset(s.Line, s.Column)
		if sp.t.lineAt(cs) != sp.t.lineAt(dash) {
			first, last := sp.t.lineAt(cs), sp.t.lineAt(end-1)
			sp.edit(sp.t.starts[first], sp.t.starts[last+1], "")
			sp.edit(start, start, text)
			return
		}
		if cs > start && strings.HasPrefix(text, " ") && !strings.Contains(text, "\n") {
			sp.edit(cs, max(cs, end), text[1:])
			return
		}
	}
	sp.edit(start, max(start, end), text)
}

// Reports a scalar with no text of its own, the empty value after a colon or dash
func emptyScalar(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Value == "" && n.Style == 0 && n.Anchor == "" && n.ShortTag() == tagNull
}

// Offset of the dash that opens item idx of a block sequence, the first dash being where the sequence starts
func (sp *splicer) dashOffset(seq *yaml.Node, idx int) int {
	if idx == 0 {
		return sp.t.offset(seq.Line, seq.Column)
	}
	dashCol := seq.Column - 1
	c := seq.Content[idx]
	floor := seq.Line - 1
	for l := sp.t.lineAt(sp.t.offset(c.Line, c.Column)); l > floor; l-- {
		prefix, rest := runePrefix(sp.t.line(l), dashCol)
		if strings.TrimLeft(prefix, " ") == "" && strings.HasPrefix(rest, "-") && (len(rest) == 1 || rest[1] == ' ' || rest[1] == '\t') {
			return sp.t.starts[l] + len(prefix)
		}
	}
	return sp.t.offset(c.Line, c.Column)
}

// First n runes of s and the rest
func runePrefix(s string, n int) (string, string) {
	i := 0
	for k := 0; k < n && i < len(s); k++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i], s[i:]
}

// End offset of item idx of a block sequence
func (sp *splicer) itemEnd(seq *yaml.Node, idx int) int {
	dash := sp.dashOffset(seq, idx)
	c := seq.Content[idx]
	if emptyScalar(c) {
		return dash + 1
	}
	return max(dash+1, sp.endOf(c, seq.Column-1))
}

// End offset of a mapping entry, the colon for an empty value
func (sp *splicer) entryEnd(k, v *yaml.Node, indent int) int {
	colon := sp.colonEnd(k)
	if emptyScalar(v) {
		return colon
	}
	return max(colon, sp.endOf(v, indent))
}

// Offset just past the colon that ends a mapping key
func (sp *splicer) colonEnd(k *yaml.Node) int {
	text := sp.t.text
	p := sp.t.offset(k.Line, k.Column)
	switch {
	case k.Kind == yaml.AliasNode:
		p += 1 + len(k.Value)
	case k.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
		p = sp.quotedEnd(sp.skipProps(p))
	case k.Kind == yaml.ScalarNode:
		for p < len(text) && text[p] != '\n' && !(text[p] == ':' && (p+1 >= len(text) || strings.ContainsRune(" \t\n", rune(text[p+1])))) {
			p++
		}
	}
	for p < len(text) {
		switch c := text[p]; {
		case c == ':':
			return p + 1
		case c == '#':
			for p < len(text) && text[p] != '\n' {
				p++
			}
		default:
			p++
		}
	}
	return p
}

// Offset past the anchor and tag properties starting at p
func (sp *splicer) skipProps(p int) int {
	text := sp.t.text
	for p < len(text) && (text[p] == '&' || text[p] == '!') {
		for p < len(text) && !strings.ContainsRune(" \t\n", rune(text[p])) {
			p++
		}
		for p < len(text) && (text[p] == ' ' || text[p] == '\t') {
			p++
		}
	}
	return p
}

// Offset just past the last character of a source node, its block content included
func (sp *splicer) endOf(n *yaml.Node, parentIndent int) int {
	start := sp.t.offset(n.Line, n.Column)
	switch n.Kind {
	case yaml.AliasNode:
		return start + 1 + len(n.Value)
	case yaml.MappingNode:
		if n.Style&yaml.FlowStyle != 0 || len(n.Content) == 0 {
			return sp.flowEnd(sp.skipProps(start))
		}
		k, v := n.Content[len(n.Content)-2], n.Content[len(n.Content)-1]
		return sp.entryEnd(k, v, k.Column-1)
	case yaml.SequenceNode:
		if n.Style&yaml.FlowStyle != 0 || len(n.Content) == 0 {
			return sp.flowEnd(sp.skipProps(start))
		}
		return sp.itemEnd(n, len(n.Content)-1)
	}
	p := sp.skipProps(start)
	switch {
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return sp.blockScalarEnd(p, parentIndent)
	case n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
		return sp.quotedEnd(p)
	case emptyScalar(n):
		return start
	}
	return sp.plainEnd(p, parentIndent)
}

// Offset past a quoted scalar starting with its quote at p
func (sp *splicer) quotedEnd(p int) int {
	text := sp.t.text
	if p >= len(text) {
		return p
	}
	q := text[p]
	for i := p + 1; i < len(text); i++ {
		switch {
		case q == '"' && text[i] == '\\':
			i++
		case text[i] == q && q == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case text[i] == q:
			return i + 1
		}
	}
	return len(text)
}

// Offset past a flow collection opening at p, quoted text and comments inside skipped
func (sp *splicer) flowEnd(p int) int {
	text := sp.t.text
	depth := 0
	for i := p; i < len(text); i++ {
		switch c := text[i]; {
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
			if depth <= 0 {
				return i + 1
			}
		case c == '"' || c == '\'':
			i = sp.quotedEnd(i) - 1
		case c == '#' && i > 0 && strings.ContainsRune(" \t\n", rune(text[i-1])):
			for i < len(text) && text[i] != '\n' {
				i++
			}
		}
	}
	return len(text)
}

// Offset past the plain text on the line from p, before a comment and trailing blanks
func (sp *splicer) plainLineEnd(p int) int {
	text := sp.t.text
	end := sp.t.lineEnd(sp.t.lineAt(p))
	i := p
	for i < end && !((text[i] == ' ' || text[i] == '\t') && i+1 < end && text[i+1] == '#') {
		i++
	}
	for i > p && (text[i-1] == ' ' || text[i-1] == '\t') {
		i--
	}
	return i
}

// Offset past a plain scalar starting at p, its more indented continuation lines included
func (sp *splicer) plainEnd(p, parentIndent int) int {
	end := sp.plainLineEnd(p)
	for l := sp.t.lineAt(p) + 1; l < sp.t.lines(); l++ {
		if sp.t.blank(l) {
			continue
		}
		if sp.t.indent(l) <= parentIndent || sp.t.comment(l) || documentMarker(sp.t.line(l)) {
			break
		}
		end = sp.plainLineEnd(sp.t.starts[l] + sp.t.indent(l))
	}
	return end
}

// Reports a document start or end marker line
func documentMarker(line string) bool {
	return strings.HasPrefix(line, "---") || strings.HasPrefix(line, "...")
}

// Offset past the last content line of a literal or folded scalar whose indicator is at p
func (sp *splicer) blockScalarEnd(p, parentIndent int) int {
	header := sp.t.lineAt(p)
	end := sp.plainLineEnd(p)
	contentIndent := -1
	h := sp.t.text[p:end]
	for _, r := range h[1:] {
		if r >= '1' && r <= '9' {
			contentIndent = max(parentIndent, 0) + int(r-'0')
		}
	}
	for l := header + 1; l < sp.t.lines(); l++ {
		if sp.t.blank(l) {
			continue
		}
		ind := sp.t.indent(l)
		if contentIndent < 0 {
			if ind <= parentIndent {
				break
			}
			contentIndent = ind
		}
		if ind < contentIndent {
			break
		}
		end = sp.t.lineEnd(l)
	}
	return end
}

// Rewrites every unit that reads a changed anchor, repeating until no further anchor changes
func (sp *splicer) settleAliases() {
	done := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for i, dep := range sp.deps {
			if done[i] || !sp.stale(dep) || sp.covered(dep.start, dep.end) {
				continue
			}
			done[i] = true
			changed = true
			sp.edits = slices.DeleteFunc(sp.edits, func(e textEdit) bool { return e.seq >= dep.edits[0] && e.seq < dep.edits[1] })
			saved := sp.stack
			sp.stack = dep.stack
			dep.fix()
			sp.stack = saved
		}
	}
}

// Reports a unit reading an anchor now undefined, or changed after the unit was weighed against it
func (sp *splicer) stale(dep aliasDependent) bool {
	for _, n := range dep.refs {
		if sp.gone[n] || (sp.dirty[n] && !dep.weighed[n]) {
			return true
		}
	}
	return false
}

// Reports a range already inside an edited range
func (sp *splicer) covered(start, end int) bool {
	for _, e := range sp.edits {
		if e.start <= start && end <= e.end && e.end > e.start {
			return true
		}
	}
	return false
}

// Spaces opening an inserted text, deeper content ordered before shallower content at one position
func leadingSpaces(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// Raw source with every edit applied in position order in its line breaks, overlapping edits refused
func (sp *splicer) apply() (string, error) {
	edits := slices.Clone(sp.edits)
	slices.SortStableFunc(edits, func(a, b textEdit) int {
		if a.start != b.start {
			return a.start - b.start
		}
		if (a.end == a.start) != (b.end == b.start) {
			if a.end == a.start {
				return -1
			}
			return 1
		}
		if ia, ib := leadingSpaces(a.text), leadingSpaces(b.text); a.end == a.start && ia != ib {
			return ib - ia
		}
		return a.seq - b.seq
	})
	var b strings.Builder
	pos, rawPos := 0, 0
	broken := sp.r.virtualNL
	for _, e := range edits {
		if e.start < pos {
			return "", ErrSpliceMismatch
		}
		b.WriteString(sp.r.raw[rawPos:sp.r.at[e.start]])
		text := e.text
		if broken && text != "" && e.start == len(sp.t.text) {
			text, broken = "\n"+text, false
		}
		b.WriteString(strings.ReplaceAll(text, "\n", sp.r.nl))
		pos, rawPos = e.end, sp.r.at[e.end]
	}
	b.WriteString(sp.r.raw[rawPos:])
	return b.String(), nil
}
