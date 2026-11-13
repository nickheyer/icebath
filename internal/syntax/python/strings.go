package python

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Identifier normalized with NFKC when it holds a non ASCII byte
func normIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return norm.NFKC.String(s)
		}
	}
	return s
}

type strPart struct {
	node Expr
	kind byte
}

// Implicitly concatenated string, bytes, f-string and t-string literals as one expression
func (p *parser) strings() Expr {
	start := p.cur()
	var parts []strPart
	for {
		t := p.cur()
		switch t.kind {
		case tString:
			p.next()
			n, isBytes := p.stringConstant(t)
			if p.bad() {
				return nil
			}
			k := byte('s')
			if isBytes {
				k = 'b'
			}
			parts = append(parts, strPart{n, k})
			continue
		case tFStart, tTStart:
			k := byte('f')
			if t.kind == tTStart {
				k = 't'
			}
			n := p.fstring()
			if p.bad() {
				return nil
			}
			parts = append(parts, strPart{n, k})
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
				return nil
			}
		}
	}
	nodes := make([]Expr, len(parts))
	for i, s := range parts {
		nodes[i] = s.node
	}
	if tCount > 0 {
		return &TemplateStr{Pos: p.span(start), Values: foldParts(nodes, true)}
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
		return nil
	}
	if hasBytes {
		var buf []byte
		for _, n := range nodes {
			buf = append(buf, n.(*Constant).Value.(Bytes)...)
		}
		return &Constant{Pos: p.span(start), Value: Bytes(buf), Kind: nodes[0].(*Constant).Kind}
	}
	if !hasF && len(parts) == 1 {
		return parts[0].node
	}
	folded := foldParts(nodes, hasF)
	if !hasF {
		return folded[0]
	}
	return &JoinedStr{Pos: p.span(start), Values: folded}
}

// Flattens joined strings and merges runs of string constants, dropping empty strings inside formatted literals
func foldParts(nodes []Expr, fstr bool) []Expr {
	var flat []Expr
	for _, n := range nodes {
		switch x := n.(type) {
		case *JoinedStr:
			flat = append(flat, x.Values...)
		case *TemplateStr:
			flat = append(flat, x.Values...)
		default:
			flat = append(flat, n)
		}
	}
	var out []Expr
	for i := 0; i < len(flat); i++ {
		e := flat[i]
		if c, ok := e.(*Constant); ok {
			if i+1 < len(flat) && isConstant(flat[i+1]) {
				var b strings.Builder
				last := c
				j := i
				for ; j < len(flat) && isConstant(flat[j]); j++ {
					last = flat[j].(*Constant)
					b.WriteString(string(last.Value.(Str)))
				}
				i = j - 1
				c = &Constant{Pos: Pos{c.Line, c.Col, last.EndLine, last.EndCol}, Value: Str(b.String()), Kind: c.Kind}
				e = c
			}
			if s, ok := c.Value.(Str); fstr && ok && s == "" {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func isConstant(e Expr) bool {
	_, ok := e.(*Constant)
	return ok
}

// Value of a STRING token with its prefixes, quotes and escapes resolved
func (p *parser) stringConstant(t *token) (*Constant, bool) {
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
	kind := ""
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
				return nil, true
			}
		}
		if raw {
			return &Constant{Pos: tokSpan(t), Value: Bytes(body), Kind: kind}, true
		}
		b, err := decodeBytesEscapes(body)
		if err != "" {
			p.raiseLast("(value error) " + err)
			return nil, true
		}
		return &Constant{Pos: tokSpan(t), Value: Bytes(b), Kind: kind}, true
	}
	if raw {
		return &Constant{Pos: tokSpan(t), Value: Str(body), Kind: kind}, false
	}
	v, err := p.decodeUnicodeEscapes(body)
	if err != "" {
		p.raiseLast("(unicode error) " + err)
		return nil, false
	}
	return &Constant{Pos: tokSpan(t), Value: Str(v), Kind: kind}, false
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

// Resolves the escapes of a bytes literal, returning the failure text for a bad hex escape
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

// Rewrites non ASCII text as \U escapes before escape decoding, so failure positions count escaped bytes
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

// Appends a code point as UTF-8, surrogates encoded as their three byte generalized form
func appendWTF8(b []byte, r rune) []byte {
	if r >= 0xd800 && r <= 0xdfff {
		return append(b, byte(0xe0|r>>12), byte(0x80|(r>>6)&0x3f), byte(0x80|r&0x3f))
	}
	return utf8.AppendRune(b, r)
}

// Resolves the escapes of a str literal, returning the decoder failure text on failure
func (p *parser) decodeUnicodeEscapes(src string) (string, string) {
	s := escapeBuffer(src)
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
			out = appendWTF8(out, v)
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
			out = appendWTF8(out, ch)
		case 'N':
			r, end, msg := p.namedEscape(s, i)
			if msg != "" {
				return "", unicodeEscapeError(start, end, msg, s)
			}
			i = end
			out = appendWTF8(out, r)
		default:
			out = append(out, '\\', e)
		}
	}
	return string(out), ""
}

