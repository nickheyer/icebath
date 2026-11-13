package fileutil

import (
	"bytes"
	"context"
	"encoding"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// On-disk JSON layout, the zero value a single line with a space after each separator
type JSONStyle struct {
	Lines  bool
	Indent int
	Tight  bool
	Sorted bool
	ASCII  bool
}

// Error for a string carrying bytes outside UTF-8 under a style that writes raw UTF-8
var ErrJSONNotUTF8 = errors.New("json string carries bytes that are not UTF-8")

// Parsed JSON value kept in document order, kind one of object, array, string, number or literal
type jsonNode struct {
	kind byte
	text string
	str  []rune
	keys [][]rune
	vals []jsonNode
}

// Encoding options that give floats their reference layout and keep raw surrogate escapes intact
var encodeOptions = json.JoinOptions(jsonv1.DefaultOptionsV1(),
	json.WithMarshalers(json.MarshalToFunc(marshalReference)), jsontext.PreserveRawStrings(true))

// Decoding options that hand lone surrogate escapes back as the bytes they stand for
var decodeOptions = json.JoinOptions(jsonv1.DefaultOptionsV1(),
	json.WithUnmarshalers(json.UnmarshalFromFunc(unmarshalReference)))

// Interfaces whose methods decide a type's own JSON form ahead of the reference layout
var (
	jsonMarshalerType   = reflect.TypeFor[jsonv1.Marshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	marshalerToType     = reflect.TypeFor[json.MarshalerTo]()
	jsonUnmarshalerType = reflect.TypeFor[jsonv1.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	unmarshalerFromType = reflect.TypeFor[json.UnmarshalerFrom]()
)

// Bytes of v in style, raw UTF-8 unless ASCII is set, and ErrJSONNotUTF8 for non-UTF-8 text without ASCII
func EncodeJSON(v any, style JSONStyle) ([]byte, error) {
	node, err := jsonTree(v)
	if err != nil {
		return nil, err
	}
	return style.render(node)
}

// Marshals v and parses the compact result into a node tree
func jsonTree(v any) (jsonNode, error) {
	raw, err := json.Marshal(v, encodeOptions)
	if err != nil {
		return jsonNode{}, err
	}
	p := jsonParser{src: raw}
	node, err := p.value()
	if err != nil {
		return jsonNode{}, err
	}
	if p.skipSpace(); p.pos != len(p.src) {
		return jsonNode{}, fmt.Errorf("json: trailing data at byte %d", p.pos)
	}
	return node, nil
}

// Writes floats in the shortest round-trip form with reference layout and non-UTF-8 strings as lone surrogate escapes
func marshalReference(enc *jsontext.Encoder, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || ownsJSONForm(rv.Type()) {
		return errors.ErrUnsupported
	}
	rv = rv.Elem()
	stringify, _ := json.GetOption(enc.Options(), json.StringifyNumbers)
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if stringify || math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.ErrUnsupported
		}
		return enc.WriteValue(jsontext.Value(referenceFloat(f, rv.Type().Bits())))
	case reflect.String:
		if stringify || utf8.ValidString(rv.String()) {
			return errors.ErrUnsupported
		}
		return enc.WriteValue(jsontext.Value(surrogateQuote(rv.String())))
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String || ownsJSONForm(rv.Type().Key()) || rv.IsNil() || !hasNonUTF8Key(rv) {
			return errors.ErrUnsupported
		}
		return marshalMapKeys(enc, rv)
	}
	return errors.ErrUnsupported
}

// Reports whether a pointer to t or t itself has methods that choose its JSON form
func ownsJSONForm(t reflect.Type) bool {
	for _, c := range []reflect.Type{t, reflect.PointerTo(t)} {
		if c.Implements(jsonMarshalerType) || c.Implements(textMarshalerType) || c.Implements(marshalerToType) {
			return true
		}
	}
	return false
}

// Reports whether any key of the string keyed map m holds bytes outside UTF-8
func hasNonUTF8Key(m reflect.Value) bool {
	for _, k := range m.MapKeys() {
		if !utf8.ValidString(k.String()) {
			return true
		}
	}
	return false
}

// Writes a string keyed map in byte sorted key order, non-UTF-8 keys as lone surrogate escapes
func marshalMapKeys(enc *jsontext.Encoder, m reflect.Value) error {
	keys := m.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, k := range keys {
		if err := enc.WriteValue(jsontext.Value(surrogateQuote(k.String()))); err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, m.MapIndex(k).Interface()); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// JSON string literal of s with every byte outside UTF-8 written as the lone surrogate U+DC80 plus the byte
func surrogateQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\u%04x`, 0xdc00+rune(s[i]))
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += max(size, 1)
	}
	b.WriteByte('"')
	return b.String()
}

// Shortest round-trip digits of f laid out as the reference writes floats, fixed point from 1e-4 below 1e16
func referenceFloat(f float64, bits int) string {
	s := strconv.FormatFloat(f, 'e', -1, bits)
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mant, expText, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	point := exp + 1
	if point > -4 && point <= 16 {
		switch {
		case point <= 0:
			return sign + "0." + strings.Repeat("0", -point) + digits
		case point >= len(digits):
			return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
		}
		return sign + digits[:point] + "." + digits[point:]
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	esign := "+"
	if exp < 0 {
		esign, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, esign, exp)
}

// Byte cursor over compact JSON text the encoder produced
type jsonParser struct {
	src []byte
	pos int
}

// Advances past insignificant whitespace
func (p *jsonParser) skipSpace() {
	for p.pos < len(p.src) && strings.IndexByte(" \t\r\n", p.src[p.pos]) >= 0 {
		p.pos++
	}
}

// Error naming the byte offset where the text stopped making sense
func (p *jsonParser) fail() error { return fmt.Errorf("json: malformed text at byte %d", p.pos) }

// Node tree of the next element at the cursor
func (p *jsonParser) value() (jsonNode, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return jsonNode{}, p.fail()
	}
	switch c := p.src[p.pos]; {
	case c == '{' || c == '[':
		return p.container(c)
	case c == '"':
		s, err := p.str()
		return jsonNode{kind: '"', str: s}, err
	}
	start := p.pos
	for p.pos < len(p.src) && strings.IndexByte(",:]} \t\r\n", p.src[p.pos]) < 0 {
		p.pos++
	}
	text := string(p.src[start:p.pos])
	switch text {
	case "true", "false", "null":
		return jsonNode{kind: 'l', text: text}, nil
	}
	if !jsonv1.Valid([]byte(text)) {
		return jsonNode{}, p.fail()
	}
	return jsonNode{kind: '0', text: text}, nil
}

// Object or array opened by c, members kept in document order
func (p *jsonParser) container(c byte) (jsonNode, error) {
	node := jsonNode{kind: c}
	closing := byte(']')
	if c == '{' {
		closing = '}'
	}
	p.pos++
	for first := true; ; first = false {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == closing {
			p.pos++
			return node, nil
		}
		if !first {
			if p.pos >= len(p.src) || p.src[p.pos] != ',' {
				return jsonNode{}, p.fail()
			}
			p.pos++
			p.skipSpace()
		}
		if c == '{' {
			if p.pos >= len(p.src) || p.src[p.pos] != '"' {
				return jsonNode{}, p.fail()
			}
			key, err := p.str()
			if err != nil {
				return jsonNode{}, err
			}
			if p.skipSpace(); p.pos >= len(p.src) || p.src[p.pos] != ':' {
				return jsonNode{}, p.fail()
			}
			p.pos++
			node.keys = append(node.keys, key)
		}
		v, err := p.value()
		if err != nil {
			return jsonNode{}, err
		}
		node.vals = append(node.vals, v)
	}
}

// Decoded string literal at the cursor, a surrogate escape without its partner kept as that lone code point
func (p *jsonParser) str() ([]rune, error) {
	p.pos++
	var out []rune
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return out, nil
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		default:
			r, size := utf8.DecodeRune(p.src[p.pos:])
			out = append(out, r)
			p.pos += size
		}
	}
	return nil, p.fail()
}

// One backslash sequence decoded, a valid surrogate pair joined into its code point
func (p *jsonParser) escape() (rune, error) {
	if p.pos+1 >= len(p.src) {
		return 0, p.fail()
	}
	c := p.src[p.pos+1]
	p.pos += 2
	if i := strings.IndexByte(`"\/bfnrt`, c); i >= 0 {
		return rune("\"\\/\b\f\n\r\t"[i]), nil
	}
	if c != 'u' {
		return 0, p.fail()
	}
	hi, ok := p.hex4()
	if !ok {
		return 0, p.fail()
	}
	if utf16.IsSurrogate(hi) && hi < 0xdc00 && p.pos+1 < len(p.src) && p.src[p.pos] == '\\' && p.src[p.pos+1] == 'u' {
		save := p.pos
		p.pos += 2
		if lo, ok := p.hex4(); ok && lo >= 0xdc00 && lo <= 0xdfff {
			return utf16.DecodeRune(hi, lo), nil
		}
		p.pos = save
	}
	return hi, nil
}

