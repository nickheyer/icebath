package state

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errImportJSONSyntax = errors.New("invalid JSON document")

var errImportJSONUnencodable = errors.New("JSON text holds a lone surrogate")

type jnode struct {
	kind  byte
	start int
	raw   []byte
	float bool
	keys  []string
	kraw  [][]byte
	kids  []*jnode
}

func (n *jnode) member(key string) *jnode {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.kids[i]
		}
	}
	return nil
}

func (n *jnode) isNull() bool { return n == nil || n.kind == 'n' }

func (n *jnode) add(key string, kraw []byte, kid *jnode) {
	if n.kind == '[' {
		n.kids = append(n.kids, kid)
		return
	}
	for i, k := range n.keys {
		if k == key {
			n.kids[i] = kid
			return
		}
	}
	n.keys = append(n.keys, key)
	n.kraw = append(n.kraw, kraw)
	n.kids = append(n.kids, kid)
}

func (n *jnode) text() string {
	if n == nil || n.kind != '"' {
		return ""
	}
	var s string
	_ = json.Unmarshal(n.raw, &s)
	return s
}

func (n *jnode) truthy() bool { return n != nil && jsonTruthy(n.raw) }

func (n *jnode) floatValue() float64 {
	switch string(n.raw) {
	case "NaN":
		return math.NaN()
	case "Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(string(n.raw), 64)
	return f
}

func (n *jnode) nonFinite() bool {
	if n.kind != '0' || !n.float {
		return false
	}
	f := n.floatValue()
	return math.IsNaN(f) || math.IsInf(f, 0)
}

type importJSONParser struct {
	data []byte
	pos  int
}

func parseImportJSON(data []byte) (*jnode, error) {
	p := &importJSONParser{data: data}
	type frame struct {
		node *jnode
		key  string
		kraw []byte
	}
	var stack []*frame
	for {
		n, err := p.begin()
		if err != nil {
			return nil, err
		}
		if n.kind == '{' || n.kind == '[' {
			p.ws()
			if p.pos < len(p.data) && p.data[p.pos] == closer(n.kind) {
				p.pos++
				n.raw = p.data[n.start:p.pos]
			} else {
				f := &frame{node: n}
				if n.kind == '{' {
					if f.key, f.kraw, err = p.key(); err != nil {
						return nil, err
					}
				}
				stack = append(stack, f)
				continue
			}
		}
		for {
			if len(stack) == 0 {
				p.ws()
				if p.pos != len(p.data) {
					return nil, errImportJSONSyntax
				}
				return n, nil
			}
			top := stack[len(stack)-1]
			top.node.add(top.key, top.kraw, n)
			p.ws()
			if p.pos >= len(p.data) {
				return nil, errImportJSONSyntax
			}
			c := p.data[p.pos]
			p.pos++
			if c == ',' {
				if top.node.kind == '{' {
					if top.key, top.kraw, err = p.key(); err != nil {
						return nil, err
					}
				}
				break
			}
			if c != closer(top.node.kind) {
				return nil, errImportJSONSyntax
			}
			top.node.raw = p.data[top.node.start:p.pos]
			n = top.node
			stack = stack[:len(stack)-1]
		}
	}
}

func closer(open byte) byte {
	if open == '{' {
		return '}'
	}
	return ']'
}

func (p *importJSONParser) ws() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *importJSONParser) key() (string, []byte, error) {
	p.ws()
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return "", nil, errImportJSONSyntax
	}
	n, err := p.str()
	if err != nil {
		return "", nil, err
	}
	p.ws()
	if p.pos >= len(p.data) || p.data[p.pos] != ':' {
		return "", nil, errImportJSONSyntax
	}
	p.pos++
	return n.text(), n.raw, nil
}

func (p *importJSONParser) literal(word string) bool {
	if strings.HasPrefix(string(p.data[p.pos:min(len(p.data), p.pos+len(word))]), word) && p.pos+len(word) <= len(p.data) {
		p.pos += len(word)
		return true
	}
	return false
}

func (p *importJSONParser) begin() (*jnode, error) {
	p.ws()
	if p.pos >= len(p.data) {
		return nil, errImportJSONSyntax
	}
	start := p.pos
	switch c := p.data[p.pos]; c {
	case '{', '[':
		p.pos++
		return &jnode{kind: c, start: start}, nil
	case '"':
		return p.str()
	case 't', 'f', 'n':
		for _, word := range []string{"true", "false", "null"} {
			if word[0] == c && p.literal(word) {
				return &jnode{kind: c, start: start, raw: p.data[start:p.pos]}, nil
			}
		}
		return nil, errImportJSONSyntax
	case 'N', 'I':
		if p.literal("NaN") || p.literal("Infinity") {
			return &jnode{kind: '0', start: start, raw: p.data[start:p.pos], float: true}, nil
		}
		return nil, errImportJSONSyntax
	}
	return p.number()
}

