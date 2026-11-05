package config

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Safe non negative config version where an invalid value reads as legacy zero
func coerceConfigVersion(v any) int {
	var n int
	switch x := v.(type) {
	case bool, nil:
		return 0
	case int:
		n = x
	case int64:
		n = int(x)
	case uint64:
		n = int(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0
		}
		n = int(x)
	case string:
		parsed, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(x), "_", ""))
		if err != nil || strings.HasPrefix(strings.TrimSpace(x), "_") || strings.HasSuffix(strings.TrimSpace(x), "_") || strings.Contains(x, "__") {
			return 0
		}
		n = parsed
	default:
		return 0
	}
	return max(n, 0)
}

// Schema version the defaults carry
func latestConfigVersion() int {
	if v := coerceConfigVersion(Defaults()["_config_version"]); v != 0 {
		return v
	}
	return 1
}

// Raw stamp of config.yaml, nil when it parsed without a version key, plus the latest version
func ReadConfigVersionStamp(h string, raiseOnParseError bool) (*int, int, error) {
	latest := latestConfigVersion()
	path := home.ConfigPath(h)
	if _, err := os.Stat(path); err != nil {
		return &latest, latest, nil
	}
	doc, err := readRawConfigRoot(path)
	if err != nil {
		WarnConfigParseFailure(path, err, "defaults")
		if raiseOnParseError {
			return nil, latest, &InvalidUserConfigError{Path: path, Err: fmt.Errorf("Cannot inspect %s: config.yaml is not valid YAML (%w)", path, err)}
		}
		return &latest, latest, nil
	}
	cfg := AsMap(doc)
	if doc != nil && cfg == nil {
		if raiseOnParseError {
			return nil, latest, &InvalidUserConfigError{Path: path, Err: fmt.Errorf(
				"Cannot inspect %s: config.yaml top-level value must be a mapping, got %s", path, PyTypeName(doc))}
		}
		cfg = map[string]any{}
	}
	raw, ok := cfg["_config_version"]
	if !ok {
		return nil, latest, nil
	}
	stamp := coerceConfigVersion(raw)
	return &stamp, latest, nil
}

// Parsed root of a config file, nil for an empty document
func readRawConfigRoot(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return nil, err
	}
	if m := AsMap(doc); m != nil {
		return normalizeMaps(m), nil
	}
	return doc, nil
}

// Current and latest config versions from the raw file, a file without a version key reading as zero
func CheckConfigVersion(h string, raiseOnParseError bool) (int, int, error) {
	stamp, latest, err := ReadConfigVersionStamp(h, raiseOnParseError)
	if err != nil {
		return 0, latest, err
	}
	if stamp == nil {
		return 0, latest, nil
	}
	return *stamp, latest, nil
}

// Collaborators the first stamp runs, supplied by the packages that own them
type StampHooks struct {
	WorkerSnapshotActive func() bool
	CoreVersions         []int
	RecordCoreUse        func(h string, cfg map[string]any, version int) bool
	AdoptAgentBrowserEnv func(h string, cfg map[string]any) []string
}

// Records left core use and adopts browser env into a first stamped config, returning the browser moves
func NoteFirstStamp(h string, onDisk, toWrite map[string]any, hooks StampHooks) []string {
	if _, stamped := onDisk["_config_version"]; stamped {
		return nil
	}
	if _, stamping := toWrite["_config_version"]; !stamping {
		return nil
	}
	if hooks.WorkerSnapshotActive() {
		return nil
	}
	for _, v := range hooks.CoreVersions {
		hooks.RecordCoreUse(h, toWrite, v)
	}
	return hooks.AdoptAgentBrowserEnv(h, toWrite)
}

// Writes a first stamped config.yaml for an unmanaged home without one whose env needs adopting
func StampMissingConfig(h string, hooks StampHooks) (bool, error) {
	path := home.ConfigPath(h)
	if _, err := os.Stat(path); err == nil || ManagedSystem(h) != "" {
		return false, nil
	}
	cfg := map[string]any{"_config_version": latestConfigVersion()}
	if len(NoteFirstStamp(h, map[string]any{}, cfg, hooks)) == 0 {
		return false, nil
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return false, err
	}
	if err := writeDocument(path, cfg); err != nil {
		return false, err
	}
	return true, os.Chmod(path, 0o600)
}
