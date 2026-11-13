package types

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FormatTimestamp writes a UTC time exactly as chrono DateTime<Utc> to_rfc3339 does
func FormatTimestamp(t time.Time) string {
	return formatRFC3339(t, false, false)
}

// FormatTimestampZ writes a UTC time as chrono serde serialization does, with a Z offset
func FormatTimestampZ(t time.Time) string {
	return formatRFC3339(t, true, false)
}

// Writes year, date, time with AutoSi fraction and a UTC offset, optionally as a leap second
func formatRFC3339(t time.Time, useZ bool, leap bool) string {
	t = t.UTC()
	var b strings.Builder
	year := t.Year()
	if year >= 0 && year <= 9999 {
		fmt.Fprintf(&b, "%04d", year)
	} else {
		fmt.Fprintf(&b, "%+05d", year)
	}
	sec := t.Second()
	if leap {
		sec++
	}
	fmt.Fprintf(&b, "-%02d-%02dT%02d:%02d:%02d", int(t.Month()), t.Day(), t.Hour(), t.Minute(), sec)
	nano := t.Nanosecond()
	switch {
	case nano == 0:
	case nano%1_000_000 == 0:
		fmt.Fprintf(&b, ".%03d", nano/1_000_000)
	case nano%1_000 == 0:
		fmt.Fprintf(&b, ".%06d", nano/1_000)
	default:
		fmt.Fprintf(&b, ".%09d", nano)
	}
	if useZ {
		b.WriteByte('Z')
	} else {
		b.WriteString("+00:00")
	}
	return b.String()
}

// Timestamp parse failures carrying the chrono ParseError Display texts
var (
	errTimestampOutOfRange = errors.New("input is out of range")
	errTimestampImpossible = errors.New("no possible date and time matching input")
	errTimestampNotEnough  = errors.New("input is not enough for unique date and time")
	errTimestampInvalid    = errors.New("input contains invalid characters")
	errTimestampTooShort   = errors.New("premature end of input")
	errTimestampTooLong    = errors.New("trailing input")
)

// ParsedTimestamp is an RFC 3339 instant in UTC that remembers a leap second
type ParsedTimestamp struct {
	Time time.Time
	Leap bool
}

// ParseTimestamp parses RFC 3339 text as chrono parse_from_rfc3339 does and converts it to UTC
func ParseTimestamp(s string) (time.Time, error) {
	p, err := ParseTimestampLeap(s)
	return p.Time, err
}

// ParseTimestampLeap parses RFC 3339 text keeping a leap second marker the way chrono represents it
func ParseTimestampLeap(s string) (ParsedTimestamp, error) {
	year, s, err := scanNumber(s, 4, 4)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if s, err = scanChar(s, '-'); err != nil {
		return ParsedTimestamp{}, err
	}
	month, s, err := scanNumber(s, 2, 2)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if month < 1 || month > 12 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	if s, err = scanChar(s, '-'); err != nil {
		return ParsedTimestamp{}, err
	}
	day, s, err := scanNumber(s, 2, 2)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if day < 1 || day > 31 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	if s == "" {
		return ParsedTimestamp{}, errTimestampTooShort
	}
	switch s[0] {
	case 't', 'T', ' ':
		s = s[1:]
	default:
		return ParsedTimestamp{}, errTimestampInvalid
	}
	hour, s, err := scanNumber(s, 2, 2)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if hour > 23 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	if s, err = scanChar(s, ':'); err != nil {
		return ParsedTimestamp{}, err
	}
	minute, s, err := scanNumber(s, 2, 2)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if minute > 59 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	if s, err = scanChar(s, ':'); err != nil {
		return ParsedTimestamp{}, err
	}
	second, s, err := scanNumber(s, 2, 2)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	if second > 60 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	var nano int64
	if strings.HasPrefix(s, ".") {
		nano, s, err = scanNanosecond(s[1:])
		if err != nil {
			return ParsedTimestamp{}, err
		}
	}
	offset, s, err := scanOffset(s)
	if err != nil {
		return ParsedTimestamp{}, err
	}
	const maxOffset = (23*60 + 59) * 60
	if offset < -maxOffset || offset > maxOffset {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	if s != "" {
		return ParsedTimestamp{}, errTimestampTooLong
	}
	if year < -262143 || year > 262142 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	date := time.Date(int(year), time.Month(month), int(day), 0, 0, 0, 0, time.UTC)
	if date.Day() != int(day) || int(date.Month()) != int(month) {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	leap := second == 60
	if leap {
		second = 59
	}
	local := time.Date(int(year), time.Month(month), int(day), int(hour), int(minute), int(second), int(nano), time.UTC)
	utc := local.Add(-time.Duration(offset) * time.Second)
	if utc.Year() < -262143 || utc.Year() > 262142 {
		return ParsedTimestamp{}, errTimestampOutOfRange
	}
	return ParsedTimestamp{Time: utc, Leap: leap}, nil
}

func scanNumber(s string, minDigits, maxDigits int) (int64, string, error) {
	if len(s) < minDigits {
		return 0, s, errTimestampTooShort
	}
	var n int64
	for i := 0; i < maxDigits && i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			if i < minDigits {
				return 0, s, errTimestampInvalid
			}
			return n, s[i:], nil
		}
		n = n*10 + int64(c-'0')
	}
	if len(s) < maxDigits {
		return n, "", nil
	}
	return n, s[maxDigits:], nil
}

