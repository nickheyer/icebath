package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cast"
	"github.com/spf13/viper"
)

type Kind int

const (
	Text Kind = iota
	Number
	Toggle
	Choice
	List
	Table
)

type Source int

const (
	FromDefault Source = iota
	FromFile
	FromEnvironment
)

// Folder of the profiles under the config folder, one folder each
const profiles = "profiles"

// Name a key can hold as one of its parts, such as a profile, a provider or an account: lower case letters, digits, dashes and underscores.
// Keys are split on dots and folded to lower case, so a name holds neither
var named = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Whether the name can be one part of a key
func Named(name string) bool {
	return named.MatchString(name)
}

type Option struct {
	Value string
	Title string
	About string
}

type Setting struct {
	Key     string
	Title   string
	About   string
	Default any
	// Environment variables that set it, the first one set winning over the file
	Env []string
	// Values it may take, read each time they are shown: one of them for text, any of them for a list
	Options func() []Option
	// Text that is never shown again once kept
	Secret bool
	// Kept by icebath itself, and left out of the settings a person goes through
	Internal bool
}

type Section struct {
	// Place of the section in the tree, and so in config.yaml: the key of its parent and one more name, or one name at the top
	Key string
	// Section it is listed under, the top when empty
	Parent string
	Title  string
	About  string
	// Kept once for every profile, in the config.yaml at the top of the config folder rather than in the profile's. Only a section at the
	// top says so, those under it following it
	Global   bool
	Settings []Setting
}

func Options(options ...Option) func() []Option {
	return func() []Option { return options }
}

func (s Setting) Kind() Kind {
	switch s.Default.(type) {
	case bool:
		return Toggle
	case int, int64, float64:
		return Number
	case []string:
		return List
	case map[string]any:
		return Table
	}
	if s.Options != nil {
		return Choice
	}
	return Text
}

func (s Setting) option(value string) bool {
	return s.Options == nil || slices.ContainsFunc(s.Options(), func(option Option) bool { return option.Value == value })
}

func (s Setting) allowed() string {
	var values []string
	for _, option := range s.Options() {
		values = append(values, cmp.Or(option.Value, `""`))
	}
	return strings.Join(values, ", ")
}

// Value as the setting keeps it, from one read out of text. Nil puts the default back
func (s Setting) Check(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch kind := s.Kind(); kind {
	case Toggle:
		if on, ok := value.(bool); ok {
			return on, nil
		}
		return nil, fmt.Errorf("%s must be true or false", s.Title)
	case Number:
		switch number := value.(type) {
		case int:
			return number, nil
		case int64:
			return int(number), nil
		case float64:
			if _, fraction := s.Default.(float64); fraction {
				return number, nil
			}
			if number == math.Trunc(number) {
				return int(number), nil
			}
		}
		return nil, fmt.Errorf("%s must be a whole number", s.Title)
	case Text, Choice:
		var text string
		switch scalar := value.(type) {
		case string:
			text = scalar
		case int, int64, float64, bool:
			text = fmt.Sprint(scalar)
		default:
			return nil, fmt.Errorf("%s must be text", s.Title)
		}
		if !s.option(text) {
			return nil, fmt.Errorf("%s must be one of: %s", s.Title, s.allowed())
		}
		return text, nil
	case List:
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be a list", s.Title)
		}
		list := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of text", s.Title)
			}
			if !s.option(text) {
				return nil, fmt.Errorf("%s can only hold: %s", s.Title, s.allowed())
			}
			if !slices.Contains(list, text) {
				list = append(list, text)
			}
		}
		return list, nil
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a table", s.Title)
	}
	return value, nil
}

// What adds sections: a fixed list of them, or sections that come and go with what the settings of a profile hold, such as one for each
// provider it has. Those are made for the profile whose settings are read
type Registrant interface {
	Sections(values *Config) []Section
}

// Sections fixed once they are registered
type Sections []Section

