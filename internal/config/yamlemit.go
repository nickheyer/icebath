package config

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Column past which the safe dumper folds long scalars at a space
const safeDumpWidth = 80

// Scalar and layout rules of one writer, the safe dumper or the comment keeping editor
type yamlStyle struct {
	width      int
	roundTrip  bool
	quoteWords bool
}

// Rules of the safe dumper, folding at eighty columns and writing null as null
func safeStyle() yamlStyle { return yamlStyle{width: safeDumpWidth} }

// Rules of the round trip save, never folding and quoting YAML 1.1 words in double quotes
func roundTripStyle() yamlStyle {
	return yamlStyle{width: math.MaxInt32, roundTrip: true, quoteWords: true}
}

// Rules of a single key write, never folding and leaving YAML 1.1 words to the single quotes the emitter picks
func keyWriteStyle() yamlStyle { return yamlStyle{width: math.MaxInt32, roundTrip: true} }

// Reports a collection kept in flow style, which only the round trip editor honors
func (s yamlStyle) flowStyled(n *yaml.Node) bool {
	return s.roundTrip && n.Style&yaml.FlowStyle != 0
}

// Strings a YAML 1.1 reader takes as a boolean or null, double quoted by the round trip editor
func ambiguousWords() map[string]bool {
	return map[string]bool{"y": true, "n": true, "yes": true, "no": true, "true": true, "false": true, "on": true, "off": true, "null": true, "~": true}
}

// Text being written with the rune column of its current line
type yamlWriter struct {
	style      yamlStyle
	b          strings.Builder
	col        int
	whitespace bool
}

// Writer starting a fresh line
func newYAMLWriter(s yamlStyle) *yamlWriter { return &yamlWriter{style: s, whitespace: true} }

// Appends text, tracking the column of the line it ends on
func (w *yamlWriter) write(s string) {
	w.b.WriteString(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		w.col = utf8.RuneCountInString(s[i+1:])
	} else {
		w.col += utf8.RuneCountInString(s)
	}
	if s != "" {
		w.whitespace = strings.HasSuffix(s, " ") || strings.HasSuffix(s, "\n")
	}
}

// Ends the current line
func (w *yamlWriter) newline() {
	w.write("\n")
	w.whitespace = true
}

// Moves to column n, starting a new line unless the cursor already rests at n after whitespace
func (w *yamlWriter) indentTo(n int) {
	if w.col > n || (w.col > 0 && w.col < n && !w.whitespace) {
		w.newline()
	}
	if w.col < n {
		w.write(strings.Repeat(" ", n-w.col))
	}
	w.whitespace = true
}

// Whole document text for a value tree, a trailing document end marker after a plain root scalar
func (s yamlStyle) renderDocument(n *yaml.Node) string {
	n = resolveAlias(n)
	if n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			n = nil
		} else {
			n = resolveAlias(n.Content[0])
		}
	}
	w := newYAMLWriter(s)
	switch {
	case n != nil && n.Kind == yaml.MappingNode && len(n.Content) > 0 && !s.flowStyled(n):
		w.blockMapping(n, 0)
	case n != nil && n.Kind == yaml.SequenceNode && len(n.Content) > 0 && !s.flowStyled(n):
		w.blockSequence(n, 2)
	case n != nil && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode):
		w.write(w.flowForm(n))
		w.newline()
	default:
		if n == nil {
			n = nullNode()
		}
		plain := w.writeScalar(n, 2, false, false, true)
		w.newline()
		if plain {
			w.write("...")
			w.newline()
		}
	}
	return w.b.String()
}

// Block mapping with its keys at column indent, every entry ending its line
func (w *yamlWriter) blockMapping(n *yaml.Node, indent int) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		w.indentTo(indent)
		w.writeEntry(n.Content[i], n.Content[i+1], indent)
	}
}