// Resolves a \N{name} escape whose N ends before i, returning the escape end or the failure end and text
func (p *parser) namedEscape(s string, i int) (rune, int, string) {
	const malformed = "malformed \\N character escape"
	if i >= len(s) || s[i] != '{' {
		return 0, i, malformed
	}
	nameStart := i + 1
	j := nameStart
	for j < len(s) && s[j] != '}' {
		j++
	}
	if j >= len(s) || j == nameStart {
		return 0, j, malformed
	}
	if r, ok := p.lookupName(s[nameStart:j]); ok {
		return r, j + 1, ""
	}
	return 0, j + 1, "unknown Unicode character name"
}

// F-string or t-string from its start token through its end token
func (p *parser) fstring() Expr {
	start := p.next()
	tstr := start.kind == tTStart
	raw := strings.ContainsAny(start.val, "rR")
	p.rawFString = append(p.rawFString, raw)
	defer func() { p.rawFString = p.rawFString[:len(p.rawFString)-1] }()
	var items []Expr
	for {
		t := p.cur()
		if p.bad() {
			return nil
		}
		if t.kind == tFEnd || t.kind == tTEnd {
			p.next()
			break
		}
		if t.kind == tFMiddle || t.kind == tTMiddle {
			p.next()
			items = append(items, &Constant{Pos: tokSpan(t), Value: Str(t.val)})
			continue
		}
		if t.kind == tOp && t.val == "{" {
			f := p.replacementField(tstr)
			if p.bad() {
				return nil
			}
			items = append(items, f)
			continue
		}
		p.fail()
		return nil
	}
	var vals []Expr
	for _, it := range items {
		if js, ok := it.(*JoinedStr); ok {
			vals = append(vals, js.Values...)
			continue
		}
		if c, ok := it.(*Constant); ok {
			text := string(c.Value.(Str))
			if text == "{{" || text == "}}" {
				text = text[:1]
			}
			if !raw && strings.Contains(text, "\\") {
				v, err := p.decodeUnicodeEscapes(text)
				if err != "" {
					p.raiseLast("(unicode error) " + err)
					return nil
				}
				text = v
			}
			if text == "" {
				continue
			}
			it = &Constant{Pos: c.Pos, Value: Str(text)}
		}
		vals = append(vals, it)
	}
	if tstr {
		return &TemplateStr{Pos: p.span(start), Values: vals}
	}
	return &JoinedStr{Pos: p.span(start), Values: vals}
}

func strPrefix(tstr bool) string {
	if tstr {
		return "t-string: "
	}
	return "f-string: "
}

// Expression source of a replacement field with comments removed
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
	var out []byte
	inStr = false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		switch {
		case ch == '"' || ch == '\'':
			if !inStr {
				inStr, q = true, ch
			} else if ch == q {
				inStr = false
			}
			out = append(out, ch)
		case ch == '#' && !inStr:
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		default:
			out = append(out, ch)
		}
	}
	return string(out)
}