func (p *importJSONParser) number() (*jnode, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
		if p.literal("Infinity") {
			return &jnode{kind: '0', start: start, raw: p.data[start:p.pos], float: true}, nil
		}
	}
	digits := func() int {
		n := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	if p.pos >= len(p.data) {
		return nil, errImportJSONSyntax
	}
	if p.data[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		return nil, errImportJSONSyntax
	}
	isFloat := false
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		save := p.pos
		p.pos++
		if digits() == 0 {
			p.pos = save
		} else {
			isFloat = true
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		save := p.pos
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			p.pos = save
		} else {
			isFloat = true
		}
	}
	return &jnode{kind: '0', start: start, raw: p.data[start:p.pos], float: isFloat}, nil
}

func (p *importJSONParser) str() (*jnode, error) {
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return &jnode{kind: '"', start: start, raw: p.data[start:p.pos]}, nil
		case c == '\\':
			if p.pos+1 >= len(p.data) {
				return nil, errImportJSONSyntax
			}
			switch p.data[p.pos+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos += 2
			case 'u':
				if p.pos+6 > len(p.data) {
					return nil, errImportJSONSyntax
				}
				if _, err := strconv.ParseUint(string(p.data[p.pos+2:p.pos+6]), 16, 16); err != nil {
					return nil, errImportJSONSyntax
				}
				p.pos += 6
			default:
				return nil, errImportJSONSyntax
			}
		case c < 0x20:
			return nil, errImportJSONSyntax
		default:
			p.pos++
		}
	}
	return nil, errImportJSONSyntax
}

func importNonFinitePath(root *jnode, path string) (string, bool) {
	type pending struct {
		node *jnode
		path string
	}
	stack := []pending{{root, path}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.node == nil {
			continue
		}
		if cur.node.nonFinite() {
			return cur.path, true
		}
		switch cur.node.kind {
		case '{':
			for i, k := range cur.node.keys {
				stack = append(stack, pending{cur.node.kids[i], cur.path + "." + k})
			}
		case '[':
			for i, kid := range cur.node.kids {
				stack = append(stack, pending{kid, cur.path + "[" + strconv.Itoa(i) + "]"})
			}
		}
	}
	return "", false
}

type importJSONStyle struct {
	spaced bool
	ascii  bool
	skip   string
}

func encodeImportJSON(b *strings.Builder, n *jnode, style importJSONStyle, top bool) error {
	itemSep, keySep := ",", ":"
	if style.spaced {
		itemSep, keySep = ", ", ": "
	}
	switch n.kind {
	case '{':
		b.WriteByte('{')
		first := true
		for i, k := range n.keys {
			if top && style.skip != "" && k == style.skip {
				continue
			}
			if !first {
				b.WriteString(itemSep)
			}
			first = false
			if err := encodeImportString(b, n.kraw[i], style.ascii); err != nil {
				return err
			}
			b.WriteString(keySep)
			if err := encodeImportJSON(b, n.kids[i], style, false); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case '[':
		b.WriteByte('[')
		for i, kid := range n.kids {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if err := encodeImportJSON(b, kid, style, false); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case '"':
		return encodeImportString(b, n.raw, style.ascii)
	case '0':
		b.WriteString(importNumberText(n))
	default:
		b.Write(n.raw)
	}
	return nil
}

func importNumberText(n *jnode) string {
	if !n.float {
		if strings.TrimLeft(string(n.raw), "-0") == "" {
			return "0"
		}
		return string(n.raw)
	}
	f := n.floatValue()
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return shortestFloatText(f)
}

func importStringUnits(raw []byte) []rune {
	body := raw[1 : len(raw)-1]
	var out []rune
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			r, size := utf8.DecodeRune(body[i:])
			out = append(out, r)
			i += size
			continue
		}
		switch body[i+1] {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			v, _ := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
			unit := rune(v)
			if unit >= 0xD800 && unit < 0xDC00 && i+12 <= len(body) && body[i+6] == '\\' && body[i+7] == 'u' {
				if low, err := strconv.ParseUint(string(body[i+8:i+12]), 16, 16); err == nil && low >= 0xDC00 && low < 0xE000 {
					out = append(out, 0x10000+(unit-0xD800)<<10+(rune(low)-0xDC00))
					i += 12
					continue
				}
			}
			out = append(out, unit)
			i += 6
			continue
		default:
			out = append(out, rune(body[i+1]))
		}
		i += 2
	}
	return out
}

