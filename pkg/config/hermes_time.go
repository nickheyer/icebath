package config

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Multiplex predicate of agent.secret_scope that pkg/provider installs, nil in a process without a multiplexer
var IsMultiplexActive func(ctx context.Context) bool

type timezoneEntry struct {
	name string
	loc  *time.Location
}

var (
	tzCacheMu sync.Mutex
	tzCache   = map[[2]string]timezoneEntry{}
)

// Locale text made UTF-8 safe, escaped bytes decoded with encoding or the locale codec, other surrogates replaced
func RepairSurrogates(text, encoding string) string {
	rs := pystr.Runes(text)
	ascii, surrogate, escapedOnly := true, false, true
	for _, r := range rs {
		if r >= 0x80 {
			ascii = false
		}
		if pystr.IsSurrogate(r) {
			surrogate = true
		}
		if r >= 0x80 && (r < 0xdc80 || r > 0xdcff) {
			escapedOnly = false
		}
	}
	if ascii || !surrogate {
		return text
	}
	if escapedOnly {
		if raw, err := pystr.SurrogateEscapeEncode(text); err == nil {
			if encoding == "" {
				encoding = pycodecs.LocaleEncoding()
			}
			if out, err := pycodecs.Decode(raw, encoding); err == nil {
				return out
			}
		}
	}
	for i, r := range rs {
		if pystr.IsSurrogate(r) {
			rs[i] = '�'
		}
	}
	return pystr.FromRunes(rs)
}

// strftime that renders a surrogate zone name repaired, the UnicodeEncodeError returned for a format holding surrogates
func SafeStrftimeChecked(value time.Time, format string) (string, error) {
	if err := pystr.StrictUTF8EncodeError(format); err != nil {
		return "", err
	}
	if zone := pydatetime.TZName(value); pystr.StrictUTF8EncodeError(zone) != nil {
		_, offset := value.Zone()
		value = value.In(time.FixedZone(RepairSurrogates(zone, ""), offset))
	}
	return RepairSurrogates(pydatetime.Strftime(value, format), ""), nil
}

// strftime of a literal format with the zone name repaired, panicking on a format holding surrogates
func SafeStrftime(value time.Time, format string) string {
	out, err := SafeStrftimeChecked(value, format)
	if err != nil {
		panic(err)
	}
	return out
}

// ICEBATH_TIMEZONE when it speaks for the active profile, empty under multiplexing where each profile reads its config
func envTimezone(ctx context.Context) string {
	if IsMultiplexActive != nil && IsMultiplexActive(ctx) {
		return ""
	}
	return pystr.Strip(os.Getenv("ICEBATH_TIMEZONE"))
}

// Cache key naming the timezone source, the env value or the active config path
func timezoneCacheIdentity(ctx context.Context) [2]string {
	if tz := envTimezone(ctx); tz != "" {
		return [2]string{"environment", tz}
	}
	return [2]string{"config", home.ConfigPath(home.Dir(ctx))}
}

// Configured IANA name from the env or the effective config, the bare document when that load fails
func resolveTimezoneName(ctx context.Context) string {
	if tz := envTimezone(ctx); tz != "" {
		return tz
	}
	path := home.ConfigPath(home.Dir(ctx))
	var doc any
	if cfg, err := loadUserEffective(ctx, path, envRefLookup, false); err == nil {
		doc = cfg
	} else {
		exists, err := ConfigExists(ctx, path)
		if err != nil {
			return ""
		}
		if exists {
			if doc, err = ReadConfigDoc(ctx, path); err != nil {
				return ""
			}
		}
	}
	cfg, ok := doc.(map[string]any)
	if !ok {
		return ""
	}
	if tz, ok := cfg["timezone"].(string); ok && pystr.Strip(tz) != "" {
		return pystr.Strip(tz)
	}
	return ""
}

// Cached configured name and zone of the active profile, the first resolver of an identity publishing it
func timezoneEntryFor(ctx context.Context) timezoneEntry {
	identity := timezoneCacheIdentity(ctx)
	tzCacheMu.Lock()
	entry, ok := tzCache[identity]
	tzCacheMu.Unlock()
	if ok {
		return entry
	}
	name := resolveTimezoneName(ctx)
	var loc *time.Location
	if name != "" {
		var err error
		if loc, err = pydatetime.ZoneInfo(name); err != nil {
			loc = nil
			slog.Warn("Invalid timezone '" + name + "': " + err.Error() + ". Falling back to server local time.")
		}
	}
	tzCacheMu.Lock()
	defer tzCacheMu.Unlock()
	if prior, ok := tzCache[identity]; ok {
		return prior
	}
	entry = timezoneEntry{name: name, loc: loc}
	tzCache[identity] = entry
	return entry
}

// Configured zone of the active profile and true, nil and false for server local time
func TimeZone(ctx context.Context) (*time.Location, bool) {
	loc := timezoneEntryFor(ctx).loc
	return loc, loc != nil
}

// Configured IANA name of the active profile even when invalid, empty for server local time
func TimezoneName(ctx context.Context) string { return timezoneEntryFor(ctx).name }

// Drops every cached zone so the next call resolves the env and config again
func ResetTimezoneCache() {
	tzCacheMu.Lock()
	defer tzCacheMu.Unlock()
	clear(tzCache)
}

// Current time in the configured zone, else in server local time
func TimeNow(ctx context.Context) time.Time {
	if loc, ok := TimeZone(ctx); ok {
		return time.Now().In(loc)
	}
	return time.Now().Local()
}
