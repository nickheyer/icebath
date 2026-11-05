package config

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	yaml11Null      = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	yaml11Merge     = regexp.MustCompile(`^<<$`)
	yaml11Value     = regexp.MustCompile(`^=$`)
	yaml11Timestamp = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
)

const safeDumpWidth = 80

var yamlEscapes = map[rune]string{
	0: "0", 0x07: "a", 0x08: "b", 0x09: "t", 0x0A: "n", 0x0B: "v", 0x0C: "f", 0x0D: "r", 0x1B: "e",
	'"': "\"", '\\': "\\", 0x85: "N", 0xA0: "_", 0x2028: "L", 0x2029: "P",
}

type safeIndent struct {
	set, seq bool
	n        int
}

type scalarAnalysis struct {
	empty, multiline                          bool
	allowBlockPlain, allowSingle, allowDouble bool
}

type safeEmitter struct {
	out                   strings.Builder
	column, indent        int
	indentSet             bool
	indents               []safeIndent
	whitespace, indention bool
	openEnded, rootCtx    bool
	simpleKey             bool
}

// YAML text of data as hermes_yaml safe_dump writes it with key order kept, block style and width 80
func SafeDump(data any) ([]byte, error) {
	e := &safeEmitter{whitespace: true, indention: true}
	if err := e.node(data, true, false, false); err != nil {
		return nil, err
	}
	e.writeIndent()
	if e.openEnded {
		e.writeIndicator("...", true, false, false)
		e.writeIndent()
	}
	return []byte(e.out.String()), nil
}

var safeDumpImpl = SafeDump

// Replaces the dumper behind AtomicYAMLWrite and returns the restore func, for tests simulating a failed dump
func SwapSafeDump(f func(any) ([]byte, error)) func() {
	old := safeDumpImpl
	safeDumpImpl = f
	return func() { safeDumpImpl = old }
}

// Writes data as YAML plus extra through a synced temp file, keeping the existing mode and owner
func AtomicYAMLWrite(path string, data any, extra string) error {
	text, err := safeDumpImpl(data)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return atomicPreservingWrite(path, append(text, extra...), "."+stem+"_")
}

func (e *safeEmitter) lastSeq() bool {
	if len(e.indents) < 2 {
		return false
	}
	return e.indents[len(e.indents)-2].seq
}

func (e *safeEmitter) increaseIndent(flow, seq, indentless bool) {
	e.indents = append(e.indents, safeIndent{set: e.indentSet, seq: seq, n: e.indent})
	if !e.indentSet {
		e.indent, e.indentSet = 0, !flow
		return
	}
	if !indentless {
		if e.lastSeq() {
			e.indent += 4
		} else {
			e.indent += 2
		}
	}
}

func (e *safeEmitter) popIndent() {
	top := e.indents[len(e.indents)-1]
	e.indents = e.indents[:len(e.indents)-1]
	e.indent, e.indentSet = top.n, top.set
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
	if !e.indention || e.column > e.indent || (e.column == e.indent && !e.whitespace) {
		e.writeLineBreak("")
	}
	if e.column < e.indent {
		e.whitespace = true
		e.out.WriteString(strings.Repeat(" ", e.indent-e.column))
		e.column = e.indent
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

// Emits one node in the ruamel block layout with mapping indent 2, sequence indent 4 and dash offset 2
func (e *safeEmitter) node(v any, root, mapping, simpleKey bool) error {
	e.rootCtx, e.simpleKey = root, simpleKey
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return e.emptyFlow("{", "}")
		}
		e.increaseIndent(false, false, false)
		for _, k := range MappingKeys(x) {
			if err := e.mappingItem(k, x[k]); err != nil {
				return err
			}
		}
		e.popIndent()
		return nil
	case Tree:
		return e.node(map[string]any(x), root, mapping, simpleKey)
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		return e.node(m, root, mapping, simpleKey)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return e.node(items, root, mapping, simpleKey)
	case []any:
		if len(x) == 0 {
			return e.emptyFlow("[", "]")
		}
		e.increaseIndent(false, true, mapping && !e.indention)
		for _, item := range x {
			e.writeIndent()
			e.writeIndicator("  -", true, false, true)
			if err := e.node(item, false, false, false); err != nil {
				return err
			}
		}
		e.popIndent()
		return nil
	}
	text, isStr, err := representScalar(v)
	if err != nil {
		return err
	}
	e.increaseIndent(true, false, false)
	e.scalar(text, isStr)
	e.popIndent()
	return nil
}

func (e *safeEmitter) emptyFlow(open, close string) error {
	e.writeIndicator(open, true, true, false)
	e.increaseIndent(true, false, false)
	e.popIndent()
	e.writeIndicator(close, false, false, false)
	return nil
}

// Writes one key and value, a key of 123 runes or more or with a line break takes the complex form
func (e *safeEmitter) mappingItem(k string, v any) error {
	e.writeIndent()
	a := analyzeScalar([]rune(k))
	if len([]rune(k))+5 < 128 && !a.multiline {
		if err := e.node(k, false, true, true); err != nil {
			return err
		}
		e.writeIndicator(":", false, false, false)
		return e.node(v, false, true, false)
	}
	e.writeIndicator("?", true, false, true)
	if err := e.node(k, false, true, false); err != nil {
		return err
	}
	e.writeIndent()
	e.writeIndicator(":", true, false, true)
	return e.node(v, false, true, false)
}

