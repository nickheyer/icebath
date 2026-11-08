package provider

import (
	"strconv"
	"strings"
	"time"
)

// ParseAbsoluteTimestamp converts epoch seconds, epoch milliseconds or ISO 8601 text into epoch seconds
func ParseAbsoluteTimestamp(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case float64:
		return scaleEpoch(v)
	case float32:
		return scaleEpoch(float64(v))
	case int:
		return scaleEpoch(float64(v))
	case int64:
		return scaleEpoch(float64(v))
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, false
		}
		if n, err := strconv.ParseFloat(raw, 64); err == nil {
			if n > 1_000_000_000_000 {
				return n / 1000, true
			}
			return n, true
		}
		if t, ok := parseISO(raw); ok {
			return float64(t.UnixNano()) / 1e9, true
		}
		return 0, false
	}
	return 0, false
}

func scaleEpoch(n float64) (float64, bool) {
	if n <= 0 {
		return 0, false
	}
	if n > 1_000_000_000_000 {
		return n / 1000, true
	}
	return n, true
}

// parseISO accepts every date and datetime shape Python fromisoformat accepts and treats naive values as UTC
func parseISO(raw string) (time.Time, bool) {
	text := strings.TrimSpace(raw)
	if len(text) > 10 && text[10] == ' ' {
		text = text[:10] + "T" + text[11:]
	}
	if strings.HasSuffix(text, "Z") {
		text = text[:len(text)-1] + "+00:00"
	}
	layouts := []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999-0700",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04-07:00",
		"2006-01-02T15:04",
		"2006-01-02T15-07:00",
		"2006-01-02T15",
		"2006-01-02",
		"20060102T150405",
		"20060102",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, text); err == nil {
			if t.Location() == time.UTC || strings.Contains(layout, "-07") {
				return t, true
			}
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC), true
		}
	}
	return time.Time{}, false
}

// ParseISOTimestamp returns epoch seconds for ISO 8601 text and false for anything else
func ParseISOTimestamp(value any) (float64, bool) {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	t, ok := parseISO(text)
	if !ok {
		return 0, false
	}
	return float64(t.UnixNano()) / 1e9, true
}

// IsExpiring reports whether an ISO expiry is missing, unparseable or within skew of now
func IsExpiring(expiresAt any, skew time.Duration) bool {
	epoch, ok := ParseISOTimestamp(expiresAt)
	if !ok {
		return true
	}
	return epoch <= nowEpoch()+skew.Seconds()
}

var nowFunc = time.Now

// SetPoolClock swaps the pool wall clock and returns the function that puts the previous one back
func SetPoolClock(fn func() time.Time) func() {
	prev := nowFunc
	nowFunc = fn
	return func() { nowFunc = prev }
}

func nowEpoch() float64 { return float64(nowFunc().UnixNano()) / 1e9 }

// utcNowZ formats the current UTC time with a trailing Z
func utcNowZ() string {
	return strings.Replace(nowFunc().UTC().Format("2006-01-02T15:04:05.000000-07:00"), "+00:00", "Z", 1)
}

// utcNowISO formats the current UTC time the way Python isoformat does
func utcNowISO() string {
	return nowFunc().UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// epochToISO renders epoch seconds as UTC ISO 8601 with offset
func epochToISO(epoch float64) string {
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format("2006-01-02T15:04:05.000000-07:00")
}