// One mapping entry at the cursor, its key written in the simple form when short and single line
func (w *yamlWriter) writeEntry(k, v *yaml.Node, indent int) {
	if simpleKey(k) {
		w.writeScalar(k, indent+2, true, false, false)
		w.write(":")
	} else {
		w.write("?")
		w.writeScalar(k, indent+2, false, false, false)
		w.newline()
		w.indentTo(indent)
		w.write(":")
	}
	w.value(v, indent)
}

// Reports a key the emitter writes before the colon, its tag and text under 128 runes and without line breaks
func simpleKey(k *yaml.Node) bool {
	k = resolveAlias(k)
	if k == nil || k.Kind != yaml.ScalarNode {
		return false
	}
	tag, text := scalarKind(k)
	shown := text
	switch tag {
	case tagNull:
		shown = "null"
	case tagBool:
		shown, _ = resolveTagged(tagBool, text)
	case tagInt:
		if v, ok := constructInt(text); ok {
			shown = v.String()
		}
	case tagFloat:
		if f, ok := constructFloat(text); ok {
			shown = reprFloat(f)
		}
	}
	return utf8.RuneCountInString(tagText(tag))+utf8.RuneCountInString(shown) < 128 && !strings.ContainsFunc(shown, isBreak)
}

// Value of a mapping entry after its colon, nested blocks below and scalars on the key line
func (w *yamlWriter) value(v *yaml.Node, indent int) {
	v = resolveAlias(v)
	if v == nil {
		v = nullNode()
	}
	switch {
	case v.Kind == yaml.MappingNode && len(v.Content) > 0 && !w.style.flowStyled(v):
		w.newline()
		w.blockMapping(v, indent+2)
	case v.Kind == yaml.SequenceNode && len(v.Content) > 0 && !w.style.flowStyled(v):
		w.newline()
		w.blockSequence(v, indent+2)
	case v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode:
		w.write(" " + w.flowForm(v))
		w.newline()
	default:
		w.writeScalar(v, indent+2, false, false, false)
		w.newline()
	}
}

// Block sequence with its dashes at column dash and item content two columns further
func (w *yamlWriter) blockSequence(n *yaml.Node, dash int) {
	for _, item := range n.Content {
		w.indentTo(dash)
		w.write("-")
		w.writeItem(item, dash)
	}
}

// One sequence item after its dash, nested blocks starting on the dash line
func (w *yamlWriter) writeItem(v *yaml.Node, dash int) {
	v = resolveAlias(v)
	if v == nil {
		v = nullNode()
	}
	switch {
	case v.Kind == yaml.MappingNode && len(v.Content) > 0 && !w.style.flowStyled(v):
		w.write(" ")
		w.blockMapping(v, dash+2)
	case v.Kind == yaml.SequenceNode && len(v.Content) > 0 && !w.style.flowStyled(v):
		w.write(" ")
		w.blockSequence(v, dash+2)
	case v.Kind == yaml.MappingNode || v.Kind == yaml.SequenceNode:
		w.write(" " + w.flowForm(v))
		w.newline()
	default:
		w.writeScalar(v, dash+4, false, false, false)
		w.newline()
	}
}

// Single line flow form of a collection, items written under flow scalar rules
func (w *yamlWriter) flowForm(n *yaml.Node) string {
	n = resolveAlias(n)
	sub := newYAMLWriter(yamlStyle{width: math.MaxInt32, roundTrip: w.style.roundTrip, quoteWords: w.style.quoteWords})
	open := func(s string) {
		sub.write(s)
		sub.whitespace = true
	}
	switch n.Kind {
	case yaml.MappingNode:
		open("{")
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				open(", ")
			}
			sub.flowItem(n.Content[i], true)
			sub.write(":")
			sub.whitespace = false
			sub.flowItem(n.Content[i+1], false)
		}
		sub.write("}")
	case yaml.SequenceNode:
		open("[")
		for i, item := range n.Content {
			if i > 0 {
				open(", ")
			}
			sub.flowItem(item, false)
		}
		sub.write("]")
	default:
		sub.writeScalar(n, 0, false, true, false)
	}
	return sub.b.String()
}

