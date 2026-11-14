package auth

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func epochOf(t time.Time) float64 { return float64(t.Unix()) + float64(t.Nanosecond())/1e9 }

func timeOf(epoch float64) time.Time {
	sec, frac := math.Modf(epoch)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9)))
}

func nowEpoch() float64 { return epochOf(time.Now()) }

func isoStamp(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

func isoStampZ(t time.Time) string { return strings.TrimSuffix(isoStamp(t), "+00:00") + "Z" }

func asciiDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func isoDatePart(raw string) (year, month, day int, rest string, ok bool) {
	if len(raw) < 7 {
		return 0, 0, 0, "", false
	}
	if year, ok = asciiDigits(raw[:4]); !ok {
		return 0, 0, 0, "", false
	}
	extended := raw[4] == '-'
	pos := 4
	if extended {
		pos = 5
	}
	if raw[pos] == 'W' {
		if len(raw) < pos+3 {
			return 0, 0, 0, "", false
		}
		week, wok := asciiDigits(raw[pos+1 : pos+3])
		if !wok {
			return 0, 0, 0, "", false
		}
		pos += 3
		weekday := 1
		if extended {
			if pos < len(raw) && raw[pos] == '-' {
				if pos+2 > len(raw) {
					return 0, 0, 0, "", false
				}
				wd, dok := asciiDigits(raw[pos+1 : pos+2])
				if !dok {
					return 0, 0, 0, "", false
				}
				weekday, pos = wd, pos+2
			}
		} else if pos < len(raw) && raw[pos] >= '0' && raw[pos] <= '9' {
			weekday, pos = int(raw[pos]-'0'), pos+1
		}
		if week < 1 || week > 53 || weekday < 1 || weekday > 7 {
			return 0, 0, 0, "", false
		}
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
		wd := int(jan4.Weekday())
		if wd == 0 {
			wd = 7
		}
		d := jan4.AddDate(0, 0, 1-wd+(week-1)*7+weekday-1)
		if y, w := d.ISOWeek(); y != year || w != week {
			return 0, 0, 0, "", false
		}
		return d.Year(), int(d.Month()), d.Day(), raw[pos:], true
	}
	var mok, dok bool
	if extended {
		if len(raw) < 10 || raw[7] != '-' {
			return 0, 0, 0, "", false
		}
		month, mok = asciiDigits(raw[5:7])
		day, dok = asciiDigits(raw[8:10])
		rest = raw[10:]
	} else {
		if len(raw) < 8 {
			return 0, 0, 0, "", false
		}
		month, mok = asciiDigits(raw[4:6])
		day, dok = asciiDigits(raw[6:8])
		rest = raw[8:]
	}
	if !mok || !dok {
		return 0, 0, 0, "", false
	}
	return year, month, day, rest, true
}

func isoClock(s string) (hour, minute, sec, nsec int, ok bool) {
	if len(s) < 2 {
		return 0, 0, 0, 0, false
	}
	if hour, ok = asciiDigits(s[:2]); !ok {
		return 0, 0, 0, 0, false
	}
	s = s[2:]
	if s == "" {
		return hour, 0, 0, 0, true
	}
	extended := s[0] == ':'
	if extended {
		s = s[1:]
	}
	if len(s) < 2 {
		return 0, 0, 0, 0, false
	}
	if minute, ok = asciiDigits(s[:2]); !ok {
		return 0, 0, 0, 0, false
	}
	s = s[2:]
	if s == "" {
		return hour, minute, 0, 0, true
	}
	if extended {
		if s[0] != ':' {
			return 0, 0, 0, 0, false
		}
		s = s[1:]
	}
	if len(s) < 2 {
		return 0, 0, 0, 0, false
	}
	if sec, ok = asciiDigits(s[:2]); !ok {
		return 0, 0, 0, 0, false
	}
	s = s[2:]
	if s == "" {
		return hour, minute, sec, 0, true
	}
	if s[0] != '.' && s[0] != ',' {
		return 0, 0, 0, 0, false
	}
	frac := s[1:]
	if _, fok := asciiDigits(frac); !fok {
		return 0, 0, 0, 0, false
	}
	if len(frac) > 6 {
		frac = frac[:6]
	}
	micro, _ := asciiDigits(frac)
	for i := len(frac); i < 6; i++ {
		micro *= 10
	}
	return hour, minute, sec, micro * 1000, true
}

func isoZone(zone string) (*time.Location, bool) {
	if zone == "Z" {
		return time.UTC, true
	}
	if len(zone) < 3 || len(zone) == 4 || (zone[0] != '+' && zone[0] != '-') {
		return nil, false
	}
	h, m, s, _, ok := isoClock(zone[1:])
	if !ok || h > 23 || m > 59 || s > 59 {
		return nil, false
	}
	offset := h*3600 + m*60 + s
	if offset == 0 {
		return time.UTC, true
	}
	if zone[0] == '-' {
		offset = -offset
	}
	return time.FixedZone("", offset), true
}

func parseISO(raw string, naive *time.Location) (time.Time, bool) {
	year, month, day, rest, ok := isoDatePart(raw)
	if !ok {
		return time.Time{}, false
	}
	hour, minute, sec, nsec := 0, 0, 0, 0
	loc := naive
	if rest != "" {
		_, size := utf8.DecodeRuneInString(rest)
		clock, zone := rest[size:], ""
		if i := strings.IndexAny(clock, "+-Z"); i >= 0 {
			clock, zone = clock[:i], clock[i:]
		}
		if hour, minute, sec, nsec, ok = isoClock(clock); !ok {
			return time.Time{}, false
		}
		if zone != "" {
			if loc, ok = isoZone(zone); !ok {
				return time.Time{}, false
			}
		}
	}
	if year < 1 || month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nsec, loc)
	if t.Day() != day || int(t.Month()) != month {
		return time.Time{}, false
	}
	return t, true
}

