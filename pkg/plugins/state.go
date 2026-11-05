package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var (
	pluginSettingSegmentRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	pluginSettingReservedRoots = map[string]bool{"model": true, "plugins": true, "security": true, "settings": true}
	pluginStateKeyRe           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	portableNamespaceRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,191}$`)
	pluginStateLocks           = map[string]*sync.Mutex{}
	pluginStateLocksGuard      sync.Mutex
)

const pluginStateQuotaBytes = 10 * 1024 * 1024

// Refusal raised when a managed install or administrator owned key blocks a plugin settings write
type PluginPermissionError struct{ Msg string }

func (e *PluginPermissionError) Error() string { return e.Msg }

// Validated segments of a plugin relative settings key, refusing global, reserved and traversal paths
func PluginRelativeSegments(key string) ([]string, error) {
	segments := strings.Split(key, ".")
	invalid := key == "" || strings.ContainsAny(key, `/\`) || pluginSettingReservedRoots[strings.ToLower(segments[0])]
	if !invalid {
		for _, s := range segments {
			if !pluginSettingSegmentRe.MatchString(s) {
				invalid = true
				break
			}
		}
	}
	if invalid {
		return nil, errors.New("Expected a plugin-relative config key such as 'endpoint' or " +
			"'retry.policy'; global, cross-plugin, and traversal paths are forbidden")
	}
	return segments, nil
}

// Value at segments through nested mappings, found false on the first miss
func nestedPluginValue(root any, segments []string) (any, bool) {
	current := root
	for _, s := range segments {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = m[s]; !ok {
			return nil, false
		}
	}
	return current, true
}

// Value wrapped in nested single key mappings with the first segment outermost
func nestedPluginMapping(segments []string, value any) any {
	nested := value
	for i := len(segments) - 1; i >= 0; i-- {
		nested = map[string]any{segments[i]: nested}
	}
	return nested
}

// The plugins.entries mapping of one plugin, nil when absent or not a mapping
func pluginSettingsEntry(cfg any, pluginID string) map[string]any {
	v, ok := nestedPluginValue(cfg, []string{"plugins", "entries", pluginID})
	if !ok {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// Writes one value under plugins.entries.<id>.settings atomically, refusing managed installs and keys
func SavePluginSetting(h, pluginID string, segments []string, value any) error {
	if config.ManagedSystem(h) != "" {
		return &PluginPermissionError{Msg: "Plugin settings cannot be changed in a managed install"}
	}
	fullPath := append([]string{"plugins", "entries", pluginID, "settings"}, segments...)
	dotted := strings.Join(fullPath, ".")
	if config.IsKeyManaged(dotted) {
		return &PluginPermissionError{Msg: fmt.Sprintf("Plugin setting %s is administrator-managed", config.PyRepr(dotted))}
	}
	partial := nestedPluginMapping(fullPath[:4], nestedPluginMapping(segments, value)).(map[string]any)
	configPath := home.ConfigPath(h)
	return withLockedPluginState(configPath, func() error {
		ctx, unlock := config.LockConfig(context.Background())
		defer unlock()
		if _, err := config.ReadUserConfigRaw(ctx, configPath); err != nil {
			return err
		}
		return config.SaveConfig(ctx, h, partial, config.SaveOptions{PreserveKeys: [][]string{fullPath}, MergeExisting: true})
	})
}

// One Windows safe directory component for plugin owned data, portable namespaces kept verbatim
func pluginDataNamespace(pluginID, dataNamespace string) string {
	candidate := dataNamespace
	if candidate == "" {
		candidate = pluginID
	}
	if dataNamespace != "" && strings.HasPrefix(candidate, "agent-plugin-") && portableNamespaceRe.MatchString(candidate) {
		return candidate
	}
	return portableDataNamespace(candidate)
}

// Runs fn while holding the thread and cross process lock of a sibling .<name>.lock file
func withLockedPluginState(path string, fn func() error) error {
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
	abs, err := filepath.Abs(lockPath)
	if err != nil {
		abs = lockPath
	}
	pluginStateLocksGuard.Lock()
	mu, ok := pluginStateLocks[abs]
	if !ok {
		mu = &sync.Mutex{}
		pluginStateLocks[abs] = mu
	}
	pluginStateLocksGuard.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return err
	}
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	defer handle.Close()
	release, err := lockStateFile(handle)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// Atomic quota bounded JSON key value state owned by one plugin
type PluginState struct {
	home      string
	namespace string
}

// State facade of one plugin under a home, keyed by its data namespace
func NewPluginState(h, pluginID, dataNamespace string) *PluginState {
	return &PluginState{home: h, namespace: pluginDataNamespace(pluginID, dataNamespace)}
}

// Profile scoped directory matching the PLUGIN_DATA of portable plugins
func (s *PluginState) DataDir() string { return filepath.Join(home.PluginDataDir(s.home), s.namespace) }

// The state.json file inside the data directory
func (s *PluginState) Path() string { return filepath.Join(s.DataDir(), "state.json") }

// Largest encoded state document a plugin may store
func (s *PluginState) QuotaBytes() int { return pluginStateQuotaBytes }

// Refuses keys outside the 1 to 128 character safe alphabet or containing a double dot
func validateStateKey(key string) error {
	if !pluginStateKeyRe.MatchString(key) || strings.Contains(key, "..") {
		return errors.New("Plugin state keys must be 1-128 characters using letters, " +
			"numbers, '_', '-', '.', or ':' (without '..')")
	}
	return nil
}

// Decoded state document, empty when the file is missing
func (s *PluginState) readUnlocked() (map[string]any, error) {
	raw, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Cannot parse plugin state %s: %v", s.Path(), err)
	}
	dec := json.NewDecoder(bytes.NewReader(stripBOM(raw)))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("Cannot parse plugin state %s: %v", s.Path(), err)
	}
	if dec.More() {
		return nil, fmt.Errorf("Cannot parse plugin state %s: Extra data", s.Path())
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Cannot parse plugin state %s: root must be an object", s.Path())
	}
	return pyJSONNumbers(m).(map[string]any), nil
}

// Decoded numbers turned into int for integer literals and float64 otherwise, oversized integers kept exact
func pyJSONNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		text := x.String()
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.Atoi(text); err == nil {
				return n
			}
			return x
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x
	case map[string]any:
		for k, item := range x {
			x[k] = pyJSONNumbers(item)
		}
	case []any:
		for i, item := range x {
			x[i] = pyJSONNumbers(item)
		}
	}
	return v
}

// Stored JSON value of a key, def when absent
func (s *PluginState) Get(key string, def any) (any, error) {
	if err := validateStateKey(key); err != nil {
		return nil, err
	}
	var out any
	err := withLockedPluginState(s.Path(), func() error {
		data, err := s.readUnlocked()
		if err != nil {
			return err
		}
		v, ok := data[key]
		if !ok {
			v = def
		}
		out = v
		return nil
	})
	return out, err
}

// Sets one JSON value atomically under the lock, refusing values that break the quota
func (s *PluginState) Set(key string, value any) error {
	if err := validateStateKey(key); err != nil {
		return err
	}
	return withLockedPluginState(s.Path(), func() error {
		data, err := s.readUnlocked()
		if err != nil {
			return err
		}
		data[key] = value
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(data); err != nil {
			return fmt.Errorf("Plugin state value for %s is not JSON-serializable", config.PyRepr(key))
		}
		encoded := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		if len(encoded) > s.QuotaBytes() {
			return fmt.Errorf("Plugin state quota exceeded: %d bytes is greater than the %d-byte per-plugin quota",
				len(encoded), s.QuotaBytes())
		}
		return config.AtomicWrite(s.Path(), encoded, 0o600)
	})
}
