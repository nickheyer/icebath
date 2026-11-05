package pyrepr

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Python format failure tagged with the Python exception class it raises
type FormatError struct {
	Class string
	Msg   string
}

func (e *FormatError) Error() string { return e.Class + ": " + e.Msg }

func fmtErr(class, format string, args ...any) error {
	return &FormatError{Class: class, Msg: fmt.Sprintf(format, args...)}
}

// Decimal digits and exponent of a float rounded to prec significant digits, shortest when prec is negative
func floatDigits(f float64, prec int) (string, int, bool) {
	neg := math.Signbit(f)
	if neg {
		f = -f
	}
	p := prec - 1
	if prec < 0 {
		p = -1
	}
	s := strconv.FormatFloat(f, 'e', p, 64)
	ePos := strings.IndexByte(s, 'e')
	mant, expPart := s[:ePos], s[ePos+1:]
	exp, _ := strconv.Atoi(expPart)
	return strings.Replace(mant, ".", "", 1), exp, neg
}

func expSuffix(exp int, e byte) string {
	sign := "+"
	if exp < 0 {
		sign, exp = "-", -exp
	}
	return fmt.Sprintf("%c%s%02d", e, sign, exp)
}

// Fixed notation from significant digits, padding to frac decimals or keeping a trailing zero
func fixedFromDigits(digits string, exp, frac int, keepPoint bool) string {
	var intPart, fracPart string
	if exp >= 0 {
		if len(digits) > exp+1 {
			intPart, fracPart = digits[:exp+1], digits[exp+1:]
		} else {
			intPart = digits + strings.Repeat("0", exp+1-len(digits))
		}
	} else {
		intPart, fracPart = "0", strings.Repeat("0", -exp-1)+digits
	}
	if frac >= 0 {
		for len(fracPart) < frac {
			fracPart += "0"
		}
	}
	if fracPart == "" {
		if keepPoint {
			return intPart + ".0"
		}
		return intPart
	}
	return intPart + "." + fracPart
}

type formatSpec struct {
	fill      rune
	align     rune
	sign      rune
	alt       bool
	zero      bool
	width     int
	grouping  rune
	precision int
	typ       rune
	negZero   bool
}

// Parses the format spec mini language
func parseSpec(spec, typeName string) (formatSpec, error) {
	fs := formatSpec{fill: ' ', precision: -1}
	rs := []rune(spec)
	i := 0
	isAlign := func(r rune) bool { return r == '<' || r == '>' || r == '^' || r == '=' }
	if len(rs) >= 2 && isAlign(rs[1]) {
		fs.fill, fs.align, i = rs[0], rs[1], 2
	} else if len(rs) >= 1 && isAlign(rs[0]) {
		fs.align, i = rs[0], 1
	}
	if i < len(rs) && (rs[i] == '+' || rs[i] == '-' || rs[i] == ' ') {
		fs.sign = rs[i]
		i++
	}
	if i < len(rs) && rs[i] == 'z' {
		fs.negZero = true
		i++
	}
	if i < len(rs) && rs[i] == '#' {
		fs.alt = true
		i++
	}
	if i < len(rs) && rs[i] == '0' {
		fs.zero = true
		i++
	}
	start := i
	for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
		i++
	}
	if i > start {
		fs.width, _ = strconv.Atoi(string(rs[start:i]))
	}
	if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
		fs.grouping = rs[i]
		i++
		if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
			return fs, fmtErr("ValueError", "Cannot specify both ',' and '_'.")
		}
	}
	if i < len(rs) && rs[i] == '.' {
		i++
		start = i
		for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
			i++
		}
		if i == start {
			return fs, fmtErr("ValueError", "Format specifier missing precision")
		}
		fs.precision, _ = strconv.Atoi(string(rs[start:i]))
	}
	if len(rs)-i > 1 {
		return fs, fmtErr("ValueError", "Invalid format specifier '%s' for object of type '%s'", spec, typeName)
	}
	if i < len(rs) {
		fs.typ = rs[i]
	}
	if fs.zero && fs.align == 0 {
		fs.fill, fs.align = '0', '='
	}
	return fs, nil
}

// Python format(value, spec) for str, int, bool, float and objects that only accept an empty spec
func Format(value any, spec string) (string, error) {
	if spec == "" {
		return Str(value), nil
	}
	switch v := value.(type) {
	case string:
		return formatString(v, spec)
	case bool:
		n := int64(0)
		if v {
			n = 1
		}
		return formatInt(big.NewInt(n), spec)
	case float32:
		return formatFloat(float64(v), spec)
	case float64:
		return formatFloat(v, spec)
	}
	if n, ok := Int(value); ok {
		return formatInt(n, spec)
	}
	return "", fmtErr("TypeError", "unsupported format string passed to %s.__format__", TypeName(value))
}

