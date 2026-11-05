package config

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// EnvManagedDir is the IT only override naming the managed scope directory
const EnvManagedDir = "ICEBATH_MANAGED_DIR"

// POSIX default managed scope directory, other platform locations belong only inside ManagedDir
const defaultManagedDir = "/etc/icebath"

// Whether the selected config backend takes managed files, an unavailable backend exiting like the Python SystemExit
var ManagedConfigHonored = func() bool {
	backend, err := GetConfigBackend(context.Background())
	if err != nil {
		panic(err)
	}
	return backend.HonorsManagedConfig()
}

var (
	managedMu          sync.Mutex
	managedConfigCache = map[string]managedEntry{}
	managedEnvCache    = map[string]managedEntry{}
)

// managedEntry is one parsed managed file keyed by its signature
type managedEntry struct {
	sig   fileSig
	value any
}

// ManagedDir resolves the managed scope directory, empty when no scope is present
func ManagedDir() string {
	override := strings.TrimSpace(os.Getenv(EnvManagedDir))
	dir := defaultManagedDir
	if override != "" {
		dir = override
	} else if os.Getenv("ICEBATH_UNDER_TEST") != "" {
		return ""
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// InvalidateManagedCache drops cached managed config and env
func InvalidateManagedCache() {
	managedMu.Lock()
	managedConfigCache = map[string]managedEntry{}
	managedEnvCache = map[string]managedEntry{}
	managedMu.Unlock()
}

// Signature keyed read of a managed file, nil when absent or malformed, a parse failure logged loudly
func managedCachedRead(path string, cache map[string]managedEntry, parse func(string) (any, error)) any {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	sig, exists, err := signature(path)
	if err == nil && !exists {
		return nil
	}
	if err != nil {
		slog.Warn("managed scope: failed to parse managed file, IGNORING it. Admin policy from this file is NOT being applied. Fix and restart.", "path", path, "error", err)
		return nil
	}
	managedMu.Lock()
	hit, ok := cache[path]
	managedMu.Unlock()
	if ok && hit.sig == sig {
		return Clone(hit.value)
	}
	parsed, err := parse(path)
	if err != nil {
		slog.Warn("managed scope: failed to parse managed file, IGNORING it. Admin policy from this file is NOT being applied. Fix and restart.", "path", path, "error", err)
		return nil
	}
	managedMu.Lock()
	cache[path] = managedEntry{sig: sig, value: Clone(parsed)}
	managedMu.Unlock()
	return parsed
}

// Parsed managed file as a map, empty when no scope, absent, malformed, or not a mapping
func loadManagedFile(name string, cache map[string]managedEntry, parse func(string) (any, error)) map[string]any {
	dir := ManagedDir()
	if dir == "" {
		return map[string]any{}
	}
	parsed, _ := managedCachedRead(filepath.Join(dir, name), cache, parse).(map[string]any)
	if parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// LoadManagedConfig is the parsed managed config.yaml, empty when absent, malformed, or not honored
func LoadManagedConfig() map[string]any {
	if !ManagedConfigHonored() {
		return map[string]any{}
	}
	return loadManagedFile("config.yaml", managedConfigCache, func(path string) (any, error) {
		text, err := readManagedText(path)
		if err != nil {
			return nil, err
		}
		doc, err := FastSafeLoad([]byte(text))
		if err != nil {
			return nil, err
		}
		if m := AsMap(doc); m != nil {
			return m, nil
		}
		if doc == nil {
			return map[string]any{}, nil
		}
		return doc, nil
	})
}

// Text of a managed file read as strict utf-8-sig, a decode error failing loud instead of falling back to Latin-1
func readManagedText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return "", pystr.UTF8DecodeError(data, i)
		}
		i += size
	}
	return string(data), nil
}

// LoadManagedEnv is the parsed managed .env, empty when absent
func LoadManagedEnv() map[string]string {
	raw := loadManagedFile(".env", managedEnvCache, func(path string) (any, error) {
		text, err := readManagedText(path)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for k, v := range ParseEnv(text) {
			out[k] = v
		}
		return out, nil
	})
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Signature of the managed config file, zero when there is no scope or file
func managedConfigSignature() fileSig {
	dir := ManagedDir()
	if dir == "" {
		return fileSig{}
	}
	sig, _, _ := signature(filepath.Join(dir, "config.yaml"))
	return sig
}

// Process environment lookup for managed refs so a user cannot shadow a managed literal
func processLookup(name string) (string, bool) { return os.LookupEnv(name) }

// ApplyManagedOverlay deep merges administrator pinned values on top of the document, unchanged without a scope
func ApplyManagedOverlay(doc map[string]any) map[string]any {
	managed := LoadManagedConfig()
	if len(managed) == 0 {
		return doc
	}
	expanded, _ := Expand(managed, processLookup).(map[string]any)
	if expanded == nil {
		return doc
	}
	expanded = normalizeModelKeys(expanded)
	if model, ok := expanded["model"].(string); ok {
		expanded = Clone(expanded).(map[string]any)
		expanded["model"] = map[string]any{"default": model}
	}
	return DeepMerge(doc, expanded)
}

// Dotted leaf keys of a nested document
func flattenKeys(d map[string]any, prefix string, out map[string]bool) {
	for k, v := range d {
		dotted := k
		if prefix != "" {
			dotted = prefix + "." + k
		}
		if m, ok := v.(map[string]any); ok && len(m) > 0 {
			flattenKeys(m, dotted, out)
		} else {
			out[dotted] = true
		}
	}
}

// ManagedConfigKeys lists the dotted leaf keys the managed config pins
func ManagedConfigKeys() []string {
	set := map[string]bool{}
	flattenKeys(LoadManagedConfig(), "", set)
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// IsKeyManaged is true when the exact dotted key is pinned by the managed layer
func IsKeyManaged(dotted string) bool {
	set := map[string]bool{}
	flattenKeys(LoadManagedConfig(), "", set)
	return set[dotted]
}

// IsEnvManaged is true when the env var name is pinned by the managed .env layer
func IsEnvManaged(name string) bool {
	_, ok := LoadManagedEnv()[name]
	return ok
}

// PinnedTerminalBackend is the non empty terminal.backend the managed config pins, else empty
func PinnedTerminalBackend() string {
	terminal, _ := ApplyManagedOverlay(map[string]any{})["terminal"].(map[string]any)
	backend, _ := terminal["backend"].(string)
	return strings.TrimSpace(backend)
}
