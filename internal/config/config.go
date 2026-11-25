package config

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	dir   string
	mu    sync.RWMutex
	viper *viper.Viper
}

func New(dir string) *Config {
	v := viper.New()
	v.SetConfigName("config")
	v.AddConfigPath(dir)
	return &Config{dir: dir, viper: v}
}

func (c *Config) Dir() string {
	return c.dir
}

func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read()
}

func (c *Config) SetDefault(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viper.SetDefault(key, value)
}

func (c *Config) MustBindEnv(key string, variables ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viper.MustBindEnv(append([]string{key}, variables...)...)
}

func (c *Config) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := c.viper.AllKeys()
	slices.Sort(keys)
	return keys
}

func (c *Config) Get(key string) any {
	return get(c, (*viper.Viper).Get, key)
}

func (c *Config) GetString(key string) string {
	return get(c, (*viper.Viper).GetString, key)
}

func (c *Config) GetInt(key string) int {
	return get(c, (*viper.Viper).GetInt, key)
}

func (c *Config) GetBool(key string) bool {
	return get(c, (*viper.Viper).GetBool, key)
}

func (c *Config) GetStringSlice(key string) []string {
	return get(c, (*viper.Viper).GetStringSlice, key)
}

func (c *Config) Set(key string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := cmp.Or(c.viper.ConfigFileUsed(), filepath.Join(c.dir, "config.yaml"))
	stored := viper.New()
	stored.SetConfigFile(path)
	if err := stored.ReadInConfig(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	settings := stored.AllSettings()
	parts := strings.Split(strings.ToLower(key), ".")
	node, last := settings, len(parts)-1
	for _, part := range parts[:last] {
		child, ok := node[part].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[part] = child
		}
		node = child
	}
	if value == nil {
		delete(node, parts[last])
	} else {
		node[parts[last]] = value
	}
	changed := viper.New()
	if err := changed.MergeConfigMap(settings); err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.dir, "config.*"+filepath.Ext(path))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Close(); err != nil {
		return err
	}
	if err := changed.WriteConfigAs(f.Name()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return c.read()
}

func (c *Config) read() error {
	var notFound viper.ConfigFileNotFoundError
	if err := c.viper.ReadInConfig(); err != nil && !errors.As(err, &notFound) {
		return err
	}
	return nil
}

func get[T any](c *Config, read func(*viper.Viper, string) T, key string) T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return read(c.viper, key)
}
