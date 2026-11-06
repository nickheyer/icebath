package pydatetime

import (
	"fmt"
	"math"
	"math/big"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Error is a Python exception raised by a datetime helper, Kind names its class
type Error struct {
	Kind string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func valueError(format string, args ...any) error {
	return &Error{Kind: "ValueError", Msg: fmt.Sprintf(format, args...)}
}

var strptimeDirectives = map[rune]string{
	'd': `(?P<d>3[0-1]|[1-2]\d|0[1-9]|[1-9]| [1-9])`,
	'f': `(?P<f>[0-9]{1,6})`,
	'H': `(?P<H>2[0-3]|[0-1]\d|\d| \d)`,
	'm': `(?P<m>1[0-2]|0[1-9]|[1-9])`,
	'M': `(?P<M>[0-5]\d|\d)`,
	'S': `(?P<S>6[0-1]|[0-5]\d|\d)`,
	'Y': `(?P<Y>\d\d\d\d)`,
	'z': `(?P<z>[+-]\d\d:?[0-5]\d(:?[0-5]\d(\.\d{1,6})?)?|(?-i:Z))`,
}

// Python regex of a strptime format over the numeric directives Y m d H M S f z and percent literals
func strptimePattern(format string) (string, error) {
	var b strings.Builder
	rs := []rune(format)
	seen := map[rune]bool{}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '%':
			j := i + 1
			for j < len(rs) && strings.ContainsRune("-_0^#", rs[j]) {
				j++
			}
			for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
				j++
			}
			if j >= len(rs) {
				return "", valueError("stray %% in format '%s'", format)
			}
			if rs[j] == '%' {
				b.WriteRune('%')
			} else if pat, ok := strptimeDirectives[rs[j]]; ok && !seen[rs[j]] {
				seen[rs[j]] = true
				b.WriteString(pat)
			} else if ok {
				return "", valueError("redefinition of group name %s as group in format '%s'", pyrepr.Repr(string(rs[j])), format)
			} else {
				return "", valueError("'%s' is a bad directive in format '%s'", string(rs[j]), format)
			}
			i = j
		case pyreSpace(r):
			for i+1 < len(rs) && pyreSpace(rs[i+1]) {
				i++
			}
			b.WriteString(`\s+`)
		case strings.ContainsRune(`\.^$*+?(){}[]|`, r):
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\'':
			b.WriteString("['ʼ]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// Unicode whitespace as Python re matches it with backslash s
func pyreSpace(r rune) bool {
	return strings.ContainsRune(" \t\n\r\f\v\x1c\x1d\x1e\x1f\x85      　", r) || (r >= ' ' && r <= ' ')
}

// Parses s like CPython datetime.strptime for the numeric directives, naive results in UTC with aware false
func Strptime(s, format string) (time.Time, bool, error) {
	pattern, err := strptimePattern(format)
	if err != nil {
		return time.Time{}, false, err
	}
	full := `(?i)\A(?:` + pattern + `)`
	re, err := pyre.Compile(full)
	if err != nil {
		return time.Time{}, false, err
	}
	index, _ := pyre.GroupIndex(full)
	m, err := re.FindStringMatch(s)
	if err != nil || m == nil {
		return time.Time{}, false, valueError("time data %s does not match format %s", pyrepr.Repr(s), pyrepr.Repr(format))
	}
	if rs := []rune(s); m.RuneIndex+m.RuneLength != len(rs) {
		return time.Time{}, false, valueError("unconverted data remains: %s", string(rs[m.RuneIndex+m.RuneLength:]))
	}
	group := func(name string) (string, bool) {
		n, named := index[name]
		if !named {
			return "", false
		}
		g := m.GroupByNumber(n)
		if g == nil || len(g.Captures) == 0 {
			return "", false
		}
		return g.String(), true
	}
	num := func(name string, def int) int {
		text, ok := group(name)
		if !ok {
			return def
		}
		n, _ := pyint.Parse(text)
		return int(n.Int64())
	}
	year, month, day := num("Y", 1900), num("m", 1), num("d", 1)
	hour, minute, second := num("H", 0), num("M", 0), num("S", 0)
	micro := 0
	if f, ok := group("f"); ok {
		micro = num("f", 0) * int(math.Pow10(6-len(f)))
	}
	if day > daysInMonth(year, month) {
		return time.Time{}, false, valueError("day %d must be in range 1..%d for month %d in year %d", day, daysInMonth(year, month), month, year)
	}
	if year < 1 || year > 9999 {
		return time.Time{}, false, valueError("year %d is out of range", year)
	}
	if second > 59 {
		return time.Time{}, false, valueError("second must be in 0..59")
	}
	wall := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC)
	z, aware := group("z")
	if !aware {
		return wall, false, nil
	}
	offset, err := strptimeOffset(z)
	if err != nil {
		return time.Time{}, false, err
	}
	return wall.Add(-offset).In(time.FixedZone("", int(offset/time.Second))), true, nil
}

// UTC offset of a matched z directive, refusing inconsistent colons and offsets of a day or more
func strptimeOffset(raw string) (time.Duration, error) {
	if raw == "Z" {
		return 0, nil
	}
	z := []rune(raw)
	if z[3] == ':' {
		z = append(z[:3:3], z[4:]...)
	}
	if len(z) > 5 {
		if z[5] != ':' {
			return 0, valueError("Inconsistent use of : in %s", raw)
		}
		z = append(z[:5:5], z[6:]...)
	}
	part := func(lo, hi int) int64 {
		if lo >= len(z) {
			return 0
		}
		n, _ := pyint.Parse(string(z[lo:min(hi, len(z))]))
		return n.Int64()
	}
	gmtoff := part(1, 3)*3600 + part(3, 5)*60 + part(5, 7)
	var fraction int64
	if len(z) > 8 {
		rem := string(z[8:])
		n, _ := pyint.Parse(rem + strings.Repeat("0", 6-len(z[8:])))
		fraction = n.Int64()
	}
	offset := time.Duration(gmtoff)*time.Second + time.Duration(fraction)*time.Microsecond
	if z[0] == '-' {
		offset = -offset
	}
	if offset <= -24*time.Hour || offset >= 24*time.Hour {
		return 0, valueError("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s", TimedeltaRepr(offset))
	}
	return offset, nil
}

// Seconds of an aware datetime since the epoch, the exact microsecond count divided like total_seconds
func Timestamp(t time.Time) float64 {
	micros := new(big.Int).Mul(big.NewInt(t.Unix()), big.NewInt(1_000_000))
	micros.Add(micros, big.NewInt(int64(t.Nanosecond()/1000)))
	f, _ := new(big.Rat).SetFrac(micros, big.NewInt(1_000_000)).Float64()
	return f
}

// Last second Windows localtime_s accepts, 3000-12-31 23:59:59 UTC
const windowsMaxTime64 = 32535215999

// Whether Windows localtime_s accepts u, negative and post 3000 instants fail with EINVAL
func WindowsLocaltimeAccepts(u int64) bool { return u >= 0 && u <= windowsMaxTime64 }

// Whether this platform localtime accepts u, only Windows refuses any instant
func LocaltimeAccepts(u int64) bool { return runtime.GOOS != "windows" || WindowsLocaltimeAccepts(u) }

// Epoch second of a wall time in loc by the CPython local_to_seconds search, false when localtime refuses a probe
func LocalToSeconds(wall time.Time, loc *time.Location, fold int, accepts func(int64) bool) (int64, bool) {
	local := func(u int64) (int64, bool) {
		if accepts != nil && !accepts(u) {
			return 0, false
		}
		_, off := time.Unix(u, 0).In(loc).Zone()
		return u + int64(off), true
	}
	y, mo, d := wall.Date()
	t := time.Date(y, mo, d, wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC).Unix()
	lt, ok := local(t)
	if !ok {
		return 0, false
	}
	a := lt - t
	u1 := t - a
	t1, ok := local(u1)
	if !ok {
		return 0, false
	}
	var b int64
	if t1 == t {
		u2 := u1 - 86400
		if fold != 0 {
			u2 = u1 + 86400
		}
		lt, ok = local(u2)
		if !ok {
			return 0, false
		}
		b = lt - u2
		if a == b {
			return u1, true
		}
	} else {
		b = t1 - u1
	}
	u2 := t - b
	t2, ok := local(u2)
	if !ok {
		return 0, false
	}
	if t2 == t {
		return u2, true
	}
	if t1 == t {
		return u1, true
	}
	if fold != 0 {
		return min(u1, u2), true
	}
	return max(u1, u2), true
}

// Datetime of epoch seconds in loc like CPython fromtimestamp, accepts gating the localtime call of a local zone
func FromTimestampIn(ts float64, loc *time.Location, accepts func(int64) bool) (time.Time, error) {
	if math.IsNaN(ts) {
		return time.Time{}, valueError("Invalid value NaN (not a number)")
	}
	intpart, frac := math.Modf(ts)
	frac = math.RoundToEven(frac * 1e6)
	if frac >= 1e6 {
		frac -= 1e6
		intpart++
	} else if frac < 0 {
		frac += 1e6
		intpart--
	}
	if intpart < -9.223372036854775808e18 || intpart >= 9.223372036854775808e18 {
		return time.Time{}, &Error{Kind: "OverflowError", Msg: "timestamp out of range for platform time_t"}
	}
	u := int64(intpart)
	utc := time.Unix(u, int64(frac)*1000).UTC()
	if y := utc.Year(); y < 1 || y > 9999 {
		return time.Time{}, valueError("year %d is out of range", y)
	}
	if accepts != nil && !accepts(u) {
		return time.Time{}, &Error{Kind: "OSError", Msg: "[Errno 22] Invalid argument"}
	}
	out := utc.In(loc)
	if y := out.Year(); y < 1 || y > 9999 {
		return time.Time{}, &Error{Kind: "OverflowError", Msg: "date value out of range"}
	}
	return out, nil
}
