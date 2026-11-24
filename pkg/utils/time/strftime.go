package timeutil

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Widest field a conversion pads to, so a width typed into a format cannot ask for unbounded output
const maxStrftimeWidth = 1024

// Text of t laid out by a C strftime format in the C locale: the glibc conversions with their _ - 0 ^ # flags, widths and E or O modifiers, plus %f for microseconds and %:z for a colon offset; an unknown conversion stays as written
func Strftime(t time.Time, format string) string {
	return string(appendStrftime(make([]byte, 0, len(format)+32), t, format))
}

func appendStrftime(b []byte, t time.Time, format string) []byte {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b = append(b, format[i])
			continue
		}
		start, j := i, i+1
		var flag byte
		for j < len(format) && strings.IndexByte("_-0^#", format[j]) >= 0 {
			flag = format[j]
			j++
		}
		width := -1
		for j < len(format) && '0' <= format[j] && format[j] <= '9' {
			width = min(max(width, 0)*10+int(format[j]-'0'), maxStrftimeWidth)
			j++
		}
		for j < len(format) && (format[j] == 'E' || format[j] == 'O') {
			j++
		}
		if j+1 < len(format) && format[j] == ':' && format[j+1] == 'z' {
			_, offset := t.Zone()
			b = appendText(b, formatOffset(offset, ":"), flag, width)
			i = j + 1
			continue
		}
		if j >= len(format) {
			return append(b, format[start:]...)
		}
		if out, ok := appendConversion(b, t, format[j], flag, width); ok {
			b = out
		} else {
			b = append(b, format[start:j+1]...)
		}
		i = j
	}
	return b
}

var (
	shortDays   = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	longDays    = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	shortMonths = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	longMonths  = [...]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
)

