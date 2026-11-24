package gateway

import (
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

const timestampPrefixPattern = `^\[(?:` +
	`(?P<dow>[A-Z][a-z]{2}) ` +
	`(?P<date>\d{4}-\d{2}-\d{2}) ` +
	`(?P<time>\d{2}:\d{2}:\d{2})` +
	`(?: (?P<tz>[A-Za-z0-9_+\-/:]+))?` +
	`|(?P<iso>\d{4}-\d{2}-\d{2}T[^\]]+)` +
	`)\]\s*`

var (
	timestampPrefixRE = regexutil.MustCompile(timestampPrefixPattern)
)

// System zone wall clock stamps are read in without a configured one, a seam for zone pinned tests
var timestampLocalZone = func() *time.Location { return time.Local }

// Matched text of a named group of the timestamp prefix, empty when the group did not take part
func timestampGroup(m *regexp2.Match, name string) string {
	return regexutil.NamedGroup(m, name)
}

// Zone wall clock stamps are read in, the configured zone or the system one
func timestampZone(tz *time.Location) *time.Location {
	if tz != nil {
		return tz
	}
	return timestampLocalZone()
}

// Epoch of an ISO 8601 string, zone-less text read in tz or the system zone
func parseTimestampISO(text string, tz *time.Location) (float64, bool) {
	t, err := timeutil.ParseISO(text, timestampZone(tz))
	if err != nil {
		return 0, false
	}
	return timeutil.Epoch(t), true
}

func parseTimestampMatch(m *regexp2.Match, tz *time.Location) (float64, bool) {
	if iso := timestampGroup(m, "iso"); iso != "" {
		return parseTimestampISO(iso, tz)
	}
	wall, err := time.ParseInLocation(time.DateTime, timestampGroup(m, "date")+" "+timestampGroup(m, "time"), timestampZone(tz))
	if err != nil {
		return 0, false
	}
	return timeutil.Epoch(wall), true
}

// Epoch seconds of a number, time, ISO string or bracketed gateway stamp, false when uninterpretable
func CoerceMessageTimestamp(value any, tz *time.Location) (float64, bool, error) {
	switch v := value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, *big.Int, float32, float64:
		f, err := anyutil.ToFloat(v)
		if err != nil {
			return 0, false, err
		}
		return f, true, nil
	case time.Time:
		return timeutil.Epoch(v), true, nil
	case *time.Time:
		if v != nil {
			return timeutil.Epoch(*v), true, nil
		}
	}
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false, nil
	}
	if m, _ := timestampPrefixRE.FindStringMatch(text); m != nil {
		if parsed, ok := parseTimestampMatch(m, tz); ok {
			return parsed, true, nil
		}
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
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
	dt := timeutil.FromEpoch(epoch)
	if dt.IsZero() {
		return "", nil
	}
	return "[" + dt.In(timestampZone(tz)).Format("Mon 2006-01-02 15:04:05 MST") + "]", nil
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
	if prefix != "" && anyutil.IsTrue(clean) {
		return prefix + " " + anyutil.String(clean), nil
	}
	if prefix != "" {
		return prefix, nil
	}
	return clean, nil
}
