package config

import (
	"bytes"
	"context"
	"errors"
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
func ReadConfigVersionStamp(ctx context.Context, h string, raiseOnParseError bool) (*int, int, error) {
	latest := latestConfigVersion()
	path := home.ConfigPath(h)
	if _, err := os.Stat(path); err != nil {
		return &latest, latest, nil
	}
	doc, err := readRawConfigRoot(path)
	if err != nil {
		if werr := WarnConfigParseFailure(ctx, path, err, "defaults"); werr != nil {
			return nil, latest, werr
		}
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
func CheckConfigVersion(ctx context.Context, h string, raiseOnParseError bool) (int, int, error) {
	stamp, latest, err := ReadConfigVersionStamp(ctx, h, raiseOnParseError)
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

// Records left core use and adopts browser env into a first stamped config, incomplete hooks panic with the reason
func NoteFirstStamp(h string, onDisk, toWrite map[string]any, hooks StampHooks) []string {
	moves, err := noteFirstStamp(h, onDisk, toWrite, hooks)
	if err != nil {
		panic(err)
	}
	return moves
}

// Error naming the first stamp collaborators no owner registered
var ErrStampHooksUnregistered = errors.New("config first stamp collaborators are not registered: link pkg/provider so left core " +
	"use is recorded and agent browser env is adopted before config.yaml gets its first _config_version")

// First stamp work, refusing with ErrStampHooksUnregistered when a collaborator it must run is missing
func noteFirstStamp(h string, onDisk, toWrite map[string]any, hooks StampHooks) ([]string, error) {
	if _, stamped := onDisk["_config_version"]; stamped {
		return nil, nil
	}
	if _, stamping := toWrite["_config_version"]; !stamping {
		return nil, nil
	}
	active := hooks.WorkerSnapshotActive
	if active == nil {
		active = func() bool { return CurrentSafeWorkerPolicy() != nil }
	}
	if active() {
		return nil, nil
	}
	if hooks.RecordCoreUse == nil || hooks.AdoptAgentBrowserEnv == nil {
		return nil, ErrStampHooksUnregistered
	}
	for _, v := range hooks.CoreVersions {
		hooks.RecordCoreUse(h, toWrite, v)
	}
	return hooks.AdoptAgentBrowserEnv(h, toWrite), nil
}

// Writes a first stamped config.yaml for an unmanaged home without one whose env needs adopting
func StampMissingConfig(ctx context.Context, h string, hooks StampHooks) (bool, error) {
	path := home.ConfigPath(h)
	if _, err := os.Stat(path); err == nil || ManagedSystem(ctx, h) != "" {
		return false, nil
	}
	cfg := map[string]any{"_config_version": latestConfigVersion()}
	moves, err := noteFirstStamp(h, map[string]any{}, cfg, hooks)
	if err != nil || len(moves) == 0 {
		return false, err
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return false, err
	}
	if err := AtomicConfigReplace(ctx, h, path, cfg, ""); err != nil {
		return false, err
	}
	return true, os.Chmod(path, 0o600)
}
