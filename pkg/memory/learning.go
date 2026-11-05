package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Usage record keys whose first parseable value dates a skill
var usageTimestampKeys = []string{"last_activity_at", "last_used_at", "last_viewed_at", "last_patched_at", "created_at"}

// One learned or bundled skill as the learning graph sees it
type SkillNode struct {
	Name     string
	Category string
	Source   string
	// Epoch seconds as int64, or an integral float64 beyond the int64 range, nil when unknown
	Timestamp any
	UseCount  int
	State     string
	CreatedBy any
	Pinned    bool
	Related   []string
}

// Skill tree the graph walks, an embedded tree when FS is set and a directory on disk otherwise
type SkillRoot struct {
	Source string
	Dir    string
	FS     fs.FS
}

// Top level frontmatter key or the icebath metadata copy, hermes metadata accepted as alias
func fmField(fm map[string]any, key string) any {
	if pyTruthy(fm[key]) {
		return fm[key]
	}
	meta, ok := fm["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	if own, ok := meta["icebath"].(map[string]any); ok {
		return own[key]
	}
	if alias, ok := meta["hermes"].(map[string]any); ok {
		return alias[key]
	}
	return nil
}

// Related skill names declared in frontmatter, a bracketed comma string split like a list
func relatedSkills(fm map[string]any) []string {
	raw := fmField(fm, "related_skills")
	var items []any
	switch v := raw.(type) {
	case string:
		for _, part := range strings.Split(strings.Trim(v, "[]"), ",") {
			items = append(items, part)
		}
	case []any:
		items = v
	default:
		return []string{}
	}
	out := []string{}
	for _, item := range items {
		if s := pyStrip(pyStr(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Whether a rune is a Unicode decimal digit that int and float accept
func isUnicodeDecimal(r rune) bool { return unicode.Is(unicode.Nd, r) }

// Digit value of a Unicode decimal digit, its offset from the start of its contiguous digit run
func unicodeDecimalValue(r rune) rune {
	zero := r
	for unicode.Is(unicode.Nd, zero-1) {
		zero--
	}
	return (r - zero) % 10
}

// Python float literal shape after Unicode digits are folded to ASCII
var pyFloatLiteralRe = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*)(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)

// Float value of text the way float parses it, false where float raises
func pyFloatString(text string) (float64, bool) {
	var b strings.Builder
	for _, r := range pyStrip(text) {
		if r > 0x7f && isUnicodeDecimal(r) {
			b.WriteRune('0' + unicodeDecimalValue(r))
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	switch strings.ToLower(strings.TrimLeft(s, "+-")) {
	case "inf", "infinity", "nan":
		if len(s)-len(strings.TrimLeft(s, "+-")) > 1 {
			return 0, false
		}
		lower := strings.ToLower(s)
		switch {
		case strings.HasSuffix(lower, "nan"):
			return math.NaN(), true
		case strings.HasPrefix(s, "-"):
			return math.Inf(-1), true
		default:
			return math.Inf(1), true
		}
	}
	if !pyFloatLiteralRe.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// Integer part of a float as int64, an integral float64 past the int64 range, nil for nan or infinity
func truncTimestamp(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	t := math.Trunc(f)
	if t >= -9.223372036854775808e18 && t < 9.223372036854775808e18 {
		return int64(t)
	}
	return t
}

// Epoch seconds from a number, numeric string, or ISO timestamp, nil otherwise
func toIntTimestamp(value any) any {
	if value == nil {
		return nil
	}
	s := pyStrip(pyStr(value))
	if s == "" {
		return nil
	}
	switch v := value.(type) {
	case bool:
		if v {
			return int64(1)
		}
		return int64(0)
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return truncTimestamp(v)
	case json.Number:
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return truncTimestamp(f)
		}
		return nil
	}
	if f, ok := pyFloatString(s); ok {
		if math.IsInf(f, 0) {
			return nil
		}
		if !math.IsNaN(f) {
			return truncTimestamp(f)
		}
	}
	t, err := pyFromISOFormat(s)
	if err != nil {
		return nil
	}
	return isoEpochSeconds(t)
}

// Seconds since the epoch truncated toward zero the way int of datetime timestamp does
func isoEpochSeconds(t time.Time) int64 {
	micros := t.Unix()*1_000_000 + int64(t.Nanosecond()/1000)
	return int64(math.Trunc(float64(micros) / 1e6))
}

// Error text of an ISO string fromisoformat refuses
func invalidISOFormat(s string) error {
	return fmt.Errorf("Invalid isoformat string: %s", pyRepr(s))
}

// Parses an ISO 8601 string with the rules of CPython datetime fromisoformat, naive results in UTC
func pyFromISOFormat(s string) (time.Time, error) {
	rs := []rune(s)
	if len(rs) < 7 {
		return time.Time{}, invalidISOFormat(s)
	}
	sep, ok := isoDatetimeSeparator(rs)
	if !ok {
		return time.Time{}, invalidISOFormat(s)
	}
	year, month, day, ok := isoParseDate(string(rs[:min(sep, len(rs))]))
	if !ok {
		return time.Time{}, invalidISOFormat(s)
	}
	hour, minute, second, micro := 0, 0, 0, 0
	offset := time.Duration(0)
	if len(rs) > sep {
		var offSeconds, offMicro int
		var aware bool
		hour, minute, second, micro, offSeconds, offMicro, aware, ok = isoParseTime(string(rs[sep+1:]))
		if !ok {
			return time.Time{}, invalidISOFormat(s)
		}
		if aware {
			offset = time.Duration(offSeconds)*time.Second + time.Duration(offMicro)*time.Microsecond
			if offset <= -24*time.Hour || offset >= 24*time.Hour {
				return time.Time{}, fmt.Errorf("offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not %s", pyTimedeltaRepr(offset))
			}
		}
	}
	if hour == 24 {
		if minute != 0 || second != 0 || micro != 0 {
			return time.Time{}, fmt.Errorf("minute, second, and microsecond must be 0 when hour is 24")
		}
		hour = 0
		day++
		if month >= 1 && month <= 12 && year >= 1 && day > daysInMonth(year, month) {
			day = 1
			month++
			if month > 12 {
				month = 1
				year++
			}
		}
	}
	if year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("year %d is out of range", year)
	}
	if month < 1 || month > 12 {
		return time.Time{}, fmt.Errorf("month must be in 1..12")
	}
	if day < 1 || day > daysInMonth(year, month) {
		return time.Time{}, fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", day, daysInMonth(year, month), month, year)
	}
	if hour > 23 {
		return time.Time{}, fmt.Errorf("hour must be in 0..23")
	}
	if minute > 59 {
		return time.Time{}, fmt.Errorf("minute must be in 0..59")
	}
	if second > 59 {
		return time.Time{}, fmt.Errorf("second must be in 0..59")
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC).Add(-offset), nil
}

// Python repr of a timedelta offset for the out of range message
func pyTimedeltaRepr(d time.Duration) string {
	micros := int64(d / time.Microsecond)
	days := micros / 86_400_000_000
	rem := micros % 86_400_000_000
	if rem < 0 {
		days--
		rem += 86_400_000_000
	}
	seconds, us := rem/1_000_000, rem%1_000_000
	var parts []string
	if days != 0 {
		parts = append(parts, fmt.Sprintf("days=%d", days))
	}
	if seconds != 0 {
		parts = append(parts, fmt.Sprintf("seconds=%d", seconds))
	}
	if us != 0 {
		parts = append(parts, fmt.Sprintf("microseconds=%d", us))
	}
	if len(parts) == 0 {
		return "datetime.timedelta(0)"
	}
	return "datetime.timedelta(" + strings.Join(parts, ", ") + ")"
}

// Days in a Gregorian month
func daysInMonth(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// Byte at i or the NUL a C string would read past its end
func cAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// Code point index of the date and time separator under the CPython disambiguation rules
func isoDatetimeSeparator(rs []rune) (int, bool) {
	n := len(rs)
	at := func(i int) rune {
		if i < n {
			return rs[i]
		}
		return 0
	}
	digit := func(r rune) bool { return r >= '0' && r <= '9' }
	if n == 7 {
		return 7, true
	}
	if at(4) == '-' {
		if at(5) == 'W' {
			if n < 8 {
				return 0, false
			}
			if n > 8 && rs[8] == '-' {
				if n == 9 {
					return 0, false
				}
				if n > 10 && digit(rs[10]) {
					return 8, true
				}
				return 10, true
			}
			return 8, true
		}
		return 10, true
	}
	if at(4) == 'W' {
		idx := 7
		for idx < n && digit(rs[idx]) {
			idx++
		}
		if idx < 9 {
			return idx, true
		}
		if idx%2 == 0 {
			return 7, true
		}
		return 8, true
	}
	return 8, true
}

// Reads n ASCII digits at p, false when any is missing
func isoDigits(s string, p, n int) (int, int, bool) {
	v := 0
	for i := 0; i < n; i++ {
		c := cAt(s, p+i)
		if !isASCIIDigit(c) {
			return 0, p, false
		}
		v = v*10 + int(c-'0')
	}
	return v, p + n, true
}

// Calendar date of a YYYY-MM-DD, YYYYMMDD, or ISO week string
func isoParseDate(d string) (int, int, int, bool) {
	year, p, ok := isoDigits(d, 0, 4)
	if !ok {
		return 0, 0, 0, false
	}
	sep := cAt(d, p) == '-'
	if sep {
		p++
	}
	if cAt(d, p) == 'W' {
		p++
		week, np, ok := isoDigits(d, p, 2)
		if !ok {
			return 0, 0, 0, false
		}
		p = np
		weekday := 1
		if p < len(d) {
			if sep {
				if cAt(d, p) != '-' {
					return 0, 0, 0, false
				}
				p++
			}
			if weekday, _, ok = isoDigits(d, p, 1); !ok {
				return 0, 0, 0, false
			}
		}
		return isoWeekToGregorian(year, week, weekday)
	}
	month, p, ok := isoDigits(d, p, 2)
	if !ok {
		return 0, 0, 0, false
	}
	if sep {
		if cAt(d, p) != '-' {
			return 0, 0, 0, false
		}
		p++
	}
	day, _, ok := isoDigits(d, p, 2)
	if !ok {
		return 0, 0, 0, false
	}
	return year, month, day, true
}

// Gregorian date of an ISO year, week, and weekday, false outside the calendar
func isoWeekToGregorian(year, week, weekday int) (int, int, int, bool) {
	if year < 1 || year > 9999 {
		return 0, 0, 0, false
	}
	if week < 1 || week > 52 {
		first := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday()
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if week != 53 || !(first == time.Thursday || (first == time.Wednesday && leap)) {
			return 0, 0, 0, false
		}
	}
	if weekday < 1 || weekday > 7 {
		return 0, 0, 0, false
	}
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
	d := monday.AddDate(0, 0, (week-1)*7+weekday-1)
	return d.Year(), int(d.Month()), d.Day(), true
}

// Clock fields of HH[:MM[:SS[.ffffff]]] or the compact form ending at end, status 1 when text trails
func isoClock(s string, end int) (h, m, sec, micro, status int) {
	vals := [3]int{}
	p := 0
	hasSep := true
	for i := 0; i < 3; i++ {
		v, np, ok := isoDigits(s, p, 2)
		if !ok {
			return 0, 0, 0, 0, -3
		}
		vals[i] = v
		p = np
		c := cAt(s, p)
		p++
		if i == 0 {
			hasSep = c == ':'
		}
		if c == '.' || c == ',' {
			if i < 2 {
				return 0, 0, 0, 0, -3
			}
			break
		}
		if p >= end {
			if c != 0 {
				return vals[0], vals[1], vals[2], 0, 1
			}
			return vals[0], vals[1], vals[2], 0, 0
		}
		if hasSep && c == ':' {
			continue
		}
		if !hasSep {
			p--
			continue
		}
		return 0, 0, 0, 0, -4
	}
	toParse := min(end-p, 6)
	if toParse <= 0 {
		return 0, 0, 0, 0, -3
	}
	frac, np, ok := isoDigits(s, p, toParse)
	if !ok {
		return 0, 0, 0, 0, -3
	}
	p = np
	for i := toParse; i < 6; i++ {
		frac *= 10
	}
	for isASCIIDigit(cAt(s, p)) {
		p++
	}
	if cAt(s, p) != 0 {
		return vals[0], vals[1], vals[2], frac, 1
	}
	return vals[0], vals[1], vals[2], frac, 0
}

// Time and optional offset after the separator, aware when an offset or Z is present
func isoParseTime(t string) (h, m, s, micro, offset, offsetMicro int, aware, ok bool) {
	tz := 0
	for tz < len(t) && t[tz] != 'Z' && t[tz] != '+' && t[tz] != '-' {
		tz++
	}
	h, m, s, micro, rv := isoClock(t, tz)
	if rv < 0 {
		return 0, 0, 0, 0, 0, 0, false, false
	}
	if tz == len(t) {
		return h, m, s, micro, 0, 0, false, rv == 0
	}
	if t[tz] == 'Z' {
		return h, m, s, micro, 0, 0, true, tz+1 == len(t)
	}
	sign := 1
	if t[tz] == '-' {
		sign = -1
	}
	rest := t[tz+1:]
	th, tm, ts, tmicro, trv := isoClock(rest, len(rest))
	if trv != 0 {
		return 0, 0, 0, 0, 0, 0, false, false
	}
	return h, m, s, micro, sign * (th*3600 + tm*60 + ts), sign * tmicro, true, true
}

// Python round to n decimals on the exact binary value, halves to even
func pyRound(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return r
}

// Skill names that root lists in walk order with their relative path parts and absolute parts
type skillFile struct {
	rel      []string
	abs      []string
	path     string
	readFile func() ([]byte, error)
	mtime    func() (time.Time, error)
}

// Every SKILL.md under a root, symlinked directories listed but never descended like rglob
func skillFiles(root SkillRoot) []skillFile {
	var out []skillFile
	if root.FS != nil {
		_ = fs.WalkDir(root.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			rel := strings.Split(p, "/")
			out = append(out, skillFile{
				rel:      rel,
				abs:      append([]string{"skills"}, rel...),
				path:     p,
				readFile: func() ([]byte, error) { return fs.ReadFile(root.FS, p) },
				mtime: func() (time.Time, error) {
					st, err := fs.Stat(root.FS, p)
					if err != nil {
						return time.Time{}, err
					}
					return st.ModTime(), nil
				},
			})
			return nil
		})
		return out
	}
	if _, err := os.Stat(root.Dir); err != nil {
		return nil
	}
	base, err := filepath.Abs(root.Dir)
	if err != nil {
		base = root.Dir
	}
	_ = filepath.WalkDir(root.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root.Dir {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		rel, err := filepath.Rel(root.Dir, p)
		if err != nil {
			return nil
		}
		relParts := strings.Split(filepath.ToSlash(rel), "/")
		abs := strings.Split(filepath.ToSlash(filepath.Join(base, rel)), "/")
		path := p
		out = append(out, skillFile{
			rel:      relParts,
			abs:      abs,
			path:     path,
			readFile: func() ([]byte, error) { return os.ReadFile(path) },
			mtime: func() (time.Time, error) {
				st, err := os.Stat(path)
				if err != nil {
					return time.Time{}, err
				}
				return st.ModTime(), nil
			},
		})
		return nil
	})
	return out
}

// Text decoded as utf-8-sig, the UnicodeDecodeError read_text raises for invalid bytes
func decodeUTF8Sig(data []byte) (string, error) {
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if err := pyUTF8DecodeError(data); err != nil {
		return "", err
	}
	return string(data), nil
}

func isContinuationByte(b byte) bool { return b >= 0x80 && b <= 0xBF }

// First UTF-8 decode failure described the way the CPython strict decoder words it, nil for valid input
func pyUTF8DecodeError(data []byte) error {
	n := len(data)
	for i := 0; i < n; {
		ch := data[i]
		if ch < 0x80 {
			i++
			continue
		}
		reason, span := "", 0
		rem := n - i
		switch {
		case ch < 0xC2:
			reason, span = "invalid start byte", 1
		case ch < 0xE0:
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+1]):
				reason, span = "invalid continuation byte", 1
			default:
				i += 2
				continue
			}
		case ch < 0xF0:
			bad1 := func(c2 byte) bool {
				return !isContinuationByte(c2) || (ch == 0xE0 && c2 < 0xA0) || (ch == 0xED && c2 >= 0xA0)
			}
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case bad1(data[i+1]):
				reason, span = "invalid continuation byte", 1
			case rem < 3:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+2]):
				reason, span = "invalid continuation byte", 2
			default:
				i += 3
				continue
			}
		case ch < 0xF5:
			bad1 := func(c2 byte) bool {
				return !isContinuationByte(c2) || (ch == 0xF0 && c2 < 0x90) || (ch == 0xF4 && c2 >= 0x90)
			}
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case bad1(data[i+1]):
				reason, span = "invalid continuation byte", 1
			case rem < 3:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+2]):
				reason, span = "invalid continuation byte", 2
			case rem < 4:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+3]):
				reason, span = "invalid continuation byte", 3
			default:
				i += 4
				continue
			}
		default:
			reason, span = "invalid start byte", 1
		}
		if span == 1 {
			return &pyError{class: "UnicodeDecodeError", msg: fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", ch, i, reason)}
		}
		return &pyError{class: "UnicodeDecodeError", msg: fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", i, i+span-1, reason)}
	}
	return nil
}