func (s Sections) Sections(*Config) []Section {
	return s
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

// Every section, those that come and go made for the profile of the values
func (r *Registry) Sections(values *Config) []Section {
	var sections []Section
	for _, registrant := range r.registrants {
		sections = append(sections, registrant.Sections(values)...)
	}
	return sections
}

func (r *Registry) Section(values *Config, key string) (Section, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, section := range r.Sections(values) {
		if section.Key == key {
			return section, true
		}
	}
	return Section{}, false
}

// Setting registered under the key, with the section it belongs to. Fixed sections are looked through first, so finding one of them
// never asks the sections that come and go
func (r *Registry) Setting(values *Config, key string) (Section, Setting, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	var moving []Registrant
	for _, registrant := range r.registrants {
		fixed, ok := registrant.(Sections)
		if !ok {
			moving = append(moving, registrant)
			continue
		}
		if section, setting, ok := find(fixed, key); ok {
			return section, setting, true
		}
	}
	for _, registrant := range moving {
		if section, setting, ok := find(registrant.Sections(values), key); ok {
			return section, setting, true
		}
	}
	return Section{}, Setting{}, false
}

func find(sections []Section, key string) (Section, Setting, bool) {
	for _, section := range sections {
		if !strings.HasPrefix(key, section.Key+".") {
			continue
		}
		for _, setting := range section.Settings {
			if setting.Key == key {
				return section, setting, true
			}
		}
	}
	return Section{}, Setting{}, false
}

// Settings of every profile and those they share, read and written by any number of them at once
type core struct {
	registry Registry
	dir      string
	mu       sync.RWMutex
	// Top keys of the sections every profile shares
	global map[string]bool
	// config.yaml at the top of the config folder
	shared *viper.Viper
	// Profile in use, the one settings are read in unless another is named
	active string
	// config.yaml of each profile read so far, by name
	profiles map[string]*viper.Viper
}

// Settings of one profile over those every profile shares: the profile in use, or the one named with In. Values come from the environment
// first, then the file of their scope, then their defaults
type Config struct {
	*core
	// Profile the settings are of, empty for the one in use
	profile string
}

func New(dir string) *Config {
	return &Config{core: &core{dir: dir, global: map[string]bool{}, shared: file(filepath.Join(dir, "config.yaml")), profiles: map[string]*viper.Viper{}}}
}

func file(path string) *viper.Viper {
	v := viper.New()
	v.SetConfigFile(path)
	return v
}

// Settings of the profile with the name, the one in use for no name. They share everything with these but the profile
func (c *Config) In(profile string) *Config {
	return &Config{core: c.core, profile: profile}
}

// Folder of the settings every profile shares, and of the profiles
func (c *Config) Dir() string {
	return c.dir
}

// Folder of the profile with the name
func (c *Config) ProfileDir(name string) string {
	return filepath.Join(c.dir, profiles, name)
}

// Whether the profile with the name is there
func (c *Config) Exists(profile string) bool {
	if !Named(profile) {
		return false
	}
	info, err := os.Stat(c.ProfileDir(profile))
	return err == nil && info.IsDir()
}

// Profile the settings are of
func (c *Config) Profile() string {
	if c.profile != "" {
		return c.profile
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

// Reads the settings every profile shares, and again those of every profile read so far
func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := []error{read(c.shared)}
	for _, own := range c.profiles {
		errs = append(errs, read(own))
	}
	return errors.Join(errs...)
}

// Uses the settings of the profile with the name from now on, wherever no other profile is named
func (c *Config) Use(name string) error {
	if !Named(name) {
		return fmt.Errorf("%q can't name a profile: use lower case letters, digits, dashes or underscores", name)
	}
	if _, err := c.In(name).own(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = name
	return nil
}

// Forgets the settings read of a profile, once it is deleted
func (c *Config) Forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.profiles, name)
}

// File of the profile the settings are of, read the first time it is asked for
func (c *Config) own() (*viper.Viper, error) {
	name := c.Profile()
	if name == "" {
		return nil, errors.New("config: no profile is in use")
	}
	c.mu.RLock()
	own, ok := c.profiles[name]
	c.mu.RUnlock()
	if ok {
		return own, nil
	}
	own = file(filepath.Join(c.ProfileDir(name), "config.yaml"))
	if err := read(own); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if have, ok := c.profiles[name]; ok {
		return have, nil
	}
	c.profiles[name] = own
	return own, nil
}

// Adds sections. A fixed section is checked as it comes: its key must be its place in the tree under a parent registered before it, only a
// section at the top may say it is global, and a key registered twice is a mistake of the code. All of them panic
func (c *Config) Register(registrant Registrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fixed, ok := registrant.(Sections); ok {
		for i, section := range fixed {
			fixed[i] = c.check(section)
		}
	}
	c.registry.Register(registrant)
}

func (c *Config) check(section Section) Section {
	section.Key, section.Parent = strings.ToLower(section.Key), strings.ToLower(section.Parent)
	key := section.Key
	switch name, _ := strings.CutPrefix(key, section.Parent+"."); {
	case section.Parent == "" && strings.Contains(key, "."):
		panic("config: section " + key + " is at the top, so its key is one name")
	case section.Parent != "" && (name == key || strings.Contains(name, ".")):
		panic("config: section " + key + " is under " + section.Parent + ", so its key is " + section.Parent + " and one more name")
	case section.Parent != "" && section.Global:
		panic("config: section " + key + " is under " + section.Parent + ", whose scope it follows")
	}
	if section.Parent != "" {
		if _, ok := c.fixed(section.Parent); !ok {
			panic("config: section " + key + " is under " + section.Parent + ", which isn't registered")
		}
	}
	if _, taken := c.fixed(key); taken {
		panic("config: section " + key + " is registered twice")
	}
	for i, setting := range section.Settings {
		setting.Key = strings.ToLower(setting.Key)
		section.Settings[i] = setting
		if !strings.HasPrefix(setting.Key, key+".") {
			panic("config: setting " + setting.Key + " is in section " + key + ", so its key starts with it")
		}
		for _, registrant := range c.registry.registrants {
			if fixed, ok := registrant.(Sections); ok {
				if _, _, taken := find(fixed, setting.Key); taken {
					panic("config: setting " + setting.Key + " is registered twice")
				}
			}
		}
	}
	if section.Global {
		c.global[key] = true
	}
	return section
}

// Fixed section with the key
func (c *Config) fixed(key string) (Section, bool) {
	for _, registrant := range c.registry.registrants {
		if fixed, ok := registrant.(Sections); ok {
			for _, section := range fixed {
				if section.Key == key {
					return section, true
				}
			}
		}
	}
	return Section{}, false
}

// Every section, those that come and go as the profile has them now
func (c *Config) Sections() []Section {
	return c.snapshot().Sections(c)
}

func (c *Config) Section(key string) (Section, bool) {
	return c.snapshot().Section(c, key)
}

func (c *Config) Setting(key string) (Section, Setting, bool) {
	return c.snapshot().Setting(c, key)
}

// Registry as it is now, read without holding the lock, since sections that come and go read settings to make themselves
func (c *Config) snapshot() *Registry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &Registry{registrants: slices.Clone(c.registry.registrants)}
}

