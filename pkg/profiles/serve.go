package profiles

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"gopkg.in/yaml.v3"
)

const standaloneDefaultWarning = "gateway.standalone is ignored on the default profile: it is the host gateway"

type standaloneMemo struct {
	signature any
	result    *bool
}

var (
	standaloneMu        sync.Mutex
	standaloneMemos     = map[string]standaloneMemo{}
	standaloneWarned    bool
	parkedDefaultWarned = map[string]bool{}
)

// Profile name and home a gateway serves
type ServedProfile struct {
	Name string
	Home string
}

type statErrorSignature struct{ errno syscall.Errno }

// Config reads behind the standalone check, replaced by tests that simulate unreadable files
var (
	configCacheVersion = config.ConfigCacheVersion
	readUserConfigRaw  = config.ReadUserConfigRaw
)

// Truthiness of gateway.standalone, strings through the shared token parser and other values by Python bool
func standaloneTruthy(value any) bool {
	if s, ok := value.(string); ok {
		v, known := pystr.BoolToken(s)
		return known && v
	}
	return pyTruthy(value)
}

// Classifies a raw config read failure as a YAML error, a transient OS or Unicode error, or neither
func standaloneReadError(err error) (isYAML, caught bool) {
	if config.IsOSError(err) || strings.Contains(err.Error(), "UTF-8") {
		return false, true
	}
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) || strings.HasPrefix(err.Error(), "yaml: ") {
		return true, true
	}
	return false, false
}

// Whether the home config opts out of the host multiplexer, never true for the default profile
func ProfileIsStandalone(ctx context.Context, home string) (bool, error) {
	cfgPath := filepath.Join(home, "config.yaml")
	key := home
	var signature any
	version, err := configCacheVersion(ctx, cfgPath)
	switch {
	case err == nil:
		signature = version
	case errors.Is(err, fs.ErrNotExist):
		signature = nil
	case config.IsOSError(err):
		var errno syscall.Errno
		errors.As(err, &errno)
		sig := statErrorSignature{errno}
		standaloneMu.Lock()
		prev, ok := standaloneMemos[key]
		if !ok || !signaturesEqual(prev.signature, sig) || prev.result == nil || *prev.result {
			slog.Warn("Cannot read gateway.standalone from " + cfgPath + " (" + pyExceptionName(err) + "); treating as not standalone")
		}
		standaloneMemos[key] = standaloneMemo{sig, boolPtr(false)}
		standaloneMu.Unlock()
		return false, nil
	default:
		return false, err
	}
	standaloneMu.Lock()
	cached, haveCached := standaloneMemos[key]
	standaloneMu.Unlock()
	if haveCached && signaturesEqual(cached.signature, signature) && cached.result != nil {
		return *cached.result, nil
	}
	var value any
	if signature != nil {
		cfg, err := readUserConfigRaw(ctx, cfgPath)
		if err != nil {
			isYAML, caught := standaloneReadError(err)
			if !caught {
				return false, err
			}
			standaloneMu.Lock()
			if !haveCached || !signaturesEqual(cached.signature, signature) {
				slog.Warn("Cannot read gateway.standalone from " + cfgPath + " (" + pyExceptionName(err) + "); treating as not standalone")
			}
			memo := standaloneMemo{signature: signature}
			if isYAML {
				memo.result = boolPtr(false)
			}
			standaloneMemos[key] = memo
			standaloneMu.Unlock()
			return false, nil
		}
		if gw, ok := cfg["gateway"].(map[string]any); ok {
			value = gw["standalone"]
		}
	}
	result := standaloneTruthy(value)
	standaloneMu.Lock()
	defer standaloneMu.Unlock()
	if filepath.Clean(home) == filepath.Clean(DefaultHome()) {
		if result && !standaloneWarned {
			slog.Warn(standaloneDefaultWarning)
			standaloneWarned = true
		}
		result = false
	}
	standaloneMemos[key] = standaloneMemo{signature, boolPtr(result)}
	return result, nil
}

func boolPtr(b bool) *bool { return &b }

// Python exception class name for a config read failure
func pyExceptionName(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case errors.Is(err, syscall.EISDIR):
		return "IsADirectoryError"
	case errors.Is(err, syscall.ENOTDIR):
		return "NotADirectoryError"
	case config.IsOSError(err):
		return "OSError"
	case strings.Contains(err.Error(), "UTF-8"):
		return "UnicodeDecodeError"
	}
	return "YAMLError"
}

// Marker file whose presence parks a profile out of the host gateway
func ParkedMarkerPath(home string) string { return filepath.Join(home, "gateway.parked") }

// Whether the parked marker exists, its contents never matter
func ProfileIsParked(home string) bool { return pathExists(ParkedMarkerPath(home)) }

// Profiles a gateway serves, the active one alone without multiplex, else default plus live named profiles
func ProfilesToServe(ctx context.Context, multiplex, includeStandalone, includeParked bool) ([]ServedProfile, error) {
	active := GetActiveProfileName(ctx)
	if active == "" {
		active = "default"
	}
	def := DefaultHome()
	if ProfileIsParked(def) {
		standaloneMu.Lock()
		if !parkedDefaultWarned[def] {
			slog.Warn("Ignoring gateway.parked for the default profile; stop the host gateway instead")
			parkedDefaultWarned[def] = true
		}
		standaloneMu.Unlock()
	}
	if !multiplex {
		dir, err := GetProfileDir(active)
		if err != nil {
			return nil, err
		}
		return []ServedProfile{{active, dir}}, nil
	}
	serve := []ServedProfile{{"default", def}}
	named, err := IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	for _, entry := range named {
		if !includeStandalone {
			standalone, err := ProfileIsStandalone(ctx, entry)
			if err != nil {
				return nil, err
			}
			if standalone {
				continue
			}
		}
		if !includeParked && ProfileIsParked(entry) {
			continue
		}
		serve = append(serve, ServedProfile{filepath.Base(entry), entry})
	}
	return serve, nil
}

// Clears the standalone memo and the one shot warnings
func ResetStandaloneMemo() {
	standaloneMu.Lock()
	defer standaloneMu.Unlock()
	clear(standaloneMemos)
	clear(parkedDefaultWarned)
	standaloneWarned = false
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
