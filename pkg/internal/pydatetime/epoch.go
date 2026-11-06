package pydatetime

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Lowest epoch second a stored timestamp may carry to be trusted
const EpochMin = 0.0

// Highest epoch second a stored timestamp may carry, inside a 32 bit time_t
const EpochMax = 4_200_000_000.0

// Stored timestamp cell as epoch seconds, false for unset or corrupt values after a warning naming the session
func CoerceEpoch(value any, sessionID, field string) (float64, bool) {
	if field == "" {
		field = "timestamp"
	}
	if value == nil {
		return 0, false
	}
	if s, ok := value.(string); ok && s == "" {
		return 0, false
	}
	ts := math.NaN()
	switch v := value.(type) {
	case time.Time:
		ts = float64(v.UnixNano()) / 1e9
	case float64:
		ts = v
	case float32:
		ts = float64(v)
	case int:
		ts = float64(v)
	case int64:
		ts = float64(v)
	case int32:
		ts = float64(v)
	case uint64:
		ts = float64(v)
	case bool:
		if v {
			ts = 1
		} else {
			ts = 0
		}
	case string:
		if f, err := pyint.ParseFloat(v); err == nil {
			ts = f
		}
	case []byte:
		if f, err := pyint.ParseFloat(string(v)); err == nil {
			ts = f
		}
	}
	if !(EpochMin <= ts && ts <= EpochMax) {
		suffix := ""
		if sessionID != "" {
			suffix = " on session " + sessionID
		}
		slog.Warn(fmt.Sprintf("Ignoring corrupt %s %s%s", field, pyrepr.Repr(value), suffix), "logger", "hermes_cli.timefmt")
		return 0, false
	}
	return ts, true
}

// Local time of epoch seconds the way datetime.fromtimestamp builds it
func FromTimestamp(ts float64) time.Time {
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).Local()
}

// Relative age of a timestamp such as 2h ago or yesterday, a question mark when unset or corrupt
func RelativeTime(value any, sessionID string, now time.Time) string {
	if !pyrepr.Truthy(value) {
		return "?"
	}
	ts, ok := CoerceEpoch(value, sessionID, "last_active")
	if !ok {
		return "?"
	}
	delta := float64(now.UnixNano())/1e9 - ts
	switch {
	case delta < 60:
		return "just now"
	case delta < 3600:
		return fmt.Sprintf("%dm ago", int(delta/60))
	case delta < 86400:
		return fmt.Sprintf("%dh ago", int(delta/3600))
	case delta < 172800:
		return "yesterday"
	case delta < 604800:
		return fmt.Sprintf("%dd ago", int(delta/86400))
	}
	return Strftime(FromTimestamp(ts), "%Y-%m-%d")
}
