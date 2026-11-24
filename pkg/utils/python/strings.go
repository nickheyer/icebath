package pythonutil

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	unicodeutil "github.com/nickheyer/icebath/pkg/utils/unicode"
)

// Longest decimal integer literal Python converts, longer ones being a syntax error
const maxStrDigits = 4300

// Value of a NUMBER token: int64, *big.Int past int64, float64 or complex128
func (p *parser) numberValue(t *token) any {
	s := t.val
	if strings.IndexByte(s, '_') >= 0 {
		s = strings.ReplaceAll(s, "_", "")
	}
	if last := s[len(s)-1]; last == 'j' || last == 'J' {
		f, _ := strconv.ParseFloat(s[:len(s)-1], 64)
		return complex(0, f)
	}
	base, digits := 10, s
	if len(s) > 1 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 10 {
			digits = s[2:]
		}
	}
	if base == 10 {
		if strings.ContainsAny(s, ".eE") {
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		if len(s) > maxStrDigits {
			p.raiseKnown(KindSyntax, t.l, 0, t.el, 0, fmt.Sprintf("Exceeds the limit (%d digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit - Consider hexadecimal for huge integer literals to avoid decimal conversion limits.", maxStrDigits, len(s)))
		}
	}
	if v, err := strconv.ParseInt(digits, base, 64); err == nil {
		return v
	}
	v, _ := new(big.Int).SetString(digits, base)
	return v
}

type strPart struct {
	node *Node
	kind byte
}

// Implicitly concatenated string literals folded into one constant, f-string or t-string
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
		nodes := make([]*Node, len(parts))
		for i, s := range parts {
			nodes[i] = s.node
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
		if len(parts) == 1 {
			return parts[0].node
		}
		var buf []byte
		for _, s := range parts {
			buf = append(buf, s.node.Get("value").([]byte)...)
		}
		return p.mkFrom(start, "Constant", buf, parts[0].node.Get("kind"))
	}
	if !hasF && len(parts) == 1 {
		return parts[0].node
	}
	nodes := make([]*Node, len(parts))
	for i, s := range parts {
		nodes[i] = s.node
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
				e = mkAt("Constant", first.Line, first.Col, last.EndLine, last.EndCol, b.String(), first.Get("kind"))
			}
			if s, ok := e.Get("value").(string); fstr && ok && s == "" {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// Constant of a STRING token with its escapes decoded, and whether it is a bytes literal
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
	raw = raw || strings.IndexByte(body, '\\') < 0
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

// Bytes of a bytes literal body with its escapes decoded, or the error text of an invalid \x escape
func decodeBytesEscapes(s string) ([]byte, string) {
	out := make([]byte, 0, len(s))
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

func unicodeEscapeError(start, end int, msg string, buf string) string {
	if end-start == 1 {
		return fmt.Sprintf("'unicodeescape' codec can't decode byte 0x%02x in position %d: %s", buf[start], start, msg)
	}
	return fmt.Sprintf("'unicodeescape' codec can't decode bytes in position %d-%d: %s", start, end-1, msg)
}

// Text of a str literal body with its escapes decoded, or the error text of a malformed escape; a surrogate code point decodes to U+FFFD
func decodeUnicodeEscapes(s string) (string, string) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		start := i
		i++
		if i >= len(s) || s[i] >= 0x80 {
			out = append(out, '\\')
			continue
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
			out = utf8.AppendRune(out, v)
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
			if ch > unicode.MaxRune || ch < 0 {
				return "", unicodeEscapeError(start, i, "illegal Unicode character", s)
			}
			out = utf8.AppendRune(out, ch)
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
					if r, ok := unicodeutil.Lookup(s[nameStart:j]); ok {
						out = utf8.AppendRune(out, r)
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
	p.rawFString = append(p.rawFString, raw)
	defer func() { p.rawFString = p.rawFString[:len(p.rawFString)-1] }()
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
			if !raw && strings.IndexByte(text, '\\') >= 0 {
				v, err := decodeUnicodeEscapes(text)
				if err != "" {
					p.raiseLast("(unicode error) " + err)
				}
				text = v
			}
			if text == "" {
				continue
			}
			it = mkAt("Constant", it.Line, it.Col, it.EndLine, it.EndCol, text, nil)
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
		return src
	}
	out := make([]byte, 0, len(src))
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
		if utf8.RuneCountInString(name) > 1 || !(name == "s" || name == "r" || name == "a") {
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
				v := t.val
				if strings.IndexByte(v, '\\') >= 0 && !p.rawFString[len(p.rawFString)-1] {
					decoded, err := decodeUnicodeEscapes(v)
					if err != "" {
						p.raiseLast("(unicode error) " + err)
					}
					v = decoded
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
		debugEndLine, debugEndOff = spec.Line, spec.Col+1
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
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	return strings.TrimSuffix(s, "=")
}
