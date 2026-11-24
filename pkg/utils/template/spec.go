package templateutil

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// How one value is laid out: [[fill]align][width][,][.precision][f]
type spec struct {
	source    string
	fill      rune
	align     byte
	width     int
	group     bool
	fixed     bool
	precision int
}

const maxWidth = 1 << 16

// Spec parsed from its text: fill and align (<, > or ^), a minimum width in characters, a comma between thousands, a precision, and f for fixed point
func parseSpec(text string) (spec, error) {
	sp := spec{source: text, fill: ' ', precision: -1}
	s := text
	if s == "" {
		return sp, nil
	}
	if r, size := utf8.DecodeRuneInString(s); len(s) > size && isAlign(s[size]) {
		sp.fill, sp.align, s = r, s[size], s[size+1:]
	} else if isAlign(s[0]) {
		sp.align, s = s[0], s[1:]
	}
	digits := leadingDigits(s)
	if len(digits) > 1 && digits[0] == '0' {
		return sp, errors.New("width " + digits + " starts with 0; pad with zeros as 0>" + digits[1:])
	}
	if digits != "" {
		w, err := strconv.Atoi(digits)
		if err != nil || w > maxWidth {
			return sp, errors.New("width " + digits + " is too large")
		}
		sp.width, s = w, s[len(digits):]
	}
	if strings.HasPrefix(s, ",") {
		sp.group, s = true, s[1:]
	}
	if strings.HasPrefix(s, ".") {
		digits = leadingDigits(s[1:])
		if digits == "" {
			return sp, errors.New("precision has no digits")
		}
		p, err := strconv.Atoi(digits)
		if err != nil || p > maxWidth {
			return sp, errors.New("precision " + digits + " is too large")
		}
		sp.precision, s = p, s[1+len(digits):]
	}
	if s == "f" {
		if sp.precision < 0 {
			sp.precision = 6
		}
		sp.fixed, s = true, ""
	}
	if s != "" {
		return sp, errors.New("unknown spec " + strconv.Quote(text))
	}
	return sp, nil
}

func isAlign(c byte) bool { return c == '<' || c == '>' || c == '^' }

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// Text of v laid out under the spec: numbers grouped and fixed to the precision, text cut to the precision, both padded to the width
func (sp spec) apply(v any) (string, error) {
	if sp.source == "" {
		return valueText(v), nil
	}
	var text string
	align := sp.align
	if n, ok := anyutil.Int(v); ok {
		text = strconv.FormatInt(n, 10)
		if sp.precision > 0 {
			text += "." + strings.Repeat("0", sp.precision)
		}
	} else if f, ok := anyutil.Float(v); ok {
		if sp.precision >= 0 {
			text = strconv.FormatFloat(f, 'f', sp.precision, 64)
		} else {
			text = anyutil.String(v)
		}
	} else {
		if sp.group || sp.fixed {
			return "", errors.New("spec needs a number, got " + anyutil.Kind(v))
		}
		text = valueText(v)
		if sp.precision >= 0 {
			text = textutil.First(text, sp.precision)
		}
		if align == 0 {
			align = '<'
		}
		return pad(text, sp.fill, align, sp.width), nil
	}
	if sp.group {
		text = groupThousands(text)
	}
	if align == 0 {
		align = '>'
	}
	return pad(text, sp.fill, align, sp.width), nil
}

// Text of a value with no spec
func valueText(v any) string { return anyutil.String(v) }

// Number text with a comma between each group of three digits of its whole part
func groupThousands(num string) string {
	sign := ""
	if num != "" && (num[0] == '-' || num[0] == '+') {
		sign, num = num[:1], num[1:]
	}
	whole := leadingDigits(num)
	if len(whole) <= 3 {
		return sign + num
	}
	var b strings.Builder
	b.Grow(len(sign) + len(num) + len(whole)/3)
	b.WriteString(sign)
	head := len(whole) % 3
	if head == 0 {
		head = 3
	}
	b.WriteString(whole[:head])
	for i := head; i < len(whole); i += 3 {
		b.WriteByte(',')
		b.WriteString(whole[i : i+3])
	}
	b.WriteString(num[len(whole):])
	return b.String()
}

// Text padded with fill to width characters on the side align leaves open, split evenly with the extra on the right when centered
func pad(text string, fill rune, align byte, width int) string {
	gap := width - utf8.RuneCountInString(text)
	if gap <= 0 {
		return text
	}
	f := string(fill)
	switch align {
	case '>':
		return strings.Repeat(f, gap) + text
	case '^':
		left := gap / 2
		return strings.Repeat(f, left) + text + strings.Repeat(f, gap-left)
	}
	return text + strings.Repeat(f, gap)
}