// Modification time as fractional epoch seconds like st_mtime
func mtimeSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// Skill nodes from each root in order, the first root to name a skill keeping it
func BuildSkillNodes(roots []SkillRoot, usage map[string]map[string]any, isExternal func(path string) bool) ([]*SkillNode, error) {
	var nodes []*SkillNode
	seen := map[string]bool{}
	for _, root := range roots {
		for _, f := range skillFiles(root) {
			excluded := false
			for _, part := range f.rel {
				if skills.ExcludedSkillDirs[part] {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			data, err := f.readFile()
			if err != nil {
				continue
			}
			text, err := decodeUTF8Sig(data)
			if err != nil {
				return nil, err
			}
			fm, _ := skills.ParseFrontmatter(runePrefix(text, 4000))
			if fm == nil {
				fm = map[string]any{}
			}
			name := pyStrOr(fm["name"], "")
			if !pyTruthy(fm["name"]) {
				name = f.abs[len(f.abs)-2]
			}
			name = pyStrip(name)
			if name == "" || seen[name] {
				continue
			}
			rec := usage[name]
			if rec == nil {
				rec = map[string]any{}
			}
			var usageTS any
			for _, key := range usageTimestampKeys {
				if ts := toIntTimestamp(rec[key]); ts != nil {
					usageTS = ts
					break
				}
			}
			source := root.Source
			if root.FS == nil && isExternal != nil && isExternal(f.path) {
				source = "external"
			}
			category := "general"
			if cat := fmField(fm, "category"); pyTruthy(cat) {
				category = pyStr(cat)
			} else if len(f.abs) >= 3 {
				category = f.abs[len(f.abs)-3]
			}
			timestamp := usageTS
			if !pyTruthy(usageTS) {
				mt, err := f.mtime()
				if err != nil {
					return nil, &pyError{class: "OSError", msg: osErrorText(err)}
				}
				timestamp = toIntTimestamp(mtimeSeconds(mt))
			}
			useCount := 0
			if raw, ok := rec["use_count"]; ok && pyTruthy(raw) {
				if useCount, err = pyIntOf(raw); err != nil {
					return nil, err
				}
			}
			state := "active"
			if raw, ok := rec["state"]; ok && pyTruthy(raw) {
				state = pyStr(raw)
			}
			seen[name] = true
			nodes = append(nodes, &SkillNode{
				Name: name, Category: category, Source: source, Timestamp: timestamp, UseCount: useCount,
				State: state, CreatedBy: rec["created_by"], Pinned: pyTruthy(rec["pinned"]), Related: relatedSkills(fm),
			})
		}
	}
	return nodes, nil
}

// Undirected related skill edges whose endpoints both exist, deduped in first seen order
func BuildEdges(nodes []*SkillNode) [][2]string {
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	var edges [][2]string
	seen := map[[2]string]bool{}
	for _, n := range nodes {
		for _, target := range n.Related {
			if !names[target] || target == n.Name {
				continue
			}
			edge := [2]string{min(n.Name, target), max(n.Name, target)}
			if !seen[edge] {
				seen[edge] = true
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

// Insertion ordered tally like collections Counter
type orderedCounter struct {
	keys   []string
	counts map[string]int
}

func newOrderedCounter() *orderedCounter { return &orderedCounter{counts: map[string]int{}} }

func (c *orderedCounter) add(key string, n int) {
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key] += n
}

func (c *orderedCounter) set(key string, n int) {
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key] = n
}

// Entries sorted by count descending, ties kept in insertion order
func (c *orderedCounter) mostCommon() []categoryCount {
	out := make([]categoryCount, len(c.keys))
	for i, k := range c.keys {
		out[i] = categoryCount{k, c.counts[k]}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

type categoryCount struct {
	category string
	count    int
}

// Graph density figures over skill nodes and their related edges
func DensityStats(nodes []*SkillNode, edges [][2]string) map[string]any {
	linked := map[string]bool{}
	for _, e := range edges {
		linked[e[0]], linked[e[1]] = true, true
	}
	cats := newOrderedCounter()
	agentCreated, used := 0, 0
	for _, n := range nodes {
		cats.add(n.Category, 1)
		if s, ok := n.CreatedBy.(string); ok && s == "agent" {
			agentCreated++
		}
		if n.UseCount > 0 {
			used++
		}
	}
	n := len(nodes)
	if n == 0 {
		n = 1
	}
	top := [][]any{}
	for i, cc := range cats.mostCommon() {
		if i == 8 {
			break
		}
		top = append(top, []any{cc.category, cc.count})
	}
	return map[string]any{
		"nodes": len(nodes), "related_edges": len(edges), "edges_per_node": pyRound(float64(len(edges))/float64(n), 3),
		"linked_nodes": len(linked), "isolated_pct": pyRound(100*float64(n-len(linked))/float64(n), 1), "categories": len(cats.keys),
		"agent_created": agentCreated, "used": used, "top_categories": top,
	}
}

// Short stable digest of a memory entry text carried in its node id
func MemoryFingerprint(entry string) string {
	sum := sha256.Sum256([]byte(pyStrip(entry)))
	return hex.EncodeToString(sum[:])[:12]
}

// Node id naming the memory source, the occurrence index, and the text fingerprint
func MemoryNodeID(card map[string]any, index int) string {
	return fmt.Sprintf("memory:%s:%d:%s", pyStr(card["source"]), index, pyStr(card["fingerprint"]))
}

// Python str.splitlines boundaries
func pySplitLines(s string) []string {
	var lines []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			lines = append(lines, string(rs[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		lines = append(lines, string(rs[start:]))
	}
	return lines
}

// MEMORY.md then USER.md entries of a home as cards, one per entry the store parses
func MemoryCards(h string) ([]map[string]any, error) {
	base := home.MemoriesDir(h)
	cards := []map[string]any{}
	for _, f := range []struct{ name, source string }{{"MEMORY.md", "memory"}, {"USER.md", "profile"}} {
		path := filepath.Join(base, f.name)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		fileTS := toIntTimestamp(mtimeSeconds(st.ModTime()))
		for idx, chunk := range ReadEntriesFile(path) {
			lines := pySplitLines(chunk)
			if len(lines) == 0 {
				return nil, &pyError{class: "IndexError", msg: "list index out of range"}
			}
			first := pyStrip(strings.TrimLeft(pyStrip(lines[0]), "# "))
			title := first
			if utf8.RuneCountInString(first) > 80 {
				title = runePrefix(first, 80) + "\u2026"
			}
			var ts any
			switch v := fileTS.(type) {
			case int64:
				ts = v + int64(idx)
			case float64:
				ts = v + float64(idx)
			}
			cards = append(cards, map[string]any{
				"source": f.source, "timestamp": ts, "title": title, "body": runePrefix(chunk, 1200),
				"fingerprint": MemoryFingerprint(chunk),
			})
		}
	}
	return cards, nil
}

// Lowercase ASCII word tokens of three or more characters
func tokenize(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range regexp.MustCompile(`[^a-z0-9]+`).Split(pyLower(text), -1) {
		if len(t) >= 3 {
			out[t] = true
		}
	}
	return out
}

// Top four lexically overlapping skills per memory card, a name hit weighing six
func memorySkillEdges(cards []map[string]any, nodes []*SkillNode) [][2]string {
	type meta struct {
		name, lower string
		tokens      map[string]bool
	}
	metas := make([]meta, len(nodes))
	for i, s := range nodes {
		metas[i] = meta{s.Name, pyLower(s.Name), tokenize(s.Name)}
	}
	var edges [][2]string
	for idx, card := range cards {
		text := pyLower(pyStrOr(card["title"], "") + "\n" + pyStrOr(card["body"], ""))
		textTokens := tokenize(text)
		type scored struct {
			score int
			name  string
		}
		var hits []scored
		for _, m := range metas {
			score := 0
			if strings.Contains(text, m.lower) {
				score = 6
			}
			for t := range m.tokens {
				if textTokens[t] {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, scored{score, m.name})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].name < hits[j].name
		})
		for i, h := range hits {
			if i == 4 {
				break
			}
			edges = append(edges, [2]string{MemoryNodeID(card, idx), h.name})
		}
	}
	return edges
}

// Graph worthy skills, agent created, taught through learn, or used, never an external mount
func hasLearningSignal(n *SkillNode) bool {
	if n.Source == "external" {
		return false
	}
	if s, ok := n.CreatedBy.(string); ok && (s == "agent" || s == "learn") {
		return true
	}
	return n.UseCount > 0
}

// Home and skills scope the learning views read, the context home override first
func learningScope(ctx context.Context, env *tools.Env) (string, *skills.Scope, error) {
	h := registryHome(ctx, env)
	cfg, err := registryConfig(ctx, env)
	if err != nil {
		return "", nil, err
	}
	scope := skills.ScopeFromEnv(env)
	scope.Home, scope.Config, scope.Secrets = h, cfg, config.NewSecrets(h)
	return h, scope, nil
}

// Learning panel payload of non bundled skills with learning signal plus memory entries as nodes
func BuildLearningGraph(ctx context.Context, env *tools.Env) (map[string]any, error) {
	h, scope, err := learningScope(ctx, env)
	if err != nil {
		return nil, err
	}
	roots := []SkillRoot{{Source: "base", FS: skills.BundledFS()}, {Source: "profile", Dir: home.SkillsDir(h)}}
	all, err := BuildSkillNodes(roots, LoadSkillUsage(h), scope.IsExternalSkillPath)
	if err != nil {
		return nil, err
	}
	var learned []*SkillNode
	for _, n := range all {
		if n.Source != "base" && hasLearningSignal(n) {
			learned = append(learned, n)
		}
	}
	skillEdges := BuildEdges(learned)
	cards, err := MemoryCards(h)
	if err != nil {
		return nil, err
	}
	memoryEdges := memorySkillEdges(cards, learned)
	clusters := newOrderedCounter()
	for _, n := range learned {
		clusters.add(n.Category, 1)
	}
	if len(cards) > 0 {
		clusters.set("memory", len(cards))
	}
	graphNodes := []map[string]any{}
	for _, n := range learned {
		graphNodes = append(graphNodes, map[string]any{
			"id": n.Name, "label": n.Name, "kind": "skill", "timestamp": n.Timestamp, "category": n.Category,
			"useCount": n.UseCount, "state": n.State, "createdBy": n.CreatedBy, "pinned": n.Pinned,
		})
	}
	for i, card := range cards {
		graphNodes = append(graphNodes, map[string]any{
			"id": MemoryNodeID(card, i), "label": card["title"], "kind": "memory", "memorySource": card["source"],
			"timestamp": card["timestamp"], "category": "memory", "useCount": 0, "state": "active",
			"createdBy": "memory", "pinned": false,
		})
	}
	edges := []map[string]any{}
	for _, e := range append(append([][2]string{}, skillEdges...), memoryEdges...) {
		edges = append(edges, map[string]any{"source": e[0], "target": e[1]})
	}
	clusterRows := []map[string]any{}
	for _, cc := range clusters.mostCommon() {
		clusterRows = append(clusterRows, map[string]any{"category": cc.category, "count": cc.count})
	}
	stats := DensityStats(learned, skillEdges)
	stats["memory_nodes"] = len(cards)
	stats["memory_skill_edges"] = len(memoryEdges)
	stats["learned_skills"] = len(learned)
	return map[string]any{"nodes": graphNodes, "edges": edges, "clusters": clusterRows, "memory": cards, "stats": stats}, nil
}

// Learning graph builds nothing at import, desktop and journey callers request it
func registerLearningGraph() {}
