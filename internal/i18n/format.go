package i18n

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Args map[string]any

type kind int

const (
	textKind kind = iota
	intKind
	floatKind
	boolKind
	noneKind
)

type value struct {
	kind kind
	text string
	num  *big.Int
	f    float64
	b    bool
}

func valueOf(v any) value {
	switch t := v.(type) {
	case nil:
		return value{kind: noneKind}
	case string:
		return value{kind: textKind, text: t}
	case bool:
		return value{kind: boolKind, b: t}
	case int:
		return value{kind: intKind, num: big.NewInt(int64(t))}
	case int8:
		return value{kind: intKind, num: big.NewInt(int64(t))}
	case int16:
		return value{kind: intKind, num: big.NewInt(int64(t))}
	case int32:
		return value{kind: intKind, num: big.NewInt(int64(t))}
	case int64:
		return value{kind: intKind, num: big.NewInt(t)}
	case uint:
		return value{kind: intKind, num: new(big.Int).SetUint64(uint64(t))}
	case uint8:
		return value{kind: intKind, num: new(big.Int).SetUint64(uint64(t))}
	case uint16:
		return value{kind: intKind, num: new(big.Int).SetUint64(uint64(t))}
	case uint32:
		return value{kind: intKind, num: new(big.Int).SetUint64(uint64(t))}
	case uint64:
		return value{kind: intKind, num: new(big.Int).SetUint64(t)}
	case uintptr:
		return value{kind: intKind, num: new(big.Int).SetUint64(uint64(t))}
	case *big.Int:
		if t == nil {
			return value{kind: noneKind}
		}
		return value{kind: intKind, num: new(big.Int).Set(t)}
	case float32:
		return value{kind: floatKind, f: float64(t)}
	case float64:
		return value{kind: floatKind, f: t}
	case error:
		return value{kind: textKind, text: t.Error()}
	case fmt.Stringer:
		return value{kind: textKind, text: t.String()}
	}
	return value{kind: textKind, text: fmt.Sprint(v)}
}

func (v value) str() string {
	switch v.kind {
	case noneKind:
		return "None"
	case boolKind:
		if v.b {
			return "True"
		}
		return "False"
	case intKind:
		return v.num.String()
	case floatKind:
		return floatRepr(v.f)
	}
	return v.text
}

func (v value) repr(ascii bool) string {
	if v.kind == textKind {
		return quote(v.text, ascii)
	}
	return v.str()
}

