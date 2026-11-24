package config

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
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

// Time written in a strftime format
func SafeStrftime(value time.Time, format string) string { return timeutil.Strftime(value, format) }

// ICEBATH_TIMEZONE when it speaks for the active profile, empty under multiplexing where each profile reads its config
func envTimezone(ctx context.Context) string {
	if IsMultiplexActive != nil && IsMultiplexActive(ctx) {
		return ""
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_TIMEZONE"))
}

// Cache key naming the timezone source, the env value or the active config path
func timezoneCacheIdentity(ctx context.Context) [2]string {
	if tz := envTimezone(ctx); tz != "" {
		return [2]string{"environment", tz}
	}
	return [2]string{"config", GetConfigPath(home.Dir(ctx))}
}

// Configured IANA name from the env or the effective config, the bare document when that load fails
func resolveTimezoneName(ctx context.Context) string {
	if tz := envTimezone(ctx); tz != "" {
		return tz
	}
	path := GetConfigPath(home.Dir(ctx))
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
	if tz, ok := cfg["timezone"].(string); ok && strings.TrimSpace(tz) != "" {
		return strings.TrimSpace(tz)
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
		if loc, err = time.LoadLocation(name); err != nil {
			loc = nil
			pkgLog().Warn("Invalid timezone '" + name + "': " + err.Error() + ". Falling back to server local time.")
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

// Current time in the configured zone, else in the fixed server local offset astimezone gives
func TimeNow(ctx context.Context) time.Time {
	if loc, ok := TimeZone(ctx); ok {
		return time.Now().In(loc)
	}
	return time.Now().Local()
}
