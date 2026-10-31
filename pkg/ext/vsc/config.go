package vsc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Name the extension registers under, its settings file under the configuration directory and the variable naming the socket
const (
	Name       = "vsc"
	ConfigFile = "ext/vsc.json"
	EnvVar     = "ICEBATH_ACP"
)

// Everything the bridge reads before it attaches
type Config struct {
	// Connect to the editor window the TUI runs in, nothing of the bridge runs when off
	Enabled bool `json:"enabled"`
	// File every protocol line is appended to, no traffic log when empty
	Log string `json:"log,omitempty"`
	// Install or update the editor extension through the code command when a VS Code terminal lacks it
	AutoInstall bool `json:"auto_install"`
}

// Settings a fresh file starts from, the bridge on and auto install off
func Default() Config { return Config{Enabled: true} }

// Every setting as text keyed by name
func (c Config) Values() map[string]string {
	return map[string]string{"enabled": strconv.FormatBool(c.Enabled), "auto_install": strconv.FormatBool(c.AutoInstall), "log": c.Log}
}

// Sets one setting from text, empty text returning it to its default, refusing an unknown key or a bad value
func (c *Config) Set(key, value string) error {
	value = strings.TrimSpace(value)
	defaults := Default()
	switch key {
	case "enabled":
		return setFlag(&c.Enabled, key, value, defaults.Enabled)
	case "auto_install":
		return setFlag(&c.AutoInstall, key, value, defaults.AutoInstall)
	case "log":
		c.Log = value
	default:
		return fmt.Errorf("unknown setting %s", key)
	}
	return nil
}

// Sets a switch from text, empty text returning it to its default
func setFlag(into *bool, key, value string, def bool) error {
	if value == "" {
		*into = def
		return nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s must be true or false, got %q", key, value)
	}
	*into = b
	return nil
}

// File the settings live in, under the user configuration directory
func Path() (string, error) {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(ConfigFile)), nil
}

// Reads the settings file over the defaults, the defaults alone when there is no file
func Load() (Config, error) {
	c := Default()
	path, err := Path()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Writes the settings file, readable by the owner alone
func Save(c Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

var (
	cfgMu  sync.RWMutex
	cfg    Config
	cfgErr error
	loaded bool
)

// Settings in force, read from the file on first use and the defaults when the file cannot be read
func Current() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if !loaded {
		cfg, cfgErr = Load()
		loaded = true
	}
	return cfg
}

// Error of the first read of the settings file, nil when it read or was absent
func LoadError() error {
	Current()
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfgErr
}

// Saves the settings, makes them the ones in force and starts or stops the bridge of the running app to match
func Apply(c Config) error {
	if err := Save(c); err != nil {
		return err
	}
	cfgMu.Lock()
	cfg, cfgErr, loaded = c, nil, true
	cfgMu.Unlock()
	return applyToBridge(c)
}

// Socket the variable names, false when it is unset or the bridge is off
func SocketRequested() (string, bool) {
	value := strings.TrimSpace(os.Getenv(EnvVar))
	if value == "" || !Current().Enabled {
		return "", false
	}
	return value, true
}
