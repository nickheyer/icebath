package pydatetime

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	shortDays   = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	longDays    = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	shortMonths = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	longMonths  = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
)

// Zone name datetime.tzname returns, UTC plus offset for an unnamed fixed zone
func TZName(t time.Time) string {
	name, offset := t.Zone()
	if name != "" {
		return name
	}
	if offset == 0 {
		return "UTC"
	}
	return "UTC" + formatOffset(offset, ":")
}

// Signed hours and minutes of an offset in seconds, seconds appended only when nonzero
func formatOffset(offset int, sep string) string {
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	out := fmt.Sprintf("%s%02d%s%02d", sign, offset/3600, sep, offset/60%60)
	if offset%60 != 0 {
		out += fmt.Sprintf("%s%02d", sep, offset%60)
	}
	return out
}

// Renders format the way datetime.strftime does on glibc in the C locale for an aware datetime
func Strftime(t time.Time, format string) string {
	var b strings.Builder
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '%' || i+1 >= len(rs) {
			b.WriteRune(rs[i])
			continue
		}
		start := i
		j := i + 1
		var flag rune
		for j < len(rs) && strings.ContainsRune("_-0^#", rs[j]) {
			flag = rs[j]
			j++
		}
		width := -1
		for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
			if width < 0 {
				width = 0
			}
			width = width*10 + int(rs[j]-'0')
			j++
		}
		if j < len(rs) && rs[j] == ':' && j+1 < len(rs) && rs[j+1] == 'z' && flag == 0 && width < 0 {
			_, offset := t.Zone()
			b.WriteString(formatOffset(offset, ":"))
			i = j + 1
			continue
		}
		if j >= len(rs) {
			b.WriteString(string(rs[start:]))
			break
		}
		out, ok := strftimeDirective(t, rs[j], flag, width)
		if !ok {
			b.WriteString(string(rs[start : j+1]))
		} else {
			b.WriteString(out)
		}
		i = j
	}
	return b.String()
}

// One conversion with its glibc flag and width, false for a directive glibc leaves literal
func strftimeDirective(t time.Time, c rune, flag rune, width int) (string, bool) {
	num := func(v, digits int, pad byte) string {
		return padNumber(v, digits, pad, flag, width)
	}
	text := func(s string, upperOnHash bool) string {
		switch {
		case flag == '^':
			s = strings.ToUpper(s)
		case flag == '#' && upperOnHash:
			s = strings.ToUpper(s)
		case flag == '#':
			s = strings.ToLower(s)
		}
		return padText(s, flag, width)
	}
	hour12 := t.Hour() % 12
	if hour12 == 0 {
		hour12 = 12
	}
	isoYear, isoWeek := t.ISOWeek()
	switch c {
	case 'a':
		return text(shortDays[t.Weekday()], true), true
	case 'A':
		return text(longDays[t.Weekday()], true), true
	case 'b', 'h':
		return text(shortMonths[t.Month()-1], true), true
	case 'B':
		return text(longMonths[t.Month()-1], true), true
	case 'c':
		return padText(Strftime(t, "%a %b %e %H:%M:%S %Y"), flag, width), true
	case 'C':
		return num(floorDiv(t.Year(), 100), 2, '0'), true
	case 'd':
		return num(t.Day(), 2, '0'), true
	case 'D', 'x':
		return padText(Strftime(t, "%m/%d/%y"), flag, width), true
	case 'e':
		return num(t.Day(), 2, ' '), true
	case 'f':
		return num(t.Nanosecond()/1000, 6, '0'), true
	case 'F':
		return padText(Strftime(t, "%Y-%m-%d"), flag, width), true
	case 'G':
		return num(isoYear, 1, '0'), true
	case 'g':
		return num(floorMod(isoYear, 100), 2, '0'), true
	case 'H':
		return num(t.Hour(), 2, '0'), true
	case 'I':
		return num(hour12, 2, '0'), true
	case 'j':
		return num(t.YearDay(), 3, '0'), true
	case 'k':
		return num(t.Hour(), 2, ' '), true
	case 'l':
		return num(hour12, 2, ' '), true
	case 'm':
		return num(int(t.Month()), 2, '0'), true
	case 'M':
		return num(t.Minute(), 2, '0'), true
	case 'n':
		return padText("\n", flag, width), true
	case 't':
		return padText("\t", flag, width), true
	case 'p':
		ampm := "AM"
		if t.Hour() >= 12 {
			ampm = "PM"
		}
		if flag == '#' {
			return padText(strings.ToLower(ampm), 0, width), true
		}
		return text(ampm, true), true
	case 'P':
		ampm := "am"
		if t.Hour() >= 12 {
			ampm = "pm"
		}
		return text(ampm, false), true
	case 'r':
		return padText(Strftime(t, "%I:%M:%S %p"), flag, width), true
	case 'R':
		return padText(Strftime(t, "%H:%M"), flag, width), true
	case 's':
		local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
		return num(int(local.Unix()), 1, '0'), true
	case 'S':
		return num(t.Second(), 2, '0'), true
	case 'T', 'X':
		return padText(Strftime(t, "%H:%M:%S"), flag, width), true
	case 'u':
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		return num(wd, 1, '0'), true
	case 'U':
		return num((t.YearDay()+6-int(t.Weekday()))/7, 2, '0'), true
	case 'V':
		return num(isoWeek, 2, '0'), true
	case 'w':
		return num(int(t.Weekday()), 1, '0'), true
	case 'W':
		return num((t.YearDay()+6-(int(t.Weekday())+6)%7)/7, 2, '0'), true
	case 'y':
		return num(floorMod(t.Year(), 100), 2, '0'), true
	case 'Y':
		return num(t.Year(), 1, '0'), true
	case 'z':
		_, offset := t.Zone()
		return padText(formatOffset(offset, ""), flag, width), true
	case 'Z':
		if flag == '^' {
			return padText(strings.ToUpper(TZName(t)), 0, width), true
		}
		return padText(TZName(t), flag, width), true
	case '%':
		return padText("%", flag, width), true
	}
	return "", false
}

// Number padded to digits or the explicit width with zero or space, unpadded under the minus flag
func padNumber(v, digits int, pad byte, flag rune, width int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := strconv.Itoa(v)
	switch flag {
	case '-':
		pad, digits = 0, 0
	case '_':
		pad = ' '
	case '0':
		pad = '0'
	}
	if width >= 0 {
		digits = width
	}
	if neg {
		digits--
	}
	if pad != 0 && len(s) < digits {
		s = strings.Repeat(string(pad), digits-len(s)) + s
	}
	if neg {
		if pad == ' ' {
			trimmed := strings.TrimLeft(s, " ")
			return s[:len(s)-len(trimmed)] + "-" + trimmed
		}
		return "-" + s
	}
	return s
}

// Text right aligned to an explicit width with spaces, or zeros under the zero flag
func padText(s string, flag rune, width int) string {
	n := len([]rune(s))
	if width <= n || flag == '-' {
		return s
	}
	pad := " "
	if flag == '0' {
		pad = "0"
	}
	return strings.Repeat(pad, width-n) + s
}

// Integer division rounding toward negative infinity
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Modulo with the sign of the divisor
func floorMod(a, b int) int { return a - floorDiv(a, b)*b }