// One key or value inside a flow collection, separated by a space unless the cursor follows an opener
func (w *yamlWriter) flowItem(n *yaml.Node, key bool) {
	n = resolveAlias(n)
	if n == nil {
		n = nullNode()
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		if !w.whitespace {
			w.write(" ")
		}
		w.write(w.flowForm(n))
		return
	}
	w.writeScalar(n, 0, key, true, false)
}

// Writes a scalar at the cursor under the style rules and reports whether it was written plain
func (w *yamlWriter) writeScalar(n *yaml.Node, indent int, key, flow, root bool) bool {
	tag, text := scalarKind(n)
	switch tag {
	case tagNull:
		if w.style.roundTrip && !key && !flow && !root {
			return true
		}
		w.plainWord("null")
		return true
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		w.plainWord(v)
		return true
	case tagInt:
		if v, ok := constructInt(text); ok {
			w.plainWord(v.String())
			return true
		}
	case tagFloat:
		if f, ok := constructFloat(text); ok {
			w.plainWord(reprFloat(f))
			return true
		}
	case tagStr, mergeTag:
		return w.str(text, indent, key, flow, root)
	case "!!timestamp":
		if got, _ := resolvePlain(text); got == "!!timestamp" {
			w.plainWord(text)
			return true
		}
	}
	w.plainWord(tagText(tag))
	return w.str(text, indent, key, flow, root)
}

// Tag as the emitter writes it before a value, the secondary handle kept short
func tagText(tag string) string {
	if strings.HasPrefix(tag, "tag:yaml.org,2002:") {
		return "!!" + strings.TrimPrefix(tag, "tag:yaml.org,2002:")
	}
	return tag
}

// Writes one token that needs no quoting, separated from the previous text by a space
func (w *yamlWriter) plainWord(s string) {
	if !w.whitespace {
		w.write(" ")
	}
	w.write(s)
}

// Writes a string in the plain, single quoted or double quoted form the emitter picks and reports a plain choice
func (w *yamlWriter) str(text string, indent int, key, flow, root bool) bool {
	if w.style.quoteWords && !key && ambiguousWords()[strings.ToLower(text)] {
		w.doubleQuoted(text, indent, !key)
		return false
	}
	a := analyzeScalar(text)
	implicit := false
	if text != "" {
		got, _ := resolvePlain(text)
		implicit = got == tagStr && text != "="
	}
	allowPlain := a.allowBlockPlain
	if flow {
		allowPlain = a.allowFlowPlain
	}
	if implicit && !(key && (a.empty || a.multiline)) && allowPlain {
		w.plainText(text, indent, !key, root)
		return true
	}
	if strings.ContainsAny(text, "'\n") {
		w.doubleQuoted(text, indent, !key)
		return false
	}
	if a.allowSingle && !(key && a.multiline) {
		w.singleQuoted(text, indent, !key)
		return false
	}
	w.doubleQuoted(text, indent, !key)
	return false
}

// Styles a string may take, from the emitter scalar analysis
type scalarAnalysis struct {
	empty, multiline                             bool
	allowFlowPlain, allowBlockPlain, allowSingle bool
}

// Reports a line break character
func isBreak(r rune) bool { return r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 }

// Reports whitespace that ends an indicator, a break or the end of text included
func isSpaceOrEnd(r rune) bool { return r == 0 || r == ' ' || r == '\t' || r == '\r' || isBreak(r) }

