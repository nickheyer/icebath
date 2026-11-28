package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Folder of the administrator's settings, for IT to name another than /etc/icebath
	ManagedDirEnv = "ICEBATH_MANAGED_DIR"
	// Package manager that installed icebath and keeps its settings, such as nixos or home-manager; false, 0, no, off or brew for none
	ManagedSystemEnv = "ICEBATH_MANAGED"
)

// Folder of the administrator's settings where no variable names one
const managedDefault = "/etc/icebath"

// File in the config folder naming the package manager that installed icebath, which the variable overrides
const managedMarker = ".managed"

// Name notices give the administrator's settings file
const administrator = "The administrator's config.yaml"

// Folder of the administrator's settings, empty when there is none: a config.yaml whose values pin settings over every profile's, and a .env
// whose variables win over every other
func ManagedDir() string {
	dir := strings.TrimSpace(os.Getenv(ManagedDirEnv))
	if dir == "" {
		dir = managedDefault
	}
	if !fsx.IsDir(dir) {
		return ""
	}
	return dir
}

// One of the administrator's files as last read, by the version of the file it was read from
type adminFile struct {
	stamp  string
	values map[string]any
}

// File of the administrator's folder by name, a generic label when there is no folder
func managedSource(name string) string {
	if dir := ManagedDir(); dir != "" {
		return filepath.Join(dir, name)
	}
	return "the managed scope"
}

// Values of one of the administrator's files and the version they were read from, read again only once it changes; a file that does not
// read is told loudly and pins nothing
func (c *core) adminRead(path string, parse func(data []byte) (map[string]any, error)) (map[string]any, string) {
	stamp := version(path)
	if stamp == "" {
		return nil, ""
	}
	c.adminMu.Lock()
	cached, ok := c.admin[path]
	c.adminMu.Unlock()
	if ok && cached.stamp == stamp {
		return cached.values, stamp
	}
	data, err := os.ReadFile(path)
	if err == nil {
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(data) {
			err = errors.New("the file holds text that is not UTF-8")
		}
	}
	var values map[string]any
	if err == nil {
		values, err = parse(data)
	}
	if err != nil {
		if c.once("managed", path, stamp) {
			slog.Warn("managed scope: failed to parse managed file, IGNORING it. Admin policy from this file is NOT being applied. Fix and restart.",
				"path", path, "error", err)
		}
		values = nil
	}
	c.adminMu.Lock()
	c.admin[path] = adminFile{stamp: stamp, values: values}
	c.adminMu.Unlock()
	return values, stamp
}

// Settings of the administrator's config.yaml: every name as the file spells it, matched as a key answers to any config.yaml's, nulls dropped
// since a null pins nothing, references read from this process's environment so a profile can't shadow a pinned value
func (c *core) adminConfig(data []byte) (map[string]any, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	m, ok := plain(doc).(map[string]any)
	if !ok {
		return nil, &shapeError{kind: kindOf(doc)}
	}
	values, _ := c.expand(pinnable(m), os.LookupEnv, func() string { return "the environment icebath started with" }).(map[string]any)
	return values, nil
}

// Mapping with its nulls dropped, with the sections they leave empty
func pinnable(m map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range m {
		if value == nil {
			continue
		}
		if child, ok := value.(map[string]any); ok && len(child) > 0 {
			kept := pinnable(child)
			if len(kept) == 0 {
				continue
			}
			value = kept
		}
		out[key] = value
	}
	return out
}

// Variables of the administrator's .env
func adminEnv(data []byte) (map[string]any, error) {
	out := map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := assignment(line); ok {
			out[name] = value
		}
	}
	return out, nil
}

// File of the administrator's settings that a profile on Remote Config ignores, empty when there is none
func (c *Config) ignoredPins() string {
	dir := ManagedDir()
	if dir == "" || !c.remote() {
		return ""
	}
	if path := filepath.Join(dir, "config.yaml"); fsx.Exists(path) {
		return path
	}
	return ""
}

