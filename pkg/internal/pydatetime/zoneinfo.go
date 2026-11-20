package pydatetime

import (
	"bytes"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ZoneInfoNotFoundError is zoneinfo.ZoneInfoNotFoundError, a KeyError whose str is the quoted message
type ZoneInfoNotFoundError struct{ Key string }

func (e *ZoneInfoNotFoundError) Error() string {
	return pyrepr.StrRepr("No time zone found with key " + e.Key)
}

// ZoneKeyError is the ValueError zoneinfo raises for a key that is not a normalized relative path
type ZoneKeyError struct{ Msg string }

func (e *ZoneKeyError) Error() string { return e.Msg }

// CPython configure default TZPATH, the list python-build-standalone interpreters search
var defaultTZPath = []string{"/usr/share/zoneinfo", "/usr/lib/zoneinfo", "/usr/share/lib/zoneinfo", "/etc/zoneinfo"}

// Search path zoneinfo reads, PYTHONTZPATH absolute entries when set, else the platform default
func tzPath() []string {
	raw, ok := os.LookupEnv("PYTHONTZPATH")
	if !ok {
		return platformTZPath()
	}
	if raw == "" {
		return nil
	}
	var out []string
	for _, entry := range strings.Split(raw, string(os.PathListSeparator)) {
		if pypath.IsAbs(entry) {
			out = append(out, entry)
		}
	}
	return out
}

// Rejects absolute, unnormalized and escaping keys with the ValueError texts of _validate_tzfile_path
func validateZoneKey(key string) error {
	if pypath.IsAbs(key) {
		return &ZoneKeyError{"ZoneInfo keys may not be absolute paths, got: " + key}
	}
	normalized := pypath.Normpath(key)
	if pystr.Len(normalized) != pystr.Len(key) {
		return &ZoneKeyError{"ZoneInfo keys must be normalized relative paths, got: " + key}
	}
	base := pypath.Normpath(pypath.Join("_", "_"))
	base = base[:len(base)-1]
	if !strings.HasPrefix(pypath.Normpath(pypath.Join(base, normalized)), base) {
		return &ZoneKeyError{"ZoneInfo keys must refer to subdirectories of TZPATH, got: " + key}
	}
	return nil
}

// Loads key the way zoneinfo.ZoneInfo does, TZPATH files first and then the bundled tzdata package
func ZoneInfo(key string) (*time.Location, error) {
	if err := validateZoneKey(key); err != nil {
		return nil, err
	}
	for _, dir := range tzPath() {
		path := pypath.Join(dir, key)
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return zoneFromTZif(key, data)
	}
	if loc, ok := tzdataPackageZone(key); ok {
		return loc, nil
	}
	return nil, &ZoneInfoNotFoundError{Key: key}
}

// Parses TZif bytes under key, the magic check raising the zoneinfo ValueError text
func zoneFromTZif(key string, data []byte) (*time.Location, error) {
	if !bytes.HasPrefix(data, []byte("TZif")) {
		return nil, &ZoneKeyError{"Invalid TZif file: magic not found"}
	}
	return time.LoadLocationFromTZData(key, data)
}