func formatString(s, spec string) (string, error) {
	fs, err := parseSpec(spec, "str")
	if err != nil {
		return "", err
	}
	if fs.typ != 0 && fs.typ != 's' {
		return "", fmtErr("ValueError", "Unknown format code '%c' for object of type 'str'", fs.typ)
	}
	if fs.sign != 0 {
		return "", fmtErr("ValueError", "Sign not allowed in string format specifier")
	}
	if fs.negZero {
		return "", fmtErr("ValueError", "Negative zero coercion (z) not allowed in string format specifier")
	}
	if fs.alt {
		return "", fmtErr("ValueError", "Alternate form (#) not allowed in string format specifier")
	}
	if fs.grouping != 0 {
		return "", fmtErr("ValueError", "Cannot specify '%c' with 's'.", fs.grouping)
	}
	if fs.align == '=' && !(fs.zero && fs.fill == '0' && !strings.ContainsAny(spec, "=")) {
		return "", fmtErr("ValueError", "'=' alignment not allowed in string format specifier")
	}
	if fs.precision >= 0 {
		rs := []rune(s)
		if len(rs) > fs.precision {
			s = string(rs[:fs.precision])
		}
	}
	align := fs.align
	if align == 0 || align == '=' {
		align = '<'
	}
	return pad(s, fs.fill, align, fs.width), nil
}

// Pads text to width code points with fill on the aligned side
func pad(s string, fill, align rune, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	gap := width - n
	f := string(fill)
	switch align {
	case '>':
		return strings.Repeat(f, gap) + s
	case '^':
		left := gap / 2
		return strings.Repeat(f, left) + s + strings.Repeat(f, gap-left)
	}
	return s + strings.Repeat(f, gap)
}

// Inserts a separator every size digits from the right
func groupDigits(digits string, sep rune, size int) string {
	if sep == 0 || len(digits) <= size {
		return digits
	}
	var parts []string
	for len(digits) > size {
		parts = append([]string{digits[len(digits)-size:]}, parts...)
		digits = digits[:len(digits)-size]
	}
	parts = append([]string{digits}, parts...)
	return strings.Join(parts, string(sep))
}

// Joins sign, prefix and body applying alignment, with sign aware zero padding for the equals form
func numberLayout(fs formatSpec, neg bool, prefix, body string, group func(string) string, intLen int) string {
	sign := ""
	switch {
	case neg:
		sign = "-"
	case fs.sign == '+':
		sign = "+"
	case fs.sign == ' ':
		sign = " "
	}
	align := fs.align
	if align == 0 {
		align = '>'
	}
	if align == '=' {
		intPart, rest := body[:intLen], body[intLen:]
		target := fs.width - utf8.RuneCountInString(sign+prefix+rest)
		grouped := group(intPart)
		if fs.fill == '0' && fs.grouping != 0 {
			for utf8.RuneCountInString(grouped) < target {
				intPart = "0" + intPart
				grouped = group(intPart)
			}
			return sign + prefix + grouped + rest
		}
		head := sign + prefix
		full := grouped + rest
		gap := fs.width - utf8.RuneCountInString(head+full)
		if gap > 0 {
			return head + strings.Repeat(string(fs.fill), gap) + full
		}
		return head + full
	}
	text := sign + prefix + group(body[:intLen]) + body[intLen:]
	return pad(text, fs.fill, align, fs.width)
}

func formatInt(n *big.Int, spec string) (string, error) {
	fs, err := parseSpec(spec, "int")
	if err != nil {
		return "", err
	}
	switch fs.typ {
	case 'e', 'E', 'f', 'F', 'g', 'G', '%':
		f, _ := new(big.Float).SetInt(n).Float64()
		return formatFloat(f, spec)
	case 0, 'd', 'n', 'b', 'o', 'x', 'X', 'c':
	default:
		return "", fmtErr("ValueError", "Unknown format code '%c' for object of type 'int'", fs.typ)
	}
	if fs.precision >= 0 {
		return "", fmtErr("ValueError", "Precision not allowed in integer format specifier")
	}
	if fs.negZero {
		return "", fmtErr("ValueError", "Negative zero coercion (z) not allowed in integer format specifier")
	}
	if fs.typ == 'c' {
		if fs.sign != 0 {
			return "", fmtErr("ValueError", "Sign not allowed with integer format specifier 'c'")
		}
		if !n.IsInt64() || n.Int64() < 0 || n.Int64() > 0x10ffff {
			return "", fmtErr("OverflowError", "%%c arg not in range(0x110000)")
		}
		return pad(string(rune(n.Int64())), fs.fill, alignOr(fs.align, '>'), fs.width), nil
	}
	if fs.typ == 'n' && fs.grouping != 0 {
		return "", fmtErr("ValueError", "Cannot specify ',' with 'n'.")
	}
	abs := new(big.Int).Abs(n)
	base, prefix, size := 10, "", 3
	switch fs.typ {
	case 'b':
		base, prefix, size = 2, "0b", 4
	case 'o':
		base, prefix, size = 8, "0o", 4
	case 'x', 'X':
		base, prefix, size = 16, "0x", 4
	}
	if fs.grouping == ',' && base != 10 {
		return "", fmtErr("ValueError", "Cannot specify ',' with '%c'.", fs.typ)
	}
	digits := abs.Text(base)
	if fs.typ == 'X' {
		digits = strings.ToUpper(digits)
		prefix = "0X"
	}
	if !fs.alt {
		prefix = ""
	}
	group := func(s string) string { return groupDigits(s, fs.grouping, size) }
	return numberLayout(fs, n.Sign() < 0, prefix, digits, group, len(digits)), nil
}