// Values the administrator pins over every profile's settings, none when there is no such file or the profile's settings come from Remote
// Config, which takes its locks from the plane alone. Values that no setting takes are told once for each version of the file
func (c *Config) Pins() map[string]any {
	dir := ManagedDir()
	if dir == "" || c.remote() {
		return nil
	}
	path := filepath.Join(dir, "config.yaml")
	pins, stamp := c.adminRead(path, c.adminConfig)
	// Marked told before the walk, which asks sections that read pins themselves
	if len(pins) > 0 && c.once("pins told", path, stamp) {
		c.tellPins(pins)
	}
	return pins
}

// Variables the administrator sets for every profile, over every other, as the file is now
func (c *Config) ManagedEnv() map[string]string {
	dir := ManagedDir()
	if dir == "" {
		return nil
	}
	values, _ := c.adminRead(filepath.Join(dir, ".env"), adminEnv)
	out := make(map[string]string, len(values))
	for name, value := range values {
		out[name], _ = value.(string)
	}
	return out
}

// Variables of the administrator's .env as icebath loaded it, at its start or as its settings were last read again: what variables are read
// through, as a fixed file needs a restart to apply
func (c *core) loadedAdminEnv() map[string]string {
	c.envMu.Lock()
	defer c.envMu.Unlock()
	if c.adminEnv == nil {
		c.adminEnv = (&Config{core: c}).ManagedEnv()
		if c.adminEnv == nil {
			c.adminEnv = map[string]string{}
		}
	}
	return c.adminEnv
}

// Keys the administrator pins, each to the value at its end, the parts icebath declares spelled as declared, sorted
func (c *Config) ManagedKeys() []string {
	look := c.planner()
	var keys []string
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for key, value := range m {
			if child, ok := value.(map[string]any); ok && len(child) > 0 {
				walk(child, prefix+key+".")
				continue
			}
			keys = append(keys, look.path(prefix+key).String())
		}
	}
	walk(c.Pins(), "")
	slices.Sort(keys)
	return slices.Compact(keys)
}

// Value the administrator pins under the key, a mapping when they pin keys below it, false when they pin nothing there or for no key
func (c *Config) pinnedAt(key keyPath) (any, bool) {
	pins := c.Pins()
	if pins == nil || len(key.parts) == 0 {
		return nil, false
	}
	return lookup(pins, key)
}

// Whether the administrator pins the key itself or a key above it to a value rather than a mapping of values
func (c *Config) pinned(key keyPath) bool {
	var node any = c.Pins()
	if node == nil {
		return false
	}
	for i, part := range key.parts {
		m, ok := node.(map[string]any)
		if !ok {
			return true
		}
		name, ok := pickIn(m, part, key.match[i])
		if !ok {
			return false
		}
		node = m[name]
	}
	_, mapping := node.(map[string]any)
	return !mapping
}

// Tells once each value the administrator pins that no setting takes, or that the setting it is for refuses
func (c *Config) tellPins(pins map[string]any) {
	var walk func(m map[string]any, prefix string)
	walk = func(m map[string]any, prefix string) {
		for _, name := range slices.Sorted(maps.Keys(m)) {
			key, value := prefix+name, m[name]
			if _, setting, ok := c.Setting(key); ok {
				if _, err := setting.Check(value); err != nil && c.once("misfit", key, fmt.Sprint(value)) {
					slog.Warn(fmt.Sprintf("%s has %s set to %v, but %s. Using the default (%v) instead.", administrator, key, value, strs.Lower(err.Error()),
						setting.DefaultValue()))
				}
				continue
			}
			if child, ok := value.(map[string]any); ok && c.holdsSettings(key) {
				walk(child, key+".")
				continue
			}
			if c.once("unknown pin", key) {
				slog.Warn(administrator + " holds " + key + ", which is not a setting; it is ignored")
			}
		}
	}
	walk(pins, "")
}

// Whether a key is a section above at least one setting, its parts answering to declared ones in any case
func (c *Config) holdsSettings(key string) bool {
	for _, section := range c.Sections() {
		if same(section.Key, key, spelled) || below(section.Key, key, spelled) {
			return true
		}
		for _, setting := range section.Settings {
			if below(setting.Key, key, spelled) {
				return true
			}
		}
	}
	return false
}