func parseISOStamp(v any) (float64, bool) {
	s, _ := v.(string)
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, false
	}
	t, ok := parseISO(text, time.UTC)
	if !ok {
		return 0, false
	}
	return epochOf(t), true
}

func isExpiringISO(v any, skew time.Duration) bool {
	expires, ok := parseISOStamp(v)
	return !ok || expires <= nowEpoch()+skew.Seconds()
}

func parseAbsolute(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil || f <= 0 {
			return 0, false
		}
		if f > 1_000_000_000_000 {
			f /= 1000
		}
		return f, true
	case string:
		raw := strings.TrimSpace(t)
		if raw == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			if f > 1_000_000_000_000 {
				f /= 1000
			}
			return f, true
		}
		if tm, ok := parseISO(raw, time.Local); ok {
			return epochOf(tm), true
		}
	}
	return 0, false
}

func coerceTTL(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if n, ok := intOf(t); ok && n > 0 {
			return n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil && n > 0 {
			return n
		}
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func optionalBaseURL(v any) string {
	s, _ := v.(string)
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

func tlsState(insecure bool, caBundle string) *doc {
	d := newDoc()
	d.set("insecure", insecure)
	if caBundle == "" {
		d.set("ca_bundle", nil)
	} else {
		d.set("ca_bundle", caBundle)
	}
	return d
}

func lastAuthErrorMarker(provider string, err *Error, reason, defaultCode string) *doc {
	d := newDoc()
	d.set("provider", provider)
	d.set("message", err.Error())
	d.set("reason", reason)
	d.set("relogin_required", true)
	code := err.Code
	if code == "" {
		code = defaultCode
	}
	if code == "" {
		d.set("code", nil)
	} else {
		d.set("code", code)
	}
	d.set("at", isoStamp(time.Now()))
	return d
}

var flatOAuthTokenKeys = []string{"access_token", "refresh_token", "expires_at", "expires_in", "obtained_at"}

func quarantineFlatOAuthState(state *doc, provider string, err *Error) {
	for _, k := range flatOAuthTokenKeys {
		state.del(k)
	}
	state.set("last_auth_error", lastAuthErrorMarker(provider, err, "runtime_refresh_failure", "refresh_failed"))
}

func fileSignature(path string) ([]any, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	ino, ctime := statIdentity(fi)
	return []any{intNum(fi.ModTime().UnixNano()), intNum(fi.Size()), intNum(ino), intNum(ctime)}, true
}

func signatureValue(path string) any {
	if path == "" {
		return nil
	}
	sig, ok := fileSignature(path)
	if !ok {
		return nil
	}
	return sig
}