func scanChar(s string, c byte) (string, error) {
	if s == "" {
		return s, errTimestampTooShort
	}
	if s[0] != c {
		return s, errTimestampInvalid
	}
	return s[1:], nil
}

func scanNanosecond(s string) (int64, string, error) {
	orig := len(s)
	v, rest, err := scanNumber(s, 1, 9)
	if err != nil {
		return 0, s, err
	}
	consumed := orig - len(rest)
	scale := [10]int64{0, 100_000_000, 10_000_000, 1_000_000, 100_000, 10_000, 1_000, 100, 10, 1}
	v *= scale[consumed]
	rest = strings.TrimLeft(rest, "0123456789")
	return v, rest, nil
}

func scanOffset(s string) (int64, string, error) {
	if s != "" && (s[0] == 'Z' || s[0] == 'z') {
		return 0, s[1:], nil
	}
	negative := false
	switch {
	case s == "":
		return 0, s, errTimestampTooShort
	case s[0] == '+':
		s = s[1:]
	case s[0] == '-':
		s = s[1:]
		negative = true
	case strings.HasPrefix(s, "−"):
		s = s[len("−"):]
		negative = true
	default:
		return 0, s, errTimestampInvalid
	}
	if len(s) < 2 {
		return 0, s, errTimestampTooShort
	}
	h1, h2 := s[0], s[1]
	if h1 < '0' || h1 > '9' || h2 < '0' || h2 > '9' {
		return 0, s, errTimestampInvalid
	}
	hours := int64(h1-'0')*10 + int64(h2-'0')
	s = s[2:]
	var err error
	if s, err = scanChar(s, ':'); err != nil {
		return 0, s, err
	}
	if len(s) < 2 {
		return 0, s, errTimestampTooShort
	}
	m1, m2 := s[0], s[1]
	var minutes int64
	switch {
	case m1 >= '0' && m1 <= '5' && m2 >= '0' && m2 <= '9':
		minutes = int64(m1-'0')*10 + int64(m2-'0')
	case m1 >= '6' && m1 <= '9' && m2 >= '0' && m2 <= '9':
		return 0, s, errTimestampOutOfRange
	default:
		return 0, s, errTimestampInvalid
	}
	s = s[2:]
	seconds := hours*3600 + minutes*60
	if negative {
		seconds = -seconds
	}
	return seconds, s, nil
}

// TimestampFromMicros converts epoch microseconds as chrono from_timestamp_micros does
func TimestampFromMicros(micros int64) (time.Time, bool) {
	t := time.UnixMicro(micros).UTC()
	if t.Year() < -262143 || t.Year() > 262142 {
		return time.Time{}, false
	}
	return t, true
}