func render(text string, args Args, depth int) (string, error) {
	if depth >= 2 {
		return "", errors.New("Max string recursion exceeded")
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		switch c {
		case '{':
			if i+1 < len(text) && text[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			if i+1 == len(text) {
				return "", errors.New("Single '{' encountered in format string")
			}
			end, err := fieldEnd(text, i+1)
			if err != nil {
				return "", err
			}
			out, err := replace(text[i+1:end], args, depth)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i = end + 1
		case '}':
			if i+1 < len(text) && text[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			return "", errors.New("Single '}' encountered in format string")
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

func fieldEnd(text string, start int) (int, error) {
	count := 1
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			count++
		case '}':
			count--
			if count == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("expected '}' before end of string")
}

func replace(field string, args Args, depth int) (string, error) {
	name, conv, spec, hasConv, err := splitField(field)
	if err != nil {
		return "", err
	}
	first := name
	if i := strings.IndexAny(name, ".["); i >= 0 {
		first = name[:i]
	}
	if first == "" {
		return "", errors.New("Format string contains positional fields")
	}
	if allDigits(first) {
		return "", fmt.Errorf("Replacement index %s out of range for positional args tuple", first)
	}
	raw, ok := args[first]
	if !ok {
		return "", fmt.Errorf("missing value for %s", quote(first, false))
	}
	if first != name {
		return "", fmt.Errorf("attribute or index access in field %s", quote(name, false))
	}
	v := valueOf(raw)
	if hasConv {
		switch conv {
		case "s":
			v = value{kind: textKind, text: v.str()}
		case "r":
			v = value{kind: textKind, text: v.repr(false)}
		case "a":
			v = value{kind: textKind, text: v.repr(true)}
		default:
			return "", fmt.Errorf("Unknown conversion specifier %s", conv)
		}
	}
	if strings.ContainsRune(spec, '{') {
		if spec, err = render(spec, args, depth+1); err != nil {
			return "", err
		}
	}
	return formatValue(v, spec)
}

func splitField(field string) (name, conv, spec string, hasConv bool, err error) {
	i := 0
	for i < len(field) {
		c := field[i]
		if c == '{' {
			return "", "", "", false, errors.New("unexpected '{' in field name")
		}
		if c == '[' {
			for i < len(field) && field[i] != ']' {
				i++
			}
			continue
		}
		if c == ':' || c == '!' {
			break
		}
		i++
	}
	name = field[:i]
	if i == len(field) {
		return name, "", "", false, nil
	}
	if field[i] == ':' {
		return name, "", field[i+1:], false, nil
	}
	rest := field[i+1:]
	if rest == "" {
		return "", "", "", false, errors.New("end of string while looking for conversion specifier")
	}
	r, size := utf8.DecodeRuneInString(rest)
	conv, rest = string(r), rest[size:]
	if rest == "" {
		return name, conv, "", true, nil
	}
	if rest[0] != ':' {
		return "", "", "", false, errors.New("expected ':' after conversion specifier")
	}
	return name, conv, rest[1:], true, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.Is(unicode.Nd, r) {
			return false
		}
	}
	return s != ""
}

type spec struct {
	fill     rune
	align    rune
	sign     rune
	z        bool
	alt      bool
	width    int
	group    rune
	prec     int
	typ      rune
	hasFill  bool
	hasAlign bool
}

func isAlign(r rune) bool {
	return r == '<' || r == '>' || r == '=' || r == '^'
}

func parseSpec(s string, defaultAlign rune) (spec, error) {
	sp := spec{fill: ' ', align: defaultAlign, prec: -1}
	rs := []rune(s)
	i := 0
	if len(rs) >= 2 && isAlign(rs[1]) {
		sp.fill, sp.align, sp.hasFill, sp.hasAlign = rs[0], rs[1], true, true
		i = 2
	} else if len(rs) >= 1 && isAlign(rs[0]) {
		sp.align, sp.hasAlign = rs[0], true
		i = 1
	}
	if i < len(rs) && (rs[i] == '+' || rs[i] == '-' || rs[i] == ' ') {
		sp.sign = rs[i]
		i++
	}
	if i < len(rs) && rs[i] == 'z' {
		sp.z = true
		i++
	}
	if i < len(rs) && rs[i] == '#' {
		sp.alt = true
		i++
	}
	if !sp.hasFill && i < len(rs) && rs[i] == '0' {
		sp.fill = '0'
		if !sp.hasAlign && defaultAlign == '>' {
			sp.align = '='
		}
		i++
	}
	n, next, err := digitsAt(rs, i)
	if err != nil {
		return sp, err
	}
	if next > i {
		sp.width = n
	}
	i = next
	if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
		sp.group = rs[i]
		i++
		if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
			return sp, errors.New("Cannot specify both ',' and '_'.")
		}
	}
	if i < len(rs) && rs[i] == '.' {
		i++
		n, next, err := digitsAt(rs, i)
		if err != nil {
			return sp, err
		}
		if next == i {
			return sp, errors.New("Format specifier missing precision")
		}
		sp.prec, i = n, next
	}
	if len(rs)-i > 1 {
		return sp, errors.New("Invalid format specifier")
	}
	if i < len(rs) {
		sp.typ = rs[i]
	}
	if sp.group != 0 {
		switch sp.typ {
		case 'd', 'e', 'f', 'g', 'E', 'G', '%', 'F', 0:
		case 'b', 'o', 'x', 'X':
			if sp.group != '_' {
				return sp, fmt.Errorf("Cannot specify ',' with '%c'.", sp.typ)
			}
		default:
			return sp, fmt.Errorf("Cannot specify '%c' with '%c'.", sp.group, sp.typ)
		}
	}
	return sp, nil
}

func digitsAt(rs []rune, i int) (int, int, error) {
	n := 0
	start := i
	for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
		if i-start >= 18 {
			return 0, i, errors.New("Too many decimal digits in format string")
		}
		n = n*10 + int(rs[i]-'0')
		i++
	}
	return n, i, nil
}

func formatValue(v value, s string) (string, error) {
	switch v.kind {
	case noneKind:
		if s == "" {
			return v.str(), nil
		}
		return "", errors.New("unsupported format string passed to NoneType.__format__")
	case boolKind:
		if s == "" {
			return v.str(), nil
		}
		n := int64(0)
		if v.b {
			n = 1
		}
		return formatInt(big.NewInt(n), s)
	case intKind:
		if s == "" {
			return v.str(), nil
		}
		return formatInt(v.num, s)
	case floatKind:
		if s == "" {
			return v.str(), nil
		}
		return formatFloat(v.f, s)
	}
	return formatText(v.text, s)
}

func formatText(text, s string) (string, error) {
	sp, err := parseSpec(s, '<')
	if err != nil {
		return "", err
	}
	if sp.typ != 0 && sp.typ != 's' {
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'str'", sp.typ)
	}
	switch {
	case sp.sign != 0:
		return "", errors.New("Sign not allowed in string format specifier")
	case sp.z:
		return "", errors.New("Negative zero coercion (z) not allowed in format specifier")
	case sp.alt:
		return "", errors.New("Alternate form (#) not allowed in string format specifier")
	case sp.align == '=':
		return "", errors.New("'=' alignment not allowed in string format specifier")
	}
	if sp.prec >= 0 && utf8.RuneCountInString(text) > sp.prec {
		text = string([]rune(text)[:sp.prec])
	}
	return pad("", text, sp), nil
}

func formatInt(n *big.Int, s string) (string, error) {
	sp, err := parseSpec(s, '>')
	if err != nil {
		return "", err
	}
	switch sp.typ {
	case 'e', 'E', 'f', 'F', 'g', 'G', '%':
		f, acc := new(big.Float).SetInt(n).Float64()
		if math.IsInf(f, 0) && acc != big.Exact {
			return "", errors.New("int too large to convert to float")
		}
		return floatWith(f, sp)
	case 0, 'd', 'n', 'b', 'o', 'x', 'X', 'c':
	default:
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'int'", sp.typ)
	}
	if sp.prec >= 0 {
		return "", errors.New("Precision not allowed in integer format specifier")
	}
	if sp.z {
		return "", errors.New("Negative zero coercion (z) not allowed in integer format specifier")
	}
	if sp.typ == 'c' {
		if sp.sign != 0 {
			return "", errors.New("Sign not allowed with integer format specifier 'c'")
		}
		if sp.alt {
			return "", errors.New("Alternate form (#) not allowed with integer format specifier 'c'")
		}
		if n.Sign() < 0 || n.Cmp(big.NewInt(0x10ffff)) > 0 {
			return "", errors.New("%c arg not in range(0x110000)")
		}
		return pad("", string(rune(n.Int64())), sp), nil
	}
	abs := new(big.Int).Abs(n)
	var digits, prefix string
	size := 3
	switch sp.typ {
	case 'b':
		digits, prefix, size = abs.Text(2), "0b", 4
	case 'o':
		digits, prefix, size = abs.Text(8), "0o", 4
	case 'x':
		digits, prefix, size = abs.Text(16), "0x", 4
	case 'X':
		digits, prefix, size = strings.ToUpper(abs.Text(16)), "0X", 4
	default:
		digits = abs.Text(10)
	}
	if !sp.alt {
		prefix = ""
	}
	return number(n.Sign() < 0, prefix, digits, "", size, sp), nil
}

func formatFloat(f float64, s string) (string, error) {
	sp, err := parseSpec(s, '>')
	if err != nil {
		return "", err
	}
	switch sp.typ {
	case 0, 'e', 'E', 'f', 'F', 'g', 'G', 'n', '%':
	default:
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'float'", sp.typ)
	}
	return floatWith(f, sp)
}

func floatWith(f float64, sp spec) (string, error) {
	neg := math.Signbit(f) && !math.IsNaN(f)
	x := math.Abs(f)
	suffix := ""
	if sp.typ == '%' {
		suffix = "%"
	}
	upper := sp.typ == 'E' || sp.typ == 'F' || sp.typ == 'G'
	if math.IsInf(x, 0) || math.IsNaN(x) {
		body := "inf"
		if math.IsNaN(x) {
			body = "nan"
		}
		if upper {
			body = strings.ToUpper(body)
		}
		return number(neg, "", "", body+suffix, 3, spec{fill: sp.fill, align: sp.align, sign: sp.sign, width: sp.width}), nil
	}
	body := floatBody(x, sp)
	if upper {
		body = strings.ToUpper(body)
	}
	if neg && sp.z && zeroDigits(body) {
		neg = false
	}
	digits, rest := body, ""
	if i := strings.IndexAny(body, ".eE"); i >= 0 {
		digits, rest = body[:i], body[i:]
	}
	return number(neg, "", digits, rest+suffix, 3, sp), nil
}

func floatBody(x float64, sp spec) string {
	prec := sp.prec
	switch sp.typ {
	case 'f', 'F', '%':
		if prec < 0 {
			prec = 6
		}
		if sp.typ == '%' {
			x *= 100
		}
		out := strconv.FormatFloat(x, 'f', prec, 64)
		if sp.alt && prec == 0 {
			out += "."
		}
		return out
	case 'e', 'E':
		if prec < 0 {
			prec = 6
		}
		out := strconv.FormatFloat(x, 'e', prec, 64)
		if sp.alt && prec == 0 {
			i := strings.IndexByte(out, 'e')
			out = out[:i] + "." + out[i:]
		}
		return out
	case 'g', 'G', 'n':
		if prec < 0 {
			prec = 6
		}
		return general(x, prec, sp.alt, false)
	}
	if prec < 0 {
		out := shortest(x)
		if sp.alt && !strings.ContainsRune(out, '.') {
			if i := strings.IndexByte(out, 'e'); i >= 0 {
				out = out[:i] + "." + out[i:]
			} else {
				out += "."
			}
		}
		return out
	}
	return general(x, prec, sp.alt, true)
}

func exponentOf(s string) int {
	i := strings.IndexByte(s, 'e')
	n, _ := strconv.Atoi(s[i+1:])
	return n
}

func general(x float64, prec int, alt, dot0 bool) string {
	if prec == 0 {
		prec = 1
	}
	sci := strconv.FormatFloat(x, 'e', prec-1, 64)
	exp := exponentOf(sci)
	limit := prec
	if dot0 {
		limit = prec - 1
	}
	if exp < -4 || exp >= limit {
		i := strings.IndexByte(sci, 'e')
		mant, tail := sci[:i], sci[i:]
		if !alt {
			mant = trimZeros(mant)
		} else if !strings.ContainsRune(mant, '.') {
			mant += "."
		}
		return mant + tail
	}
	out := strconv.FormatFloat(x, 'f', prec-1-exp, 64)
	if !alt {
		out = trimZeros(out)
	} else if !strings.ContainsRune(out, '.') {
		out += "."
	}
	if dot0 && !strings.ContainsRune(out, '.') {
		out += ".0"
	}
	return out
}

func trimZeros(s string) string {
	if !strings.ContainsRune(s, '.') {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func shortest(x float64) string {
	sci := strconv.FormatFloat(x, 'e', -1, 64)
	exp := exponentOf(sci)
	if exp < -4 || exp >= 16 {
		return sci
	}
	out := strconv.FormatFloat(x, 'f', -1, 64)
	if !strings.ContainsRune(out, '.') {
		out += ".0"
	}
	return out
}

func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if math.Signbit(f) {
		return "-" + shortest(-f)
	}
	return shortest(f)
}

func zeroDigits(s string) bool {
	for _, r := range s {
		if r >= '1' && r <= '9' {
			return false
		}
		if r == 'e' || r == 'E' {
			break
		}
	}
	return true
}

func number(neg bool, prefix, digits, rest string, size int, sp spec) string {
	sign := ""
	switch {
	case neg:
		sign = "-"
	case sp.sign == '+':
		sign = "+"
	case sp.sign == ' ':
		sign = " "
	}
	minWidth := 0
	if sp.group != 0 && sp.fill == '0' && sp.align == '=' {
		minWidth = sp.width - len(sign) - len(prefix) - utf8.RuneCountInString(rest)
	}
	if sp.group != 0 && digits != "" {
		digits = group(digits, byte(sp.group), size, minWidth)
	}
	return pad(sign+prefix, digits+rest, sp)
}

func group(digits string, sep byte, size, minWidth int) string {
	out := make([]byte, 0, len(digits)+len(digits)/size+minWidth)
	count := 0
	for i := len(digits) - 1; i >= 0 || len(out) < minWidth; i-- {
		ch := byte('0')
		if i >= 0 {
			ch = digits[i]
		}
		if count == size {
			out = append(out, sep)
			count = 0
		}
		out = append(out, ch)
		count++
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return string(out)
}

func pad(lead, body string, sp spec) string {
	n := utf8.RuneCountInString(lead) + utf8.RuneCountInString(body)
	if sp.width <= n {
		return lead + body
	}
	fill := strings.Repeat(string(sp.fill), sp.width-n)
	switch sp.align {
	case '<':
		return lead + body + fill
	case '=':
		return lead + fill + body
	case '^':
		left := (sp.width - n) / 2
		return strings.Repeat(string(sp.fill), left) + lead + body + strings.Repeat(string(sp.fill), sp.width-n-left)
	}
	return fill + lead + body
}

func quote(s string, ascii bool) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x80:
			b.WriteRune(r)
		case !ascii && unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
