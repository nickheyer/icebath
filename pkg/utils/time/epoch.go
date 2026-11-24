package timeutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	firstEpoch     = -62135596800.0 // 0001-01-01T00:00:00Z
	endEpoch       = 253402300800.0 // 10000-01-01T00:00:00Z
	storedEpochMax = 4_200_000_000.0
)

// Seconds since the Unix epoch of t as a float, to the nanosecond as far as a float64 holds it
func Epoch(t time.Time) float64 { return float64(t.Unix()) + float64(t.Nanosecond())/1e9 }

// Local instant of float seconds since the Unix epoch to the nanosecond, the zero Time for NaN, infinities and instants outside the years 1 to 9999
func FromEpoch(ts float64) time.Time {
	if !(ts > firstEpoch && ts < endEpoch) {
		return time.Time{}
	}
	sec, frac := math.Modf(ts)
	t := time.Unix(int64(sec), int64(math.Round(frac*1e9)))
	if t.Unix() >= int64(endEpoch) {
		return time.Time{}
	}
	return t
}

// Epoch seconds a stored timestamp cell holds, false for unset and corrupt values, a warning naming the field and session logged for corrupt ones
func StoredEpoch(v any, sessionID, field string) (float64, bool) {
	ts := math.NaN()
	switch x := v.(type) {
	case nil:
		return 0, false
	case string:
		if x == "" {
			return 0, false
		}
		ts = parseEpochText(x)
	case []byte:
		if len(x) == 0 {
			return 0, false
		}
		ts = parseEpochText(string(x))
	case json.Number:
		ts = parseEpochText(string(x))
	case time.Time:
		ts = Epoch(x)
	case *time.Time:
		if x == nil {
			return 0, false
		}
		ts = Epoch(*x)
	case float64:
		ts = x
	case float32:
		ts = float64(x)
	case int:
		ts = float64(x)
	case int64:
		ts = float64(x)
	case int32:
		ts = float64(x)
	case int16:
		ts = float64(x)
	case int8:
		ts = float64(x)
	case uint:
		ts = float64(x)
	case uint64:
		ts = float64(x)
	case uint32:
		ts = float64(x)
	case uint16:
		ts = float64(x)
	case uint8:
		ts = float64(x)
	}
	if !(ts >= 0 && ts <= storedEpochMax) {
		if field == "" {
			field = "timestamp"
		}
		msg := "Ignoring corrupt " + field + " " + describeCell(v)
		if sessionID != "" {
			msg += " on session " + sessionID
		}
		slog.Default().With("logger", "utils/time").Warn(msg)
		return 0, false
	}
	return ts, true
}

// Age of a stored timestamp cell as just now, 5m ago, 3h ago, yesterday or 4d ago, its local date when older and ? when unset, zero or corrupt
func RelativeAge(v any, sessionID string, now time.Time) string {
	ts, ok := StoredEpoch(v, sessionID, "last_active")
	if !ok || ts == 0 {
		return "?"
	}
	switch delta := Epoch(now) - ts; {
	case delta < 60:
		return "just now"
	case delta < 3600:
		return strconv.Itoa(int(delta/60)) + "m ago"
	case delta < 86400:
		return strconv.Itoa(int(delta/3600)) + "h ago"
	case delta < 172800:
		return "yesterday"
	case delta < 604800:
		return strconv.Itoa(int(delta/86400)) + "d ago"
	}
	return FromEpoch(ts).Format(time.DateOnly)
}

// Decimal or exponent text of a stored number, NaN when it is none
func parseEpochText(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// Cell value as it appears in the corrupt timestamp warning, text quoted
func describeCell(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case []byte:
		return strconv.Quote(string(x))
	}
	return fmt.Sprint(v)
}
