package pyast

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
	"golang.org/x/text/unicode/norm"
)

func nfkc(s string) string { return norm.NFKC.String(s) }

const maxStrDigits = 4300

// Value of a NUMBER token as parsenumber, with the huge decimal literal error
func (p *parser) numberValue(t *token) any {
	s := strings.ReplaceAll(t.val, "_", "")
	last := s[len(s)-1]
	if last == 'j' || last == 'J' {
		f, _ := strconv.ParseFloat(s[:len(s)-1], 64)
		return Complex{Imag: f}
	}
	if len(s) > 1 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			v, _ := new(big.Int).SetString(s[2:], base)
			return v
		}
	}
	if strings.ContainsAny(s, ".eE") {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	if len(s) > maxStrDigits {
		p.raiseKnown("SyntaxError", t.l, 0, t.el, 0, fmt.Sprintf("Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit - Consider hexadecimal for huge integer literals to avoid decimal conversion limits.", maxStrDigits, len(s)))
	}
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

type strPart struct {
	node *Node
	kind byte
}

// Implicitly concatenated string literals as _PyPegen_concatenate_strings and its t-string twin
func (p *parser) strings() *Node {
	start := p.cur()
	var parts []strPart
	for {
		t := p.cur()
		switch t.kind {
		case tString:
			p.next()
			n, isBytes := p.stringConstant(t)
			k := byte('s')
			if isBytes {
				k = 'b'
			}
			parts = append(parts, strPart{n, k})
			continue
		case tFStart:
			parts = append(parts, strPart{p.fstring(), 'f'})
			continue
		case tTStart:
			parts = append(parts, strPart{p.fstring(), 't'})
			continue
		}
		break
	}
	tCount := 0
	for _, s := range parts {
		if s.kind == 't' {
			tCount++
		}
	}
	if tCount > 0 && tCount < len(parts) {
		first := parts[0].kind == 't'
		for _, s := range parts {
			if (s.kind == 't') != first {
				p.raiseRange(parts[0].node, s.node, "cannot mix t-string literals with string or bytes literals")
			}
		}
	}
	if tCount > 0 {
		var nodes []*Node
		for _, s := range parts {
			nodes = append(nodes, s.node)
		}
		return p.mkFrom(start, "TemplateStr", foldParts(nodes, true))
	}
	hasBytes, hasOther, hasF := false, false, false
	for _, s := range parts {
		switch s.kind {
		case 'b':
			hasBytes = true
		case 'f':
			hasF, hasOther = true, true
		default:
			hasOther = true
		}
	}
	if hasBytes && hasOther {
		p.raiseLast("cannot mix bytes and nonbytes literals")
	}
	if hasBytes {
		var buf []byte
		for _, s := range parts {
			buf = append(buf, s.node.Get("value").([]byte)...)
		}
		return p.mkFrom(start, "Constant", buf, parts[0].node.Get("kind"))
	}
	if !hasF && len(parts) == 1 {
		return parts[0].node
	}
	var nodes []*Node
	for _, s := range parts {
		nodes = append(nodes, s.node)
	}
	folded := foldParts(nodes, hasF)
	if !hasF {
		return folded[0]
	}
	return p.mkFrom(start, "JoinedStr", folded)
}

// Flattens joined strings and merges runs of constants, dropping empty strings inside formatted literals
func foldParts(nodes []*Node, fstr bool) []*Node {
	var flat []*Node
	for _, n := range nodes {
		if n.Is("JoinedStr", "TemplateStr") {
			flat = append(flat, n.Children("values")...)
		} else {
			flat = append(flat, n)
		}
	}
	var out []*Node
	for i := 0; i < len(flat); i++ {
		e := flat[i]
		if e.Is("Constant") {
			if i+1 < len(flat) && flat[i+1].Is("Constant") {
				first := e
				var b strings.Builder
				last := e
				j := i
				for ; j < len(flat) && flat[j].Is("Constant"); j++ {
					b.WriteString(flat[j].Get("value").(string))
					last = flat[j]
				}
				i = j - 1
				e = mkAt("Constant", first.Lineno, first.ColOffset, last.EndLineno, last.EndColOffset, b.String(), first.Get("kind"))
			}
			if s, ok := e.Get("value").(string); fstr && ok && s == "" {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// Decodes a STRING token as _PyPegen_constant_from_string
func (p *parser) stringConstant(t *token) (*Node, bool) {
	s := t.val
	i := 0
	bytesMode, raw := false, false
	for ; i < len(s); i++ {
		c := s[i] | 0x20
		if c == 'b' {
			bytesMode = true
		} else if c == 'r' {
			raw = true
		} else if c != 'u' {
			break
		}
	}
	var kind any
	if s[0] == 'u' {
		kind = "u"
	}
	quote := s[i]
	body := s[i+1 : len(s)-1]
	if len(body) >= 4 && body[0] == quote && body[1] == quote {
		body = body[2 : len(body)-2]
	}
	raw = raw || !strings.Contains(body, "\\")
	if bytesMode {
		for j := 0; j < len(body); j++ {
			if body[j] >= 0x80 {
				p.raiseTok(t, "bytes can only contain ASCII literal characters")
			}
		}
		if raw {
			return p.mkTok(t, "Constant", []byte(body), kind), true
		}
		b, err := decodeBytesEscapes(body)
		if err != "" {
			p.raiseLast("(value error) " + err)
		}
		return p.mkTok(t, "Constant", b, kind), true
	}
	if raw {
		return p.mkTok(t, "Constant", body, kind), false
	}
	v, err := decodeUnicodeEscapes(body)
	if err != "" {
		p.raiseLast("(unicode error) " + err)
	}
	return p.mkTok(t, "Constant", v, kind), false
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 99
}

// Bytes literal escapes as _PyBytes_DecodeEscape with the invalid \x ValueError text
func decodeBytesEscapes(s string) ([]byte, string) {
	var out []byte
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		i++
		if i >= len(s) {
			return nil, "Trailing \\ in string"
		}
		e := s[i]
		i++
		switch e {
		case '\n':
		case '\\', '\'', '"':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 'v':
			out = append(out, '\v')
		case 'a':
			out = append(out, '\a')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := int(e - '0')
			for k := 0; k < 2 && i < len(s) && s[i] >= '0' && s[i] <= '7'; k++ {
				v = v<<3 + int(s[i]-'0')
				i++
			}
			out = append(out, byte(v))
		case 'x':
			if i+1 < len(s) && hexVal(s[i]) < 16 && hexVal(s[i+1]) < 16 {
				out = append(out, byte(hexVal(s[i])<<4+hexVal(s[i+1])))
				i += 2
				continue
			}
			return nil, fmt.Sprintf("invalid \\x escape at position %d", i-2)
		default:
			out = append(out, '\\')
			i--
		}
	}
	return out, ""
}

// Rewrites non ASCII text as \U escapes like decode_unicode_with_escapes before escape decoding
func escapeBuffer(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' {
			b.WriteByte('\\')
			i++
			if i >= len(s) || s[i] >= 0x80 {
				b.WriteString("u005c")
				if i >= len(s) {
					break
				}
			}
		}
		if s[i] >= 0x80 {
			r, size := utf8.DecodeRuneInString(s[i:])
			fmt.Fprintf(&b, "\\U%08x", r)
			i += size
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func unicodeEscapeError(start, end int, msg string, buf string) string {
	if end-start == 1 {
		return fmt.Sprintf("'unicodeescape' codec can't decode byte 0x%02x in position %d: %s", buf[start], start, msg)
	}
	return fmt.Sprintf("'unicodeescape' codec can't decode bytes in position %d-%d: %s", start, end-1, msg)
}

// Decodes str literal escapes as the unicode_escape codec, returning its error text on failure
func decodeUnicodeEscapes(src string) (string, string) {
	s := escapeBuffer(src)
	var out []byte
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		start := i
		i++
		if i >= len(s) {
			return "", unicodeEscapeError(start, len(s), "\\ at end of string", s)
		}
		e := s[i]
		i++
		switch e {
		case '\n':
		case '\\', '\'', '"':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 'v':
			out = append(out, '\v')
		case 'a':
			out = append(out, '\a')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := rune(e - '0')
			for k := 0; k < 2 && i < len(s) && s[i] >= '0' && s[i] <= '7'; k++ {
				v = v<<3 + rune(s[i]-'0')
				i++
			}
			out = pystr.AppendRune(out, v)
		case 'x', 'u', 'U':
			count, msg := 2, "truncated \\xXX escape"
			if e == 'u' {
				count, msg = 4, "truncated \\uXXXX escape"
			} else if e == 'U' {
				count, msg = 8, "truncated \\UXXXXXXXX escape"
			}
			var ch rune
			for ; count > 0; count-- {
				if i >= len(s) || hexVal(s[i]) >= 16 {
					return "", unicodeEscapeError(start, i, msg, s)
				}
				ch = ch<<4 + rune(hexVal(s[i]))
				i++
			}
			if ch > 0x10ffff || ch < 0 {
				return "", unicodeEscapeError(start, i, "illegal Unicode character", s)
			}
			out = pystr.AppendRune(out, ch)
		case 'N':
			msg := "malformed \\N character escape"
			if i >= len(s) {
				return "", unicodeEscapeError(start, i, msg, s)
			}
			if s[i] == '{' {
				nameStart := i + 1
				j := nameStart
				for j < len(s) && s[j] != '}' {
					j++
				}
				if j >= len(s) {
					return "", unicodeEscapeError(start, j, msg, s)
				}
				if j > nameStart {
					i = j + 1
					if r, ok := pyunicode.Lookup(s[nameStart:j]); ok {
						out = pystr.AppendRune(out, r)
						continue
					}
					return "", unicodeEscapeError(start, i, "unknown Unicode character name", s)
				}
				i = j
			}
			return "", unicodeEscapeError(start, i, msg, s)
		default:
			out = append(out, '\\', e)
		}
	}
	return string(out), ""
}

// Parses an f-string or t-string from its start token into a JoinedStr or TemplateStr
func (p *parser) fstring() *Node {
	start := p.next()
	tstr := start.kind == tTStart
	raw := strings.ContainsAny(start.val, "rR")
	var items []*Node
	for {
		t := p.cur()
		if t.kind == tFEnd || t.kind == tTEnd {
			p.next()
			break
		}
		if t.kind == tFMiddle || t.kind == tTMiddle {
			p.next()
			items = append(items, p.mkTok(t, "Constant", t.val, nil))
			continue
		}
		if t.kind == tOp && t.val == "{" {
			items = append(items, p.replacementField(tstr))
			continue
		}
		p.fail()
	}
	var vals []*Node
	for _, it := range items {
		if it.Is("JoinedStr") {
			vals = append(vals, it.Children("values")...)
			continue
		}
		if it.Is("Constant") {
			text := it.Get("value").(string)
			if text == "{{" || text == "}}" {
				text = text[:1]
			}
			if !raw && strings.Contains(text, "\\") {
				v, err := decodeUnicodeEscapes(text)
				if err != "" {
					p.raiseLast("(unicode error) " + err)
				}
				text = v
			}
			if text == "" {
				continue
			}
			it = mkAt("Constant", it.Lineno, it.ColOffset, it.EndLineno, it.EndColOffset, text, nil)
		}
		vals = append(vals, it)
	}
	typ := "JoinedStr"
	if tstr {
		typ = "TemplateStr"
	}
	return p.mkFrom(start, typ, vals)
}

func (p *parser) strPrefix(tstr bool) string {
	if tstr {
		return "t-string: "
	}
	return "f-string: "
}

// Expression source of a replacement field with comments removed, as the tokenizer metadata
func (p *parser) fieldText(open, end *token) string {
	src := p.lx.src[open.eo:end.so]
	hash, inStr := false, false
	var q byte
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if ch == '\\' {
			i++
			continue
		}
		if ch == '"' || ch == '\'' {
			if !inStr {
				inStr, q = true, ch
			} else if ch == q {
				inStr = false
			}
			continue
		}
		if ch == '#' && !inStr {
			hash = true
			break
		}
	}
	if !hash {
		return string(src)
	}
	var out []byte
	inStr = false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if ch == '"' || ch == '\'' {
			if !inStr {
				inStr, q = true, ch
			} else if ch == q {
				inStr = false
			}
			out = append(out, ch)
		} else if ch == '#' && !inStr {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		} else {
			out = append(out, ch)
		}
	}
	return string(out)
}

// One replacement field with its conversion, format spec and self documenting text
func (p *parser) replacementField(tstr bool) *Node {
	open := p.next()
	pre := p.strPrefix(tstr)
	if p.isOp("=", "!", ":", "}") {
		t := p.cur()
		p.raiseTok(t, pre+"valid expression required before '"+t.val+"'")
	}
	if !p.isKw("yield") && !p.startsStarExpression() {
		p.raiseNextToken(pre + "expecting a valid expression after '{'")
	}
	expr := p.annotatedRHS()
	var debug *token
	if p.isOp("=") {
		debug = p.next()
	}
	if !p.isOp("!", ":", "}") {
		if debug == nil && !p.isOp("=") {
			p.raiseNextToken(pre + "expecting '=', or '!', or ':', or '}'")
		}
		if debug != nil {
			p.raiseNextToken(pre + "expecting '!', or ':', or '}'")
		}
	}
	conversion := -1
	var convTok, convName, specTok *token
	if p.isOp("!") {
		convTok = p.next()
		if p.isOp(":", "}") {
			p.raiseNextToken(pre + "missing conversion character")
		}
		if !p.isName() {
			p.raiseNextToken(pre + "invalid conversion character")
		}
		convName = p.next()
		if convTok.el != convName.l || convTok.ec != convName.c {
			p.raiseAt(convTok.l, convTok.c, convName.el, convName.ec, pre+"conversion type must come right after the exclamation mark")
		}
		name := p.identifier(convName.val)
		if pystr.Len(name) > 1 || !(name == "s" || name == "r" || name == "a") {
			p.raiseTok(convName, fmt.Sprintf("%sinvalid conversion character %s: expected 's', 'r', or 'a'", pre, reprIdent(name)))
		}
		conversion = int(name[0])
		if !p.isOp(":", "}") {
			p.raiseNextToken(pre + "expecting ':' or '}'")
		}
	}
	var spec *Node
	if p.isOp(":") {
		specTok = p.next()
		var items []*Node
		for {
			t := p.cur()
			if t.kind == tFMiddle || t.kind == tTMiddle {
				p.next()
				v, err := decodeUnicodeEscapes(t.val)
				if !strings.Contains(t.val, "\\") {
					v, err = t.val, ""
				}
				if err != "" {
					p.raiseLast("(unicode error) " + err)
				}
				items = append(items, p.mkTok(t, "Constant", v, nil))
				continue
			}
			if t.kind == tOp && t.val == "{" {
				items = append(items, p.replacementField(tstr))
				continue
			}
			break
		}
		if !p.isOp("}") {
			p.raiseNextToken(pre + "expecting '}', or format specs")
		}
		var kept []*Node
		for _, it := range items {
			if s, ok := it.Get("value").(string); it.Is("Constant") && ok && s == "" {
				continue
			}
			kept = append(kept, it)
		}
		if len(kept) == 0 || (len(kept) == 1 && kept[0].Is("Constant")) {
			spec = p.mkFrom(specTok, "JoinedStr", kept)
		} else {
			spec = p.mkFrom(specTok, "JoinedStr", foldParts(kept, true))
		}
	}
	if !p.isOp("}") {
		p.raiseNextToken(pre + "expecting '}'")
	}
	closeTok := p.next()
	if debug != nil && spec == nil && convTok == nil {
		conversion = 'r'
	}
	l, c := open.l, open.c
	el, ec := p.prevEnd()
	endTok := closeTok
	debugEndLine, debugEndOff := el, ec
	if convName != nil {
		endTok = convTok
		debugEndLine, debugEndOff = convName.l, convName.c
	} else if spec != nil {
		endTok = specTok
		debugEndLine, debugEndOff = spec.Lineno, spec.ColOffset+1
	}
	text := p.fieldText(open, endTok)
	var value *Node
	if tstr {
		value = mkAt("Interpolation", l, c, el, ec, expr, stripInterpolation(text), conversion, spec)
	} else {
		value = mkAt("FormattedValue", l, c, el, ec, expr, conversion, spec)
	}
	if debug == nil {
		return value
	}
	dt := mkAt("Constant", l, c+1, debugEndLine, debugEndOff-1, text, nil)
	return mkAt("JoinedStr", l, c, debugEndLine, debugEndOff, []*Node{dt, value})
}

func reprIdent(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}

// Expression text of an interpolation without trailing whitespace and the self documenting equals sign
func stripInterpolation(s string) string {
	r := pystr.Runes(s)
	n := len(r)
	for n > 0 && pystr.IsSpace(r[n-1]) {
		n--
	}
	if n > 0 && r[n-1] == '=' {
		n--
	}
	return pystr.FromRunes(r[:n])
}