// Whether the key is kept once for every profile
func (c *Config) Global(key string) bool {
	root, _, _ := strings.Cut(strings.ToLower(key), ".")
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.global[root]
}

// File the key is kept in
func (c *Config) store(key string) (*viper.Viper, error) {
	if c.Global(key) {
		return c.shared, nil
	}
	return c.own()
}

// Where the value of a setting comes from, and the environment variable when it is one
func (c *Config) Source(setting Setting) (Source, string) {
	for _, variable := range setting.Env {
		if os.Getenv(variable) != "" {
			return FromEnvironment, variable
		}
	}
	store, err := c.store(setting.Key)
	if err != nil {
		return FromDefault, ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if store.InConfig(setting.Key) {
		return FromFile, ""
	}
	return FromDefault, ""
}

// Value kept in the file under the key, nil when there is none, without looking at environment variables or defaults. Sections that come
// and go read what they are made of with it
func (c *Config) Stored(key string) any {
	key = strings.ToLower(key)
	store, err := c.store(key)
	if err != nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return store.Get(key)
}

// Value of the key: from the first environment variable set of its setting, else the file, else the default of its setting
func (c *Config) Get(key string) any {
	key = strings.ToLower(key)
	_, setting, known := c.Setting(key)
	if known {
		for _, variable := range setting.Env {
			if value := os.Getenv(variable); value != "" {
				return value
			}
		}
	}
	if value := c.Stored(key); value != nil {
		return value
	}
	if known {
		return setting.Default
	}
	return nil
}

func (c *Config) GetString(key string) string {
	return cast.ToString(c.Get(key))
}

func (c *Config) GetInt(key string) int {
	return cast.ToInt(c.Get(key))
}

func (c *Config) GetBool(key string) bool {
	return cast.ToBool(c.Get(key))
}

func (c *Config) GetStringSlice(key string) []string {
	return cast.ToStringSlice(c.Get(key))
}

// Saves the value under the key in the file of its scope, nil removing the key and all under it
func (c *Config) Set(key string, value any) error {
	key = strings.ToLower(key)
	store, err := c.store(key)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	path := store.ConfigFileUsed()
	stored := file(path)
	if err := stored.ReadInConfig(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	settings := stored.AllSettings()
	parts := strings.Split(key, ".")
	node, last := settings, len(parts)-1
	for _, part := range parts[:last] {
		child, ok := node[part].(map[string]any)
		if !ok {
			if value == nil {
				return read(store)
			}
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
	if err := Write(path, settings); err != nil {
		return err
	}
	return read(store)
}

// Writes settings to a config file at once, so a reader never sees half of them
func Write(path string, settings map[string]any) error {
	changed := viper.New()
	if err := changed.MergeConfigMap(settings); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "config.*"+filepath.Ext(path))
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
	return os.Rename(f.Name(), path)
}

func read(v *viper.Viper) error {
	if v.ConfigFileUsed() == "" {
		return nil
	}
	err := v.ReadInConfig()
	var notFound viper.ConfigFileNotFoundError
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.As(err, &notFound) {
		return err
	}
	return nil
}

// Keys under one prefix, read and written by the names below it, such as the settings of one provider
type View struct {
	config *Config
	prefix string
}

func (c *Config) View(prefix string) View {
	return View{config: c, prefix: strings.ToLower(prefix)}
}

func (v View) Prefix() string {
	return v.prefix
}

// Full key of the name under the prefix
func (v View) Key(name string) string {
	return v.prefix + "." + strings.ToLower(name)
}

func (v View) View(name string) View {
	return View{config: v.config, prefix: v.Key(name)}
}

// Names of the tables kept under the prefix, in order
func (v View) Names() []string {
	table, _ := v.Stored("").(map[string]any)
	var names []string
	for _, name := range slices.Sorted(maps.Keys(table)) {
		if _, ok := table[name].(map[string]any); ok {
			names = append(names, name)
		}
	}
	return names
}

// Whether anything is kept under the prefix
func (v View) Exists() bool {
	return v.Stored("") != nil
}

// Value kept in the file under the name, the prefix itself for no name, without environment variables or defaults: what sections that
// come and go read. A view of nothing holds nothing
func (v View) Stored(name string) any {
	if v.config == nil {
		return nil
	}
	if name == "" {
		return v.config.Stored(v.prefix)
	}
	return v.config.Stored(v.Key(name))
}

func (v View) Get(name string) any {
	if v.config == nil {
		return nil
	}
	return v.config.Get(v.Key(name))
}

func (v View) GetString(name string) string        { return cast.ToString(v.Get(name)) }
func (v View) GetInt(name string) int              { return cast.ToInt(v.Get(name)) }
func (v View) GetBool(name string) bool            { return cast.ToBool(v.Get(name)) }
func (v View) GetStringSlice(name string) []string { return cast.ToStringSlice(v.Get(name)) }

func (v View) Set(name string, value any) error {
	if v.config == nil {
		return errors.New("config: nothing to keep " + name + " under")
	}
	return v.config.Set(v.Key(name), value)
}

// Removes everything under the prefix
func (v View) Remove() error {
	if v.config == nil {
		return nil
	}
	return v.config.Set(v.prefix, nil)
}
