package gateway

import (
	"math/big"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const timestampPrefixPattern = `^\[(?:` +
	`(?P<dow>[A-Z][a-z]{2}) ` +
	`(?P<date>\d{4}-\d{2}-\d{2}) ` +
	`(?P<time>\d{2}:\d{2}:\d{2})` +
	`(?: (?P<tz>[A-Za-z0-9_+\-/:]+))?` +
	`|(?P<iso>\d{4}-\d{2}-\d{2}T[^\]]+)` +
	`)\]\s*`

var (
	timestampPrefixRE     = regexp2.MustCompile(mustTranslatePy(timestampPrefixPattern), regexp2.None)
	timestampPrefixGroups = mustGroupIndexPy(timestampPrefixPattern)
)

// Local zone and localtime gate of the system timezone conversions, seams for zone pinned tests
var (
	timestampLocalZone        = func() *time.Location { return time.Local }
	timestampLocaltimeAccepts = pydatetime.LocaltimeAccepts
)

func mustTranslatePy(pattern string) string {
	out, err := pyre.Translate(pattern)
	if err != nil {
		panic(err)
	}
	return out
}

func mustGroupIndexPy(pattern string) map[string]int {
	index, err := pyre.GroupIndex(pattern)
	if err != nil {
		panic(err)
	}
	return index
}

// Matched text of a named group of the timestamp prefix, empty when the group did not take part
func timestampGroup(m *regexp2.Match, name string) string {
	g := m.GroupByNumber(timestampPrefixGroups[name])
	if g == nil || len(g.Captures) == 0 {
		return ""
	}
	return g.String()
}

// Exact epoch of whole seconds plus microseconds, divided like timedelta total_seconds
func epochWithMicros(sec int64, micro int) float64 {
	return pydatetime.Timestamp(time.Unix(sec, int64(micro)*1000))
}

// Epoch of a parsed datetime in tz or the local zone, false outside the platform localtime range
func localizeTimestamp(wall time.Time, aware bool, fold int, tz *time.Location) (float64, bool) {
	if aware {
		return pydatetime.Timestamp(wall), true
	}
	micro := wall.Nanosecond() / 1000
	if tz != nil {
		sec, _ := pydatetime.LocalToSeconds(wall, tz, fold, nil)
		return epochWithMicros(sec, micro), true
	}
	local := timestampLocalZone()
	if sec, ok := pydatetime.LocalToSeconds(wall, local, fold, timestampLocaltimeAccepts); ok && timestampLocaltimeAccepts(sec) {
		return epochWithMicros(sec, micro), true
	}
	epoch, _ := pydatetime.LocalToSeconds(wall, local, fold, nil)
	if epoch < 0 || epoch >= 86_400 {
		return 0, false
	}
	back := time.Unix(epoch, 0).In(local)
	y, mo, d := back.Date()
	wy, wmo, wd := wall.Date()
	if y != wy || mo != wmo || d != wd || back.Hour() != wall.Hour() || back.Minute() != wall.Minute() || back.Second() != wall.Second() {
		return 0, false
	}
	return float64(epoch) + float64(micro)/1_000_000, true
}

// Epoch of an ISO 8601 string through fromisoformat, then strptime for the colonless offsets it rejects
func parseTimestampISO(text string, tz *time.Location) (float64, bool) {
	if t, aware, err := pydatetime.ParseISOFormat(text); err == nil {
		return localizeTimestamp(t, aware, 0, tz)
	}
	if t, aware, err := pydatetime.Strptime(text, "%Y-%m-%dT%H:%M:%S%z"); err == nil {
		return localizeTimestamp(t, aware, 0, tz)
	}
	return 0, false
}

func parseTimestampMatch(m *regexp2.Match, tz *time.Location) (float64, bool) {
	if iso := timestampGroup(m, "iso"); iso != "" {
		return parseTimestampISO(iso, tz)
	}
	wall, _, err := pydatetime.Strptime(timestampGroup(m, "date")+" "+timestampGroup(m, "time"), "%Y-%m-%d %H:%M:%S")
	if err != nil {
		return 0, false
	}
	return localizeTimestamp(wall, false, 0, tz)
}

// Epoch seconds of a number, time, ISO string or bracketed gateway stamp, false when uninterpretable
func CoerceMessageTimestamp(value any, tz *time.Location) (float64, bool, error) {
	switch v := value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, *big.Int, float32, float64:
		f, err := pyint.Float(v)
		if err != nil {
			return 0, false, err
		}
		return f, true, nil
	case time.Time:
		return pydatetime.Timestamp(v), true, nil
	case *time.Time:
		if v != nil {
			return pydatetime.Timestamp(*v), true, nil
		}
	}
	text, _ := value.(string)
	text = pystr.Strip(text)
	if text == "" {
		return 0, false, nil
	}
	if m, _ := timestampPrefixRE.FindStringMatch(text); m != nil {
		if parsed, ok := parseTimestampMatch(m, tz); ok {
			return parsed, true, nil
		}
	}
	if f, err := pyint.ParseFloat(text); err == nil {
		return f, true, nil
	}
	parsed, ok := parseTimestampISO(text, tz)
	return parsed, ok, nil
}

// Timestamp rendered as a bracketed weekday, date, time and zone name, empty when unrenderable
func FormatMessageTimestamp(value any, tz *time.Location) (string, error) {
	epoch, ok, err := CoerceMessageTimestamp(value, tz)
	if err != nil || !ok {
		return "", err
	}
	var dt time.Time
	if tz != nil {
		dt, err = pydatetime.FromTimestampIn(epoch, tz, nil)
	} else {
		dt, err = pydatetime.FromTimestampIn(epoch, timestampLocalZone(), timestampLocaltimeAccepts)
	}
	if err != nil {
		return "", nil
	}
	return "[" + config.SafeStrftime(dt, "%a %Y-%m-%d %H:%M:%S %Z") + "]", nil
}

// Leading gateway timestamp prefixes removed, with the epoch of the prefix closest to the text
func StripLeadingMessageTimestamps(content any, tz *time.Location) (any, *float64) {
	text, isStr := content.(string)
	if !isStr || text == "" {
		return content, nil
	}
	var embedded *float64
	for {
		m, _ := timestampPrefixRE.FindStringMatch(text)
		if m == nil {
			break
		}
		if parsed, ok := parseTimestampMatch(m, tz); ok {
			embedded = &parsed
		}
		start, length := m.ByteRange()
		text = text[start+length:]
	}
	return text, embedded
}

// User message for model context with exactly one timestamp prefix, an embedded stamp winning over value
func RenderUserContentWithTimestamp(content, value any, tz *time.Location) (any, error) {
	clean, embedded := StripLeadingMessageTimestamps(content, tz)
	source := value
	if embedded != nil {
		source = *embedded
	}
	prefix, err := FormatMessageTimestamp(source, tz)
	if err != nil {
		return nil, err
	}
	if prefix != "" && pyrepr.Truthy(clean) {
		return prefix + " " + pyrepr.Str(clean), nil
	}
	if prefix != "" {
		return prefix, nil
	}
	return clean, nil
}