// Indicator, whitespace and character facts that decide which styles can carry a string
func analyzeScalar(s string) scalarAnalysis {
	if s == "" {
		return scalarAnalysis{empty: true, allowBlockPlain: true, allowSingle: true}
	}
	rs := []rune(s)
	var blockInd, flowInd, lineBreaks, special bool
	var leadSpace, leadBreak, trailSpace, trailBreak, breakSpace, spaceBreak bool
	if strings.HasPrefix(s, "---") || strings.HasPrefix(s, "...") {
		blockInd, flowInd = true, true
	}
	preceded := true
	followed := len(rs) == 1 || isSpaceOrEnd(rs[1])
	prevSpace, prevBreak := false, false
	for i, ch := range rs {
		if i == 0 {
			if strings.ContainsRune("#,[]{}&*!|>'\"%@`", ch) {
				flowInd, blockInd = true, true
			}
			if ch == '?' || ch == ':' {
				flowInd = flowInd || len(rs) == 1
				blockInd = blockInd || followed
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
		if isBreak(ch) {
			lineBreaks = true
		}
		if ch != '\n' && (ch < 0x20 || ch > 0x7e) {
			printable := ch == 0x85 || (ch >= 0xa0 && ch <= 0xd7ff) || (ch >= 0xe000 && ch <= 0xfffd) || (ch >= 0x10000 && ch <= 0x10ffff)
			if !printable || ch == 0xfeff {
				special = true
			}
		}
		switch {
		case ch == ' ':
			leadSpace = leadSpace || i == 0
			trailSpace = trailSpace || i == len(rs)-1
			breakSpace = breakSpace || prevBreak
			prevSpace, prevBreak = true, false
		case isBreak(ch):
			leadBreak = leadBreak || i == 0
			trailBreak = trailBreak || i == len(rs)-1
			spaceBreak = spaceBreak || prevSpace
			prevSpace, prevBreak = false, true
		default:
			prevSpace, prevBreak = false, false
		}
		preceded = isSpaceOrEnd(ch)
		followed = i+2 >= len(rs) || isSpaceOrEnd(rs[i+2])
	}
	a := scalarAnalysis{multiline: lineBreaks, allowFlowPlain: true, allowBlockPlain: true, allowSingle: true}
	if leadSpace || leadBreak || trailSpace || trailBreak {
		a.allowFlowPlain, a.allowBlockPlain = false, false
	}
	if breakSpace {
		a.allowFlowPlain, a.allowBlockPlain, a.allowSingle = false, false, false
	}
	if spaceBreak || special {
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

// Starts a continuation line at the scalar indent
func (w *yamlWriter) breakLine(indent int) {
	w.newline()
	w.write(strings.Repeat(" ", indent))
	w.whitespace = false
}

// Plain scalar folded at a single space from the width on, a word past the width moved down
func (w *yamlWriter) plainText(text string, indent int, split, root bool) {
	if text == "" {
		return
	}
	if !w.whitespace {
		w.write(" ")
	}
	words := strings.Split(text, " ")
	for i, word := range words {
		if i > 0 {
			if word != "" && words[i-1] != "" && w.col >= w.style.width && split {
				w.breakLine(indent)
			} else {
				w.write(" ")
			}
		}
		if word != "" && !root && utf8.RuneCountInString(word)+w.col > w.style.width && w.col > indent {
			w.breakLine(indent)
		}
		w.write(word)
	}
	w.whitespace = false
}

// Single quoted scalar with quotes doubled, line breaks written as blank lines and long lines folded
func (w *yamlWriter) singleQuoted(text string, indent int, split bool) {
	if !w.whitespace {
		w.write(" ")
	}
	w.write("'")
	rs := []rune(text)
	spaces, breaks := false, false
	start := 0
	for end := 0; end <= len(rs); end++ {
		var ch rune
		if end < len(rs) {
			ch = rs[end]
		}
		atEnd := end == len(rs)
		switch {
		case spaces:
			if atEnd || ch != ' ' {
				if start+1 == end && w.col > w.style.width && split && start != 0 && end != len(rs) {
					w.breakLine(indent)
				} else {
					w.write(string(rs[start:end]))
				}
				start = end
			}
		case breaks:
			if atEnd || !isBreak(ch) {
				if rs[start] == '\n' {
					w.newline()
				}
				for _, br := range rs[start:end] {
					if br == '\n' {
						w.newline()
						continue
					}
					w.b.WriteRune(br)
					w.col, w.whitespace = 0, true
				}
				w.write(strings.Repeat(" ", indent))
				start = end
			}
		default:
			if atEnd || ch == ' ' || isBreak(ch) || ch == '\'' {
				if start < end {
					w.write(string(rs[start:end]))
					start = end
				}
			}
		}
		if !atEnd && ch == '\'' {
			w.write("''")
			start = end + 1
		}
		if !atEnd {
			spaces, breaks = ch == ' ', isBreak(ch)
		}
	}
	w.write("'")
	w.whitespace = false
}

// Reports a character the double quoted writer always escapes, printable or not
func isDoubleEscaped(ch rune) bool {
	return ch == '"' || ch == '\\' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || ch == 0xfeff
}

// Escape letters the double quoted writer uses in place of a hexadecimal escape
func escapeLetters() map[rune]string {
	return map[rune]string{
		0: "0", 0x07: "a", 0x08: "b", 0x09: "t", 0x0a: "n", 0x0b: "v", 0x0c: "f", 0x0d: "r", 0x1b: "e",
		'"': "\"", '\\': "\\", 0x85: "N", 0xa0: "_", 0x2028: "L", 0x2029: "P",
	}
}

// Double quoted scalar with every unprintable character escaped and long lines folded with a backslash
func (w *yamlWriter) doubleQuoted(text string, indent int, split bool) {
	if !w.whitespace {
		w.write(" ")
	}
	w.write("\"")
	letters := escapeLetters()
	rs := []rune(text)
	start := 0
	for end := 0; end <= len(rs); end++ {
		var ch rune
		atEnd := end == len(rs)
		if !atEnd {
			ch = rs[end]
		}
		printable := (ch >= 0x20 && ch <= 0x7e) || (ch >= 0xa0 && ch <= 0xd7ff) || (ch >= 0xe000 && ch <= 0xfffd) || (ch >= 0x10000 && ch <= 0x10ffff)
		if atEnd || isDoubleEscaped(ch) || !printable {
			if start < end {
				w.write(string(rs[start:end]))
				start = end
			}
			if !atEnd {
				switch esc, ok := letters[ch]; {
				case ok:
					w.write("\\" + esc)
				case ch <= 0xff:
					w.write(fmt.Sprintf("\\x%02X", ch))
				case ch <= 0xffff:
					w.write(fmt.Sprintf("\\u%04X", ch))
				default:
					w.write(fmt.Sprintf("\\U%08X", ch))
				}
				start = end + 1
			}
		}
		if end > 0 && end < len(rs)-1 && (ch == ' ' || start >= end) && w.col+(end-start) > w.style.width && split {
			escaped := foldNeedsEscape(rs, start, end)
			data := ""
			if start < end {
				data = string(rs[start:end])
				start = end
			}
			if escaped {
				data += "\\"
			}
			w.write(data)
			w.breakLine(indent)
			if rs[start] == ' ' {
				if escaped {
					w.write("\\")
				} else {
					start++
				}
			}
		}
	}
	w.write("\"")
	w.whitespace = false
}

// Reports a double quoted fold needing a backslash, a bare fold kept where its break reads back as a space
func foldNeedsEscape(rs []rune, start, end int) bool {
	if start > end && rs[start] != ' ' {
		return true
	}
	sp := -1
	for i := end; i < len(rs); i++ {
		if rs[i] == ' ' {
			sp = i
			break
		}
	}
	if sp < 0 {
		return true
	}
	for i := end; i < sp; i++ {
		if rs[i] == '\n' {
			sp = i
			break
		}
	}
	if sp+1 >= len(rs) || (rs[sp] == '\n' && rs[sp+1] != ' ') {
		return true
	}
	return strings.ContainsAny(string(rs[end:sp]), "\"'") || rs[sp+1] == ' ' || rs[sp+1] == '\n' || string(rs[end-1:end+1]) == "  " || start == end
}

// Shortest decimal text reading back as f, in exponent form below 1e-4 and from 1e16 as dumpers write floats
func reprFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return ".nan"
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(e, "-") {
		sign, e = "-", e[1:]
	}
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -4 && exp < 16 {
		if exp < 0 {
			return sign + "0." + strings.Repeat("0", -exp-1) + digits
		}
		if len(digits) <= exp+1 {
			return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
		}
		return sign + digits[:exp+1] + "." + digits[exp+1:]
	}
	out := sign + digits[:1]
	if len(digits) > 1 {
		out += "." + digits[1:]
	}
	expSign := "+"
	if exp < 0 {
		expSign, exp = "-", -exp
	}
	return fmt.Sprintf("%se%s%02d", out, expSign, exp)
}

// YAML 1.1 tag and text of a value tree scalar, its tag trusted when set and its text resolved otherwise
func scalarKind(n *yaml.Node) (tag, text string) {
	n = resolveAlias(n)
	if n == nil {
		return tagNull, ""
	}
	if n.Tag == "" || n.Tag == "!" {
		return scalarType(n)
	}
	tag = n.ShortTag()
	if n.Style&yaml.TaggedStyle != 0 {
		text, _ = resolveTagged(tag, n.Value)
		return tag, text
	}
	return tag, n.Value
}

// Number a numeric scalar holds, exact for integers, ok false for anything else or not a number
func numericValue(tag, text string) (*big.Float, bool) {
	switch tag {
	case tagInt:
		v, ok := constructInt(text)
		if !ok {
			return nil, false
		}
		return new(big.Float).SetInt(v), true
	case tagFloat:
		f, ok := constructFloat(text)
		if !ok || math.IsNaN(f) {
			return nil, false
		}
		if math.IsInf(f, 0) {
			return new(big.Float).SetInf(f < 0), true
		}
		return new(big.Float).SetFloat64(f), true
	}
	return nil, false
}

// Reports equal values the way the loaded data compares, mappings by key, lists by position, numbers by value
func valueEqual(a, b *yaml.Node, strictBool bool) bool {
	a, b = resolveAlias(a), resolveAlias(b)
	if a == nil || b == nil {
		return isNull(a) && isNull(b)
	}
	switch {
	case a.Kind == yaml.MappingNode && b.Kind == yaml.MappingNode:
		ap, bp := mappingPairs(a), mappingPairs(b)
		if len(ap) != len(bp) {
			return false
		}
		for _, p := range ap {
			q, ok := pairNamed(bp, p.key.Value)
			if !ok || !valueEqual(p.value, q.value, strictBool) {
				return false
			}
		}
		return true
	case a.Kind == yaml.SequenceNode && b.Kind == yaml.SequenceNode:
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !valueEqual(a.Content[i], b.Content[i], strictBool) {
				return false
			}
		}
		return true
	case a.Kind == yaml.ScalarNode && b.Kind == yaml.ScalarNode:
		return scalarEqual(a, b, strictBool)
	}
	return false
}

// Pair under key in a pair list
func pairNamed(pairs []pair, key string) (pair, bool) {
	for _, p := range pairs {
		if p.key.Value == key {
			return p, true
		}
	}
	return pair{}, false
}

// Reports equal scalars, booleans counting as one and zero unless strict
func scalarEqual(a, b *yaml.Node, strictBool bool) bool {
	ta, xa := scalarKind(a)
	tb, xb := scalarKind(b)
	if ta == tagBool {
		ta, xa = boolAsNumber(xa, strictBool)
	}
	if tb == tagBool {
		tb, xb = boolAsNumber(xb, strictBool)
	}
	if na, ok := numericValue(ta, xa); ok {
		nb, ok := numericValue(tb, xb)
		return ok && na.Cmp(nb) == 0
	}
	if ta == tagBool || tb == tagBool {
		return ta == tb && xa == xb
	}
	if ta == tagNull || tb == tagNull {
		return ta == tb
	}
	return ta == tb && xa == xb
}

// Boolean text normalized, read as the integer one or zero unless strict keeps it a boolean
func boolAsNumber(text string, strict bool) (string, string) {
	v, _ := resolveTagged(tagBool, text)
	if strict {
		return tagBool, v
	}
	if v == "true" {
		return tagInt, "1"
	}
	return tagInt, "0"
}
