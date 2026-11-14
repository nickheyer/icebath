package builtin

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var errISO = errors.New("not an ISO 8601 date or datetime")

func parseBound(value any, now time.Time) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	text := strip(pyStr(value))
	if text == "" {
		return nil, nil
	}
	if n, unit, ok := relativeBound(text); ok {
		at := now.Unix() - n*unit
		return &at, nil
	}
	t, err := parseISO(text)
	if err != nil {
		repr := pyStr(value)
		if s, ok := value.(string); ok {
			repr = pyRepr(s)
		}
		return nil, fmt.Errorf("invalid time bound: %s (expected ISO date/datetime like "+
			"2026-07-01, or a relative duration like 7d, 24h, 2w)", repr)
	}
	at := t.Unix()
	if at < 0 && t.Nanosecond() > 0 {
		at++
	}
	return &at, nil
}

func relativeBound(text string) (int64, int64, bool) {
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, 0, false
	}
	digits := text[:i]
	rest := strings.TrimLeftFunc(text[i:], unicode.IsSpace)
	if len(rest) != 1 {
		return 0, 0, false
	}
	var unit int64
	switch rest[0] {
	case 'h', 'H':
		unit = 3600
	case 'd', 'D':
		unit = 86400
	case 'w', 'W':
		unit = 604800
	default:
		return 0, 0, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > (1<<62)/unit {
		n = (1 << 62) / unit
	}
	return n, unit, true
}

func parseISO(s string) (time.Time, error) {
	year, month, day, rest, err := isoDate(s)
	if err != nil {
		return time.Time{}, err
	}
	if rest == "" {
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), nil
	}
	_, size := firstRune(rest)
	clock := rest[size:]
	tzAt := strings.IndexAny(clock, "Z+-")
	loc := time.UTC
	if tzAt >= 0 {
		offset, err := isoOffset(clock[tzAt:])
		if err != nil {
			return time.Time{}, err
		}
		loc = time.FixedZone("", offset)
		clock = clock[:tzAt]
	}
	hour, minute, second, nanos, err := isoClock(clock)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, nanos, loc), nil
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

func num(s string) (int, bool) {
	if s == "" || !isDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func isoDate(s string) (int, int, int, string, error) {
	if len(s) < 7 {
		return 0, 0, 0, "", errISO
	}
	year, ok := num(s[:4])
	if !ok || year < 1 {
		return 0, 0, 0, "", errISO
	}
	switch {
	case s[4] == '-' && s[5] == 'W', s[4] == 'W':
		at := 5
		if s[4] == '-' {
			at = 6
		}
		if len(s) < at+2 {
			return 0, 0, 0, "", errISO
		}
		week, ok := num(s[at : at+2])
		if !ok {
			return 0, 0, 0, "", errISO
		}
		at += 2
		weekday := 1
		if s[4] == '-' && len(s) >= at+2 && s[at] == '-' && isDigits(s[at+1:at+2]) {
			weekday, _ = num(s[at+1 : at+2])
			at += 2
		} else if s[4] == 'W' && len(s) >= at+1 && isDigits(s[at:at+1]) {
			weekday, _ = num(s[at : at+1])
			at++
		}
		if weekday < 1 || weekday > 7 || week < 1 || week > isoWeeks(year) {
			return 0, 0, 0, "", errISO
		}
		jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
		offset := (int(jan4.Weekday()) + 6) % 7
		d := jan4.AddDate(0, 0, -offset+(week-1)*7+weekday-1)
		return d.Year(), int(d.Month()), d.Day(), s[at:], nil
	case len(s) >= 10 && s[4] == '-' && s[7] == '-':
		month, ok1 := num(s[5:7])
		day, ok2 := num(s[8:10])
		if !ok1 || !ok2 || !validDay(year, month, day) {
			return 0, 0, 0, "", errISO
		}
		return year, month, day, s[10:], nil
	case len(s) >= 8:
		month, ok1 := num(s[4:6])
		day, ok2 := num(s[6:8])
		if !ok1 || !ok2 || !validDay(year, month, day) {
			return 0, 0, 0, "", errISO
		}
		return year, month, day, s[8:], nil
	}
	return 0, 0, 0, "", errISO
}

func validDay(year, month, day int) bool {
	if month < 1 || month > 12 || day < 1 {
		return false
	}
	return day <= time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func isoWeeks(year int) int {
	dec28 := time.Date(year, time.December, 28, 0, 0, 0, 0, time.UTC)
	_, week := dec28.ISOWeek()
	return week
}

func isoClock(s string) (int, int, int, int, error) {
	frac := ""
	if at := strings.IndexAny(s, ".,"); at >= 0 {
		frac = s[at+1:]
		s = s[:at]
		if frac == "" || !isDigits(frac) {
			return 0, 0, 0, 0, errISO
		}
	}
	var parts []string
	if strings.Contains(s, ":") {
		parts = strings.Split(s, ":")
	} else {
		if len(s)%2 != 0 {
			return 0, 0, 0, 0, errISO
		}
		for i := 0; i < len(s); i += 2 {
			parts = append(parts, s[i:i+2])
		}
	}
	if len(parts) == 0 || len(parts) > 3 || frac != "" && len(parts) != 3 {
		return 0, 0, 0, 0, errISO
	}
	values := [3]int{}
	for i, p := range parts {
		if len(p) != 2 {
			return 0, 0, 0, 0, errISO
		}
		v, ok := num(p)
		if !ok {
			return 0, 0, 0, 0, errISO
		}
		values[i] = v
	}
	if values[0] > 23 || values[1] > 59 || values[2] > 59 {
		return 0, 0, 0, 0, errISO
	}
	nanos := 0
	if frac != "" {
		if len(frac) > 6 {
			frac = frac[:6]
		}
		micro, _ := strconv.Atoi(frac + strings.Repeat("0", 6-len(frac)))
		nanos = micro * 1000
	}
	return values[0], values[1], values[2], nanos, nil
}

func isoOffset(s string) (int, error) {
	if s == "Z" {
		return 0, nil
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	h, m, sec, _, err := isoClock(s[1:])
	if err != nil {
		return 0, err
	}
	return sign * (h*3600 + m*60 + sec), nil
}

func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7f && !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