// One conversion appended with its flag and width, false for a conversion character glibc leaves as written
func appendConversion(b []byte, t time.Time, c, flag byte, width int) ([]byte, bool) {
	num := func(v, digits int, pad byte) []byte { return appendNumber(b, v, digits, pad, flag, width) }
	name := func(s string) []byte {
		if flag == '^' || flag == '#' {
			s = strings.ToUpper(s)
		}
		return appendText(b, s, flag, width)
	}
	hour12 := t.Hour() % 12
	if hour12 == 0 {
		hour12 = 12
	}
	switch c {
	case 'a':
		return name(shortDays[t.Weekday()]), true
	case 'A':
		return name(longDays[t.Weekday()]), true
	case 'b', 'h':
		return name(shortMonths[t.Month()-1]), true
	case 'B':
		return name(longMonths[t.Month()-1]), true
	case 'c':
		return appendComposite(b, t, "%a %b %e %H:%M:%S %Y", flag, width), true
	case 'C':
		return num(floorDiv(t.Year(), 100), 2, '0'), true
	case 'd':
		return num(t.Day(), 2, '0'), true
	case 'D', 'x':
		return appendComposite(b, t, "%m/%d/%y", flag, width), true
	case 'e':
		return num(t.Day(), 2, ' '), true
	case 'f':
		return num(t.Nanosecond()/1000, 6, '0'), true
	case 'F':
		return appendComposite(b, t, "%Y-%m-%d", flag, width), true
	case 'G':
		year, _ := t.ISOWeek()
		return num(year, 1, '0'), true
	case 'g':
		year, _ := t.ISOWeek()
		return num(floorMod(year, 100), 2, '0'), true
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
		return appendText(b, "\n", flag, width), true
	case 't':
		return appendText(b, "\t", flag, width), true
	case 'p':
		ampm := "AM"
		if t.Hour() >= 12 {
			ampm = "PM"
		}
		if flag == '#' {
			ampm = strings.ToLower(ampm)
		}
		return appendText(b, ampm, flag, width), true
	case 'P':
		ampm := "am"
		if t.Hour() >= 12 {
			ampm = "pm"
		}
		if flag == '^' {
			ampm = strings.ToUpper(ampm)
		}
		return appendText(b, ampm, flag, width), true
	case 'r':
		return appendComposite(b, t, "%I:%M:%S %p", flag, width), true
	case 'R':
		return appendComposite(b, t, "%H:%M", flag, width), true
	case 's':
		return appendNumber64(b, t.Unix(), 1, '0', flag, width), true
	case 'S':
		return num(t.Second(), 2, '0'), true
	case 'T', 'X':
		return appendComposite(b, t, "%H:%M:%S", flag, width), true
	case 'u':
		weekday := int(t.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		return num(weekday, 1, '0'), true
	case 'U':
		return num((t.YearDay()+6-int(t.Weekday()))/7, 2, '0'), true
	case 'V':
		_, week := t.ISOWeek()
		return num(week, 2, '0'), true
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
		return appendText(b, formatOffset(offset, ""), flag, width), true
	case 'Z':
		zone := zoneName(t)
		switch flag {
		case '^':
			zone = strings.ToUpper(zone)
		case '#':
			zone = strings.ToLower(zone)
		}
		return appendText(b, zone, flag, width), true
	case '%':
		return appendText(b, "%", flag, width), true
	}
	return b, false
}

// Nested layout such as %c or %F, padded and upper cased as a whole
func appendComposite(b []byte, t time.Time, layout string, flag byte, width int) []byte {
	if width <= 0 && flag != '^' {
		return appendStrftime(b, t, layout)
	}
	s := string(appendStrftime(nil, t, layout))
	if flag == '^' {
		s = strings.ToUpper(s)
	}
	return appendText(b, s, flag, width)
}

// Zone abbreviation of t, UTC or UTC with its offset for an unnamed fixed zone
func zoneName(t time.Time) string {
	name, offset := t.Zone()
	switch {
	case name != "":
		return name
	case offset == 0:
		return "UTC"
	}
	return "UTC" + formatOffset(offset, ":")
}

// Signed hours and minutes of an offset in seconds joined by sep, seconds added only when there are some
func formatOffset(offset int, sep string) string {
	sign := byte('+')
	if offset < 0 {
		sign, offset = '-', -offset
	}
	b := make([]byte, 0, 10)
	b = append(b, sign)
	b = appendTwo(b, offset/3600)
	b = append(b, sep...)
	b = appendTwo(b, offset/60%60)
	if offset%60 != 0 {
		b = append(b, sep...)
		b = appendTwo(b, offset%60)
	}
	return string(b)
}

func appendTwo(b []byte, v int) []byte {
	if v < 10 {
		b = append(b, '0')
	}
	return strconv.AppendInt(b, int64(v), 10)
}

func appendNumber(b []byte, v, digits int, pad, flag byte, width int) []byte {
	return appendNumber64(b, int64(v), digits, pad, flag, width)
}

// Number padded to its default digits or the explicit width, spaces under the _ flag, zeros under 0 and none under -
func appendNumber64(b []byte, v int64, digits int, pad, flag byte, width int) []byte {
	switch flag {
	case '-':
		pad = 0
	case '_':
		pad = ' '
	case '0':
		pad = '0'
	}
	if width >= 0 {
		digits = width
	}
	neg := v < 0
	var tmp [20]byte
	var text []byte
	if neg {
		digits--
		text = strconv.AppendUint(tmp[:0], uint64(-(v+1))+1, 10)
	} else {
		text = strconv.AppendInt(tmp[:0], v, 10)
	}
	fill := 0
	if pad != 0 {
		fill = digits - len(text)
	}
	if pad == ' ' {
		for ; fill > 0; fill-- {
			b = append(b, ' ')
		}
	}
	if neg {
		b = append(b, '-')
	}
	for ; fill > 0; fill-- {
		b = append(b, '0')
	}
	return append(b, text...)
}

// Text right aligned to an explicit width with spaces, or zeros under the 0 flag, unpadded under -
func appendText(b []byte, s string, flag byte, width int) []byte {
	if n := utf8.RuneCountInString(s); flag != '-' && width > n {
		pad := byte(' ')
		if flag == '0' {
			pad = '0'
		}
		for range width - n {
			b = append(b, pad)
		}
	}
	return append(b, s...)
}

// Integer division rounding toward negative infinity
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// Remainder with the sign of the divisor
func floorMod(a, b int) int { return a - floorDiv(a, b)*b }