func encodeImportString(b *strings.Builder, raw []byte, ascii bool) error {
	b.WriteByte('"')
	for _, r := range importStringUnits(raw) {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			b.WriteString(`\u` + hexN(uint32(r), 4))
		case ascii && r >= 0x10000:
			r -= 0x10000
			b.WriteString(`\u` + hexN(uint32(0xD800+(r>>10)), 4) + `\u` + hexN(uint32(0xDC00+(r&0x3FF)), 4))
		case ascii && r > 0x7e:
			b.WriteString(`\u` + hexN(uint32(r), 4))
		case r >= 0xD800 && r < 0xE000:
			return errImportJSONUnencodable
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return nil
}

type importIntOutcome int

const (
	importIntValid importIntOutcome = iota
	importIntOutOfRange
	importIntInvalid
)

func importIntOf(n *jnode) (int64, importIntOutcome) {
	if n == nil {
		return 0, importIntInvalid
	}
	switch n.kind {
	case 't':
		return 1, importIntValid
	case 'f':
		return 0, importIntValid
	case '0':
		if !n.float {
			v, err := strconv.ParseInt(string(n.raw), 10, 64)
			if err != nil {
				return 0, importIntOutOfRange
			}
			return v, importIntValid
		}
		return importIntOfFloat(n.floatValue())
	case '"':
		return importIntOfText(n.text())
	}
	return 0, importIntInvalid
}

func importIntOfFloat(f float64) (int64, importIntOutcome) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, importIntInvalid
	}
	f = math.Trunc(f)
	if f >= 9223372036854775808.0 || f < -9223372036854775808.0 {
		return 0, importIntOutOfRange
	}
	return int64(f), importIntValid
}

func importIntOfText(s string) (int64, importIntOutcome) {
	t := strings.TrimFunc(s, unicode.IsSpace)
	sign := ""
	if t != "" && (t[0] == '+' || t[0] == '-') {
		sign, t = t[:1], t[1:]
	}
	digits, ok := importDigitRun(t)
	if !ok {
		return 0, importIntInvalid
	}
	v, err := strconv.ParseInt(sign+digits, 10, 64)
	if err != nil {
		return 0, importIntOutOfRange
	}
	return v, importIntValid
}

func importDigitRun(t string) (string, bool) {
	if t == "" {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '_' && i > 0 && i+1 < len(t) && t[i-1] >= '0' && t[i-1] <= '9' && t[i+1] >= '0' && t[i+1] <= '9':
		default:
			return "", false
		}
	}
	return b.String(), true
}

func importFloatOf(n *jnode) (float64, bool) {
	if n == nil {
		return 0, false
	}
	switch n.kind {
	case 't':
		return 1, true
	case 'f':
		return 0, true
	case '0':
		if n.float {
			return n.floatValue(), true
		}
		f, _ := strconv.ParseFloat(string(n.raw), 64)
		return f, true
	case '"':
		return importFloatOfText(n.text())
	}
	return 0, false
}

func importFloatOfText(s string) (float64, bool) {
	t := strings.TrimFunc(s, unicode.IsSpace)
	sign := ""
	if t != "" && (t[0] == '+' || t[0] == '-') {
		sign, t = t[:1], t[1:]
	}
	switch strings.ToLower(t) {
	case "inf", "infinity":
		if sign == "-" {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	mantissa, exponent, hasExp := strings.Cut(strings.ReplaceAll(t, "E", "e"), "e")
	whole, frac, hasPoint := strings.Cut(mantissa, ".")
	clean := ""
	if whole != "" {
		w, ok := importDigitRun(whole)
		if !ok {
			return 0, false
		}
		clean = w
	}
	if hasPoint {
		clean += "."
		if frac != "" {
			f, ok := importDigitRun(frac)
			if !ok {
				return 0, false
			}
			clean += f
		}
	}
	if whole == "" && frac == "" {
		return 0, false
	}
	if hasExp {
		expSign := ""
		if exponent != "" && (exponent[0] == '+' || exponent[0] == '-') {
			expSign, exponent = exponent[:1], exponent[1:]
		}
		e, ok := importDigitRun(exponent)
		if !ok {
			return 0, false
		}
		clean += "e" + expSign + e
	}
	f, err := strconv.ParseFloat(sign+clean, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

func importEpochOf(n *jnode, field string) (float64, bool) {
	if n.isNull() || (n.kind == '"' && n.text() == "") {
		return 0, false
	}
	f, ok := importFloatOf(n)
	if !ok {
		f = math.NaN()
	}
	return trustedEpoch(f, field)
}