// Text of a scalar and whether it carries the str tag, as the YAML 1.1 safe representer writes it
func representScalar(v any) (string, bool, error) {
	switch x := v.(type) {
	case nil:
		return "null", false, nil
	case string:
		return x, true, nil
	case bool:
		if x {
			return "true", false, nil
		}
		return "false", false, nil
	case float32:
		return representFloat(float64(x)), false, nil
	case float64:
		return representFloat(x), false, nil
	}
	if n, ok := pyrepr.Int(v); ok {
		return n.String(), false, nil
	}
	return "", false, fmt.Errorf("cannot represent an object: %s", pyrepr.Repr(v))
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

// Whether the plain text of a str resolves back to str under the YAML 1.1 implicit resolvers
func plainResolvesToStr(s string) bool {
	for _, re := range []*regexp.Regexp{yaml11Bool, yaml11Int, yaml11Float, yaml11Null, yaml11Merge, yaml11Timestamp, yaml11Value} {
		if re.MatchString(s) {
			return false
		}
	}
	return true
}

func isBreakRune(r rune) bool { return r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029 }

func isBlankRune(r rune) bool { return r == 0 || r == ' ' || r == '\t' || r == '\r' || isBreakRune(r) }

func printableUnicode(r rune) bool {
	return r == 0x85 || (r >= 0xA0 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

// Styles a scalar may take in block context, the ruamel analyze_scalar rules with allow_unicode on
func analyzeScalar(s []rune) scalarAnalysis {
	if len(s) == 0 {
		return scalarAnalysis{empty: true, allowBlockPlain: true, allowSingle: true, allowDouble: true}
	}
	text := string(s)
	blockInd := strings.HasPrefix(text, "---") || strings.HasPrefix(text, "...")
	var lineBreaks, special, leadSpace, leadBreak, trailSpace, trailBreak, breakSpace, spaceBreak, prevSpace, prevBreak bool
	preceded := true
	followed := len(s) == 1 || isBlankRune(s[1])
	for i, ch := range s {
		if i == 0 {
			if strings.ContainsRune("#,[]{}&*!|>'\"%@`", ch) {
				blockInd = true
			}
			if (ch == '?' || ch == ':') && followed {
				blockInd = true
			}
			if ch == '-' && followed {
				blockInd = true
			}
		} else {
			if ch == ':' && followed {
				blockInd = true
			}
			if ch == '#' && preceded {
				blockInd = true
			}
		}
		if isBreakRune(ch) {
			lineBreaks = true
		}
		if !(ch == '\n' || (ch >= 0x20 && ch <= 0x7E)) && !(printableUnicode(ch) && ch != 0xFEFF) {
			special = true
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
	a := scalarAnalysis{multiline: lineBreaks, allowBlockPlain: true, allowSingle: true, allowDouble: true}
	if leadSpace || leadBreak || trailSpace || trailBreak {
		a.allowBlockPlain = false
	}
	if breakSpace || special || spaceBreak {
		a.allowBlockPlain, a.allowSingle = false, false
	}
	if lineBreaks || blockInd {
		a.allowBlockPlain = false
	}
	return a
}

// Picks plain, single or double quoting the way the ruamel emitter does and writes the scalar
func (e *safeEmitter) scalar(text string, isStr bool) {
	s := []rune(text)
	a := analyzeScalar(s)
	split := !e.simpleKey
	implicit := !isStr || plainResolvesToStr(text)
	switch {
	case implicit && !(e.simpleKey && (a.empty || a.multiline)) && a.allowBlockPlain:
		e.writePlain(s, split)
	case a.allowDouble && (strings.ContainsRune(text, '\'') || strings.ContainsRune(text, '\n')):
		e.writeDouble(s, split)
	case a.allowSingle && !(e.simpleKey && a.multiline):
		e.writeSingle(s, split)
	default:
		e.writeDouble(s, split)
	}
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
	spaces, start := false, 0
	for end := 0; end <= len(s); end++ {
		var ch rune = -1
		if end < len(s) {
			ch = s[end]
		}
		if spaces {
			if ch != ' ' {
				if start+1 == end && e.column > safeDumpWidth && split {
					e.writeIndent()
					e.whitespace, e.indention = false, false
				} else {
					e.write(string(s[start:end]))
				}
				start = end
			}
		} else if ch == -1 || ch == ' ' || isBreakRune(ch) {
			data := s[start:end]
			if len(data) > safeDumpWidth && e.indentSet && e.column > e.indent {
				e.writeIndent()
			}
			e.write(string(data))
			start = end
		}
		if ch != -1 {
			spaces = ch == ' '
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
				if start+1 == end && e.column > safeDumpWidth && split && start != 0 && end != len(s) {
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

func indexRune(s []rune, r rune, from int) int {
	for i := from; i < len(s); i++ {
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
		if ch == -1 || ch == '"' || ch == '\\' || ch == 0x85 || ch == 0x2028 || ch == 0x2029 || ch == 0xFEFF || !((ch >= 0x20 && ch <= 0x7E) || printableUnicode(ch)) {
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
		if end > 0 && end < len(s)-1 && (ch == ' ' || start >= end) && e.column+(end-start) > safeDumpWidth && split {
			needBackslash := true
			if sp := indexRune(s, ' ', end); sp >= 0 && sp+1 < len(s) {
				seg := string(s[end:sp])
				if !strings.ContainsAny(seg, "\"'") && s[sp+1] != ' ' && string(s[end-1:end+1]) != "  " {
					needBackslash = false
				}
			}
			data := string(s[start:end])
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