// ParseUUID parses a UUID with the uuid crate formats and error texts
func ParseUUID(s string) (uuid.UUID, error) {
	var body string
	switch {
	case len(s) == 32:
		return parseSimpleUUID(s)
	case len(s) == 36:
		body = s
	case len(s) == 38 && s[0] == '{' && s[37] == '}':
		body = s[1:37]
	case len(s) == 45 && strings.HasPrefix(s, "urn:uuid:"):
		body = s[9:]
	default:
		return uuid.UUID{}, uuidParseError(s)
	}
	if body[8] != '-' || body[13] != '-' || body[18] != '-' || body[23] != '-' {
		return uuid.UUID{}, uuidParseError(s)
	}
	hex := body[0:8] + body[9:13] + body[14:18] + body[19:23] + body[24:36]
	u, err := parseSimpleUUID(hex)
	if err != nil {
		return uuid.UUID{}, uuidParseError(s)
	}
	return u, nil
}

func parseSimpleUUID(s string) (uuid.UUID, error) {
	var u uuid.UUID
	for i := 0; i < 16; i++ {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return uuid.UUID{}, uuidParseError(s)
		}
		u[i] = hi<<4 | lo
	}
	return u, nil
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Builds the detailed uuid crate diagnostic for an unparseable input
func uuidParseError(input string) error {
	uuidStr, offset, simple := input, 0, true
	switch {
	case len(input) >= 2 && input[0] == '{' && input[len(input)-1] == '}':
		uuidStr, offset, simple = input[1:len(input)-1], 1, false
	case strings.HasPrefix(input, "urn:uuid:"):
		uuidStr, offset, simple = input[9:], 9, false
	}
	hyphens := 0
	var bounds [4]int
	for index, r := range uuidStr {
		if r >= 0x80 {
			return fmt.Errorf("invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `%c` at %d", r, index+offset+1)
		}
		c := byte(r)
		if c == '-' {
			if hyphens < 4 {
				bounds[hyphens] = index
			}
			hyphens++
		} else if _, ok := hexNibble(c); !ok {
			return fmt.Errorf("invalid character: expected an optional prefix of `urn:uuid:` followed by [0-9a-fA-F-], found `%c` at %d", c, index+offset+1)
		}
	}
	if hyphens == 0 && simple {
		return fmt.Errorf("invalid length: expected length 32 for simple format, found %d", len(input))
	}
	if hyphens != 4 {
		return fmt.Errorf("invalid group count: expected 5, found %d", hyphens+1)
	}
	starts := [5]int{0, 9, 14, 19, 24}
	expected := [5]int{8, 4, 4, 4, 12}
	for i := 0; i < 4; i++ {
		if bounds[i] != starts[i+1]-1 {
			return fmt.Errorf("invalid group length in group %d: expected %d, found %d", i, expected[i], bounds[i]-starts[i])
		}
	}
	return fmt.Errorf("invalid group length in group %d: expected %d, found %d", 4, expected[4], len(input)-starts[4])
}

// Decodes the BaseEvent timestamp field from an RFC 3339 string or epoch microseconds
func decodeEventTimestamp(v any) (ParsedTimestamp, error) {
	const expect = "an RFC 3339 timestamp string or epoch microseconds integer"
	if s, ok := v.(string); ok {
		p, err := ParseTimestampLeap(s)
		if err != nil {
			return ParsedTimestamp{}, err
		}
		return p, nil
	}
	n, ok := classifyNumber(v)
	if !ok || n.kind == numberFloat {
		return ParsedTimestamp{}, InvalidTypeError(v, expect)
	}
	var micros int64
	if n.kind == numberPosInt {
		if n.u > 1<<63-1 {
			return ParsedTimestamp{}, errors.New("epoch microseconds value is out of range")
		}
		micros = int64(n.u)
	} else {
		micros = n.i
	}
	t, ok := TimestampFromMicros(micros)
	if !ok {
		return ParsedTimestamp{}, errors.New("epoch microseconds value is out of range")
	}
	return ParsedTimestamp{Time: t}, nil
}

// Formats a leap-aware timestamp as to_rfc3339
func (p ParsedTimestamp) String() string {
	return formatRFC3339(p.Time, false, p.Leap)
}