func alignOr(a, def rune) rune {
	if a == 0 {
		return def
	}
	return a
}

func formatFloat(f float64, spec string) (string, error) {
	fs, err := parseSpec(spec, "float")
	if err != nil {
		return "", err
	}
	switch fs.typ {
	case 0, 'e', 'E', 'f', 'F', 'g', 'G', 'n', '%':
	default:
		return "", fmtErr("ValueError", "Unknown format code '%c' for object of type 'float'", fs.typ)
	}
	if fs.typ == 'n' && fs.grouping != 0 {
		return "", fmtErr("ValueError", "Cannot specify ',' with 'n'.")
	}
	neg := math.Signbit(f) && !math.IsNaN(f)
	a := math.Abs(f)
	upper := fs.typ == 'E' || fs.typ == 'F' || fs.typ == 'G'
	var body string
	switch {
	case math.IsInf(a, 0):
		body = "inf"
	case math.IsNaN(a):
		body = "nan"
	default:
		body = floatBody(a, fs)
	}
	if upper {
		body = strings.ToUpper(body)
	}
	if fs.negZero && roundsToZero(body) {
		neg = false
	}
	intLen := 0
	for intLen < len(body) && body[intLen] >= '0' && body[intLen] <= '9' {
		intLen++
	}
	group := func(s string) string { return groupDigits(s, fs.grouping, 3) }
	return numberLayout(fs, neg, "", body, group, intLen), nil
}

// Unsigned float body for one presentation type
func floatBody(a float64, fs formatSpec) string {
	prec := fs.precision
	switch fs.typ {
	case 'e', 'E':
		if prec < 0 {
			prec = 6
		}
		digits, exp, _ := floatDigits(a, prec+1)
		mant := digits[:1]
		if prec > 0 || fs.alt {
			mant += "." + digits[1:]
		}
		return mant + expSuffix(exp, 'e')
	case 'f', 'F':
		if prec < 0 {
			prec = 6
		}
		s := strconv.FormatFloat(a, 'f', prec, 64)
		if prec == 0 && fs.alt {
			s += "."
		}
		return s
	case '%':
		if prec < 0 {
			prec = 6
		}
		s := strconv.FormatFloat(a*100, 'f', prec, 64)
		if prec == 0 && fs.alt {
			s += "."
		}
		return s + "%"
	case 'g', 'G', 'n':
		if prec < 0 {
			prec = 6
		}
		return generalFloat(a, prec, fs.alt, false)
	}
	if prec < 0 {
		return Float(a)
	}
	return generalFloat(a, prec, fs.alt, true)
}

// General format with Python's switch between fixed and exponent, trimming zeros unless alternate
func generalFloat(a float64, prec int, alt, keepPoint bool) string {
	if prec == 0 {
		prec = 1
	}
	if a == 0 {
		switch {
		case alt:
			return "0." + strings.Repeat("0", prec-1)
		case keepPoint:
			return "0.0"
		}
		return "0"
	}
	digits, exp, _ := floatDigits(a, prec)
	var out string
	if exp >= -4 && exp < prec {
		out = fixedFromDigits(digits, exp, prec-1-exp, false)
		if !alt && strings.Contains(out, ".") {
			out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
		}
		if keepPoint && !strings.Contains(out, ".") {
			out += ".0"
		}
		if alt && !strings.Contains(out, ".") {
			out += "."
		}
		return out
	}
	mant := digits[:1]
	if len(digits) > 1 {
		mant += "." + digits[1:]
	}
	if !alt && strings.Contains(mant, ".") {
		mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
	}
	if alt && !strings.Contains(mant, ".") {
		mant += "."
	}
	return mant + expSuffix(exp, 'e')
}

// Reports whether the mantissa of a formatted float body holds only zero digits
func roundsToZero(body string) bool {
	if end := strings.IndexAny(body, "eE%"); end >= 0 {
		body = body[:end]
	}
	return strings.Trim(body, "0.") == ""
}