// One replacement field with its conversion, format spec and self documenting text
func (p *parser) replacementField(tstr bool) Expr {
	open := p.next()
	pre := strPrefix(tstr)
	if p.isOp("=", "!", ":", "}") {
		t := p.cur()
		p.raiseTok(t, pre+"valid expression required before '"+t.val+"'")
		return nil
	}
	if !p.isKw("yield") && !p.startsStarExpression() {
		p.raiseNextToken(pre + "expecting a valid expression after '{'")
		return nil
	}
	expr := p.annotatedRHS()
	if p.bad() {
		return nil
	}
	var debug *token
	if p.isOp("=") {
		debug = p.next()
	}
	if !p.isOp("!", ":", "}") {
		if debug == nil {
			p.raiseNextToken(pre + "expecting '=', or '!', or ':', or '}'")
		} else {
			p.raiseNextToken(pre + "expecting '!', or ':', or '}'")
		}
		return nil
	}
	conversion := -1
	var convTok, convName, specTok *token
	if p.isOp("!") {
		convTok = p.next()
		if p.isOp(":", "}") {
			p.raiseNextToken(pre + "missing conversion character")
			return nil
		}
		if !p.isName() {
			p.raiseNextToken(pre + "invalid conversion character")
			return nil
		}
		convName = p.next()
		if convTok.el != convName.l || convTok.ec != convName.c {
			p.raiseAt(convTok.l, convTok.c, convName.el, convName.ec, pre+"conversion type must come right after the exclamation mark")
			return nil
		}
		name := normIdent(convName.val)
		if runeLen(name) > 1 || !(name == "s" || name == "r" || name == "a") {
			p.raiseTok(convName, fmt.Sprintf("%sinvalid conversion character %s: expected 's', 'r', or 'a'", pre, "'"+strings.ReplaceAll(name, "'", "\\'")+"'"))
			return nil
		}
		conversion = int(name[0])
		if !p.isOp(":", "}") {
			p.raiseNextToken(pre + "expecting ':' or '}'")
			return nil
		}
	}
	var spec Expr
	if p.isOp(":") {
		specTok = p.next()
		if spec = p.formatSpec(specTok, tstr); p.bad() {
			return nil
		}
	}
	if !p.isOp("}") {
		p.raiseNextToken(pre + "expecting '}'")
		return nil
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
		sp := spec.Position()
		debugEndLine, debugEndOff = sp.Line, sp.Col+1
	}
	text := p.fieldText(open, endTok)
	var value Expr
	if tstr {
		value = &Interpolation{Pos: Pos{l, c, el, ec}, Value: expr, Str: stripInterpolation(text), Conversion: conversion, FormatSpec: spec}
	} else {
		value = &FormattedValue{Pos: Pos{l, c, el, ec}, Value: expr, Conversion: conversion, FormatSpec: spec}
	}
	if debug == nil {
		return value
	}
	dt := &Constant{Pos: Pos{l, c + 1, debugEndLine, debugEndOff - 1}, Value: Str(text)}
	return &JoinedStr{Pos: Pos{l, c, debugEndLine, debugEndOff}, Values: []Expr{dt, value}}
}

// Format spec after the colon of a replacement field, literal parts and nested fields
func (p *parser) formatSpec(specTok *token, tstr bool) Expr {
	pre := strPrefix(tstr)
	var items []Expr
	for {
		t := p.cur()
		if p.bad() {
			return nil
		}
		if t.kind == tFMiddle || t.kind == tTMiddle {
			p.next()
			v := t.val
			if strings.Contains(t.val, "\\") && !p.rawFString[len(p.rawFString)-1] {
				dv, err := p.decodeUnicodeEscapes(t.val)
				if err != "" {
					p.raiseLast("(unicode error) " + err)
					return nil
				}
				v = dv
			}
			items = append(items, &Constant{Pos: tokSpan(t), Value: Str(v)})
			continue
		}
		if t.kind == tOp && t.val == "{" {
			f := p.replacementField(tstr)
			if p.bad() {
				return nil
			}
			items = append(items, f)
			continue
		}
		break
	}
	if !p.isOp("}") {
		p.raiseNextToken(pre + "expecting '}', or format specs")
		return nil
	}
	var kept []Expr
	for _, it := range items {
		if c, ok := it.(*Constant); ok {
			if s, ok := c.Value.(Str); ok && s == "" {
				continue
			}
		}
		kept = append(kept, it)
	}
	if len(kept) == 0 || (len(kept) == 1 && isConstant(kept[0])) {
		return &JoinedStr{Pos: p.span(specTok), Values: kept}
	}
	return &JoinedStr{Pos: p.span(specTok), Values: foldParts(kept, true)}
}

// Expression text of an interpolation without trailing whitespace and the self documenting equals sign
func stripInterpolation(s string) string {
	r := []rune(s)
	n := len(r)
	for n > 0 && (unicode.IsSpace(r[n-1]) || r[n-1] >= 0x1c && r[n-1] <= 0x1f) {
		n--
	}
	if n > 0 && r[n-1] == '=' {
		n--
	}
	return string(r[:n])
}