// Four hex digits at the cursor as a code unit
func (p *jsonParser) hex4() (rune, bool) {
	if p.pos+4 > len(p.src) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.src[p.pos:p.pos+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	p.pos += 4
	return rune(n), true
}

// Writes the tree in style, failing with ErrJSONNotUTF8 when a lone surrogate needs raw UTF-8
func (s JSONStyle) render(n jsonNode) ([]byte, error) {
	var b bytes.Buffer
	if err := s.emit(&b, n, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Writes n at nesting depth with the separators, indentation and key order of the style
func (s JSONStyle) emit(b *bytes.Buffer, n jsonNode, depth int) error {
	switch n.kind {
	case '"':
		return s.writeString(b, n.str)
	case '0', 'l':
		b.WriteString(n.text)
		return nil
	}
	open, closing := "[", "]"
	if n.kind == '{' {
		open, closing = "{", "}"
	}
	if len(n.vals) == 0 {
		b.WriteString(open + closing)
		return nil
	}
	item, key := ", ", ": "
	if s.Lines {
		item = ","
	}
	if s.Tight {
		item, key = ",", ":"
	}
	order := make([]int, len(n.vals))
	for i := range order {
		order[i] = i
	}
	if n.kind == '{' && s.Sorted {
		slices.SortStableFunc(order, func(a, c int) int { return slices.Compare(n.keys[a], n.keys[c]) })
	}
	b.WriteString(open)
	for i, idx := range order {
		if i > 0 {
			b.WriteString(item)
		}
		if s.Lines {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", s.Indent*(depth+1)))
		}
		if n.kind == '{' {
			if err := s.writeString(b, n.keys[idx]); err != nil {
				return err
			}
			b.WriteString(key)
		}
		if err := s.emit(b, n.vals[idx], depth+1); err != nil {
			return err
		}
	}
	if s.Lines {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", s.Indent*depth))
	}
	b.WriteString(closing)
	return nil
}

// Quoted string escaping quotes, backslashes and controls, and every rune outside printable ASCII under ASCII
func (s JSONStyle) writeString(b *bytes.Buffer, str []rune) error {
	b.WriteByte('"')
	for _, r := range str {
		if i := strings.IndexRune("\"\\\n\r\t\b\f", r); i >= 0 {
			b.WriteString([]string{`\"`, `\\`, `\n`, `\r`, `\t`, `\b`, `\f`}[i])
			continue
		}
		switch {
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case utf16.IsSurrogate(r) && !s.ASCII:
			return ErrJSONNotUTF8
		case s.ASCII && r >= 0x10000:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
		case s.ASCII && r > 0x7e:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return nil
}

// Atomically replaces path with v in style and platform line endings, keeping an existing mode and owner
func WriteJSON(ctx context.Context, path string, v any, style JSONStyle, opts ...WriteOption) error {
	node, err := jsonTree(v)
	if err != nil {
		return err
	}
	data, err := style.render(node)
	if errors.Is(err, ErrJSONNotUTF8) {
		style.ASCII = true
		data, err = style.render(node)
	}
	if err != nil {
		return err
	}
	cfg := newWriteConfig(writeConfig{prefix: "." + fileStem(path) + "_", keepPerm: true, keepOwner: true, umaskNew: true}, opts)
	return writeStaged(ctx, path, []byte(nativeLineEndings(string(data))), cfg)
}

// Decodes the JSON file at path into v, dropping a BOM, keeping interface numbers as json.Number and refusing invalid UTF-8
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	offset := int64(0)
	if bytes.HasPrefix(data, []byte(utf8BOM)) {
		data, offset = data[len(utf8BOM):], int64(len(utf8BOM))
	}
	if !utf8.Valid(data) {
		return &UTF8Error{Path: path, Offset: offset + int64(firstInvalidUTF8(string(data)))}
	}
	if err := json.Unmarshal(data, v, decodeOptions); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Fills interface values with exact numbers and string fields and keys with surrogate escapes turned back into bytes
func unmarshalReference(dec *jsontext.Decoder, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || ownsDecoding(rv.Type().Elem()) {
		return errors.ErrUnsupported
	}
	target := rv.Elem()
	stringify, _ := json.GetOption(dec.Options(), json.StringifyNumbers)
	switch {
	case target.Kind() == reflect.Interface && target.NumMethod() == 0:
		if !target.IsNil() && target.Elem().Kind() == reflect.Pointer && dec.PeekKind() != 'n' {
			return errors.ErrUnsupported
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		p := jsonParser{src: raw}
		node, err := p.value()
		if err != nil {
			return err
		}
		if val := node.goValue(); val != nil {
			target.Set(reflect.ValueOf(val))
		} else {
			target.SetZero()
		}
		return nil
	case target.Kind() == reflect.String && !stringify && dec.PeekKind() == '"':
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		p := jsonParser{src: raw}
		s, err := p.str()
		if err != nil {
			return err
		}
		target.SetString(surrogateBytes(s))
		return nil
	case target.Kind() == reflect.Map && target.Type().Key().Kind() == reflect.String && !ownsDecoding(target.Type().Key()) && dec.PeekKind() == '{':
		return unmarshalMapKeys(dec, target)
	}
	return errors.ErrUnsupported
}

// Reports whether t or a pointer to it decodes itself
func ownsDecoding(t reflect.Type) bool {
	for _, c := range []reflect.Type{t, reflect.PointerTo(t)} {
		if c.Implements(jsonUnmarshalerType) || c.Implements(textUnmarshalerType) || c.Implements(unmarshalerFromType) {
			return true
		}
	}
	return false
}

// Merges a JSON object into a string keyed map, keys decoded with their surrogate escapes as bytes
func unmarshalMapKeys(dec *jsontext.Decoder, m reflect.Value) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if m.IsNil() {
		m.Set(reflect.MakeMap(m.Type()))
	}
	for dec.PeekKind() != '}' {
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		p := jsonParser{src: raw}
		key, err := p.str()
		if err != nil {
			return err
		}
		elem := reflect.New(m.Type().Elem())
		if err := json.UnmarshalDecode(dec, elem.Interface()); err != nil {
			return err
		}
		m.SetMapIndex(reflect.ValueOf(surrogateBytes(key)).Convert(m.Type().Key()), elem.Elem())
	}
	_, err := dec.ReadToken()
	return err
}

// Go value of a node, objects as maps, arrays as slices and numbers as their exact text
func (n jsonNode) goValue() any {
	switch n.kind {
	case '{':
		m := make(map[string]any, len(n.vals))
		for i, k := range n.keys {
			m[surrogateBytes(k)] = n.vals[i].goValue()
		}
		return m
	case '[':
		s := make([]any, len(n.vals))
		for i, v := range n.vals {
			s[i] = v.goValue()
		}
		return s
	case '"':
		return surrogateBytes(n.str)
	case '0':
		return jsonv1.Number(n.text)
	}
	switch n.text {
	case "true":
		return true
	case "false":
		return false
	}
	return nil
}

// String of runes with each lone surrogate U+DC80 to U+DCFF turned back into the byte it escapes
func surrogateBytes(rs []rune) string {
	var b strings.Builder
	for _, r := range rs {
		if r >= 0xdc80 && r <= 0xdcff {
			b.WriteByte(byte(r - 0xdc00))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