// Package manager that installed icebath and keeps its settings, from ICEBATH_MANAGED or the marker in the config folder, empty when none does
func (c *Config) ManagedSystem() string {
	marker := strings.ToLower(strings.TrimSpace(os.Getenv(ManagedSystemEnv)))
	set := marker != ""
	// The marker of the config folder, else of the profile's folder
	for _, dir := range []string{c.dir, c.ProfileDir(c.Profile())} {
		if path := filepath.Join(dir, managedMarker); !set && fsx.Exists(path) {
			set = true
			if data, err := os.ReadFile(path); err == nil {
				marker = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(strings.ReplaceAll(string(data), "\r\n", "\n"), "�")))
			}
		}
	}
	switch marker {
	case "brew", "homebrew", "false", "0", "no", "off":
		return ""
	case "", "true", "1", "yes":
		if !set {
			return ""
		}
		return "nixos"
	}
	return marker
}

// Who decides a key that can't be changed here
const (
	ByAdministrator  = "administrator"
	ByRemote         = "remote"
	ByPackageManager = "package manager"
)

// Change refused because something above the person decides the key: the administrator's pins, a level of Remote Config, or the package
// manager that installed icebath
type LockedError struct {
	Key string
	By  string
	// File of the administrator's that pins it, the level of Remote Config that locks it, or the package manager that keeps the settings
	Source string
}

// Why the key can't be changed, as the refusal ends
func (e *LockedError) Reason() string {
	switch e.By {
	case ByRemote:
		return "The " + e.Source + " level of the remote settings locks this."
	case ByPackageManager:
		return "This copy of icebath is managed by " + e.Source + ", which keeps its settings. Change them through " + e.Source + "."
	}
	return "Your administrator sets this in " + e.Source + "."
}

// Refusal of a change of the key, or of the settings as a whole for a lock that names no key
func (e *LockedError) Refusal() string {
	if e.By == ByPackageManager || e.Key == "" {
		return "Settings can't be changed here. " + e.Reason()
	}
	return e.Key + " can't be changed here. " + e.Reason()
}

func (e *LockedError) Error() string { return e.Refusal() }

// Why the key can't be changed here, nil when it can: the administrator pins it, a level of Remote Config locks it, or, for a setting a person
// changes or a part of a table setting, the administrator sets a variable the setting reads or the package manager that installed icebath
// keeps the settings
func (c *Config) Lock(key string) *LockedError {
	look := c.planner()
	return c.lock(look, look.path(key))
}

// Why the key as the settings keep it can't be changed here, nil when it can, its setting found through the finder
func (c *Config) lock(look *finder, path keyPath) *LockedError {
	key := path.String()
	if c.pinned(path) {
		return &LockedError{Key: key, By: ByAdministrator, Source: managedSource("config.yaml")}
	}
	_, setting, known := look.setting(key)
	if !known {
		setting, _, known = look.under(key)
	}
	if known && !setting.Internal {
		managed := c.ManagedEnv()
		for _, variable := range setting.Env {
			if _, ok := managed[variable]; ok {
				return &LockedError{Key: key, By: ByAdministrator, Source: managedSource(".env")}
			}
		}
		if system := c.ManagedSystem(); system != "" {
			return &LockedError{Key: key, By: ByPackageManager, Source: system}
		}
	}
	if store, err := c.store(key); err == nil {
		if level := store.locked(path); level != "" {
			return &LockedError{Key: key, By: ByRemote, Source: level}
		}
	}
	return nil
}

// Why the variable can't be written to or removed from the profile's .env, nil when it can: the package manager that installed icebath keeps
// the settings, or the administrator's .env sets it
func (c *Config) envLock(name string) *LockedError {
	if system := c.ManagedSystem(); system != "" {
		return &LockedError{Key: name, By: ByPackageManager, Source: system}
	}
	if _, ok := c.ManagedEnv()[name]; ok {
		return &LockedError{Key: name, By: ByAdministrator, Source: managedSource(".env")}
	}
	return nil
}

// Refuses an edit of a config file of an install a package manager keeps, nil when it can be edited
func (c *Config) EditLock() *LockedError {
	if system := c.ManagedSystem(); system != "" {
		return &LockedError{By: ByPackageManager, Source: system}
	}
	return nil
}

// Value of a pin as the setting takes it, its default when the setting refuses it
func pinValue(setting Setting, known bool, pin any) any {
	if !known {
		return pin
	}
	checked, err := setting.Check(pin)
	if err != nil {
		return setting.DefaultValue()
	}
	return checked
}
