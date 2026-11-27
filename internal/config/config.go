package config

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"
	"github.com/spf13/viper"

	"github.com/nickheyer/icebath/internal/config/remote"
	"github.com/nickheyer/icebath/pkg/fsx"
)

type Kind int

const (
	Text Kind = iota
	Number
	Toggle
	Choice
	List
	Table
	// Length of time, kept in config.yaml as a number of its unit and written as one or as text such as 30s or 5m
	Duration
)

type Source int

const (
	FromDefault Source = iota
	FromFile
	FromEnvironment
	// The administrator pins it
	FromManaged
)

// Folder of the profiles under the config folder, one folder each
const profiles = "profiles"

// Variable naming where every profile's settings come from: file, the config.yaml of its folder, or remote, Remote Config's plane
const BackendEnv = "ICEBATH_CONFIG_BACKEND"

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
	Key   string
	Title string
	// One sentence saying what it sets
	About string
	// Up to two sentences more than About: an example, where its value is found, what follows from it
	Help    string
	Default any
	// Environment variables that set it, the first one set winning over the file
	Env []string
	// Values it may take, read each time they are shown: one of them for text, any of them for a list
	Options func() []Option
	// Text that is never shown again once kept
	Secret bool
	// Kept by icebath itself, and left out of the settings a person goes through
	Internal bool
	// Changed by few people, and listed behind Advanced on the settings screen
	Advanced bool
	// When a change takes hold, said with the setting when it is not at once
	Apply Apply
	// Command that changes it as well, without its slash
	Slash string
	// What is in effect while it holds its zero value, shown in place of that value, such as No limit: the zero value is 0, empty text, an
	// empty list or an empty table
	Zero string
	// Value Zero speaks of when it is not the zero value of the kind, such as -1
	ZeroValue any
	// What a number counts, in the plural, shown after it: tokens, characters, MB. A count of bytes is shown in the largest unit that fits,
	// a share as a percentage and a number that names a thing digit for digit
	Counts string
	// Skill that asks for the value, for a setting a skill declares, said where the setting is explained
	Skill string
	// A field of an entry that the entry is nothing without: its form does not save while the field is empty
	Need bool
	// What text must start with, one of them, such as https:// for an address; any text when empty
	Starts []string
	// Least and most a number may be, no bound when nil; the value Zero speaks of is taken whatever they are
	Least, Most *float64
	// What one entry of a table holds, nil for a table whose entries are free. On a list whose default is a list of anything, what one
	// record of the list holds
	Entry *Entry
	// Unit a length of time is kept in, as a number of it: a second when zero
	Unit time.Duration
	// Least and most a length of time may be, no bound when nil
	Min, Max *time.Duration
	// A stored length of time under the least gives way to the default rather than to the least
	DefaultBelow bool
	// What a length of time left unset means, such as no limit, when it has no default: the setting then holds nothing until one is written
	Null string
}

// When a change of a setting takes hold
type Apply int

const (
	// Nothing is said of it: at once, or not looked into
	ApplyUnsaid Apply = iota
	// From the next message sent
	NextMessage
	// In the sessions started after it
	NewSessions
	// Once icebath is started again
	Restart
)

// What a number counts that is not shown as it is counted
const (
	// Bytes, shown in the largest unit that fits
	Bytes = "bytes"
	// Part of a whole between 0 and 1, shown as a percentage
	Share = "share"
	// Nothing: the number names a thing, as a port or a seed does, and is shown digit for digit with no grouping
	Plain = "plain"
)

// Bound of a number
func Num(n float64) *float64 {
	return &n
}

// What one entry of a table setting holds, so the settings screen changes it in fields
type Entry struct {
	// What one entry is, singular and lower case, such as shortcut or server
	Noun string
	// What the name of an entry is called where one is asked for, Name when empty
	Name string
	// The names of the entries are names of variables: read in capitals whatever case the file keeps them in, and shown so
	Capitals bool
	// Fields of an entry, each keyed by its name inside the entry; an entry that is one value has a single field with an empty key
	Fields []Setting
	// The names an entry may have, each with what a person calls it, for a table whose entries are known things such as events: a new
	// entry is picked from them and every entry shows by its title. Any name when nil
	Names func() []Option
}

// Group of the first settings screen a section at the top is listed under
type Group string

const (
	GroupModels    Group = "models"
	GroupSafety    Group = "safety"
	GroupAbilities Group = "abilities"
	GroupMedia     Group = "media"
	GroupVoice     Group = "voice"
	GroupSessions  Group = "sessions"
	GroupSecrets   Group = "secrets"
	GroupApp       Group = "app"
	// Sections that name no group, such as a plugin's
	GroupMore Group = ""
)

// Heading of a group
type GroupInfo struct {
	ID    Group
	Title string
}

// Groups in the order the first settings screen lists them
func Groups() []GroupInfo {
	return []GroupInfo{
		{GroupModels, "Models"},
		{GroupSafety, "Safety"},
		{GroupAbilities, "What the agent can do"},
		{GroupMedia, "Images and video"},
		{GroupVoice, "Voice"},
		{GroupSessions, "Sessions"},
		{GroupSecrets, "Keys and passwords"},
		{GroupApp, "This app"},
		{GroupMore, "More"},
	}
}

// Place of a group among the groups, the last place for one that is not known
func (g Group) place() int {
	groups := Groups()
	for i, group := range groups {
		if group.ID == g {
			return i
		}
	}
	return len(groups) - 1
}

type Section struct {
	// Place of the section in the tree, and so in config.yaml: the key of its parent and one more name, or one name at the top
	Key string
	// Section it is listed under, the top when empty
	Parent string
	Title  string
	// Phrase saying what is set there, no period
	About string
	// Group of the first settings screen a section at the top is listed under, More when empty
	Group Group
	// Place among the sections beside it, lowest first, those of one place by title
	Order int
	// Kept once for every profile, in the config.yaml at the top of the config folder rather than in the profile's. Only a section at the
	// top says so, those under it following it
	Global   bool
	Settings []Setting
}

// Sections in the order the settings screen lists them: those at the top by group, then all by their place beside each other and their
// title
func Listed(sections []Section) []Section {
	listed := slices.Clone(sections)
	slices.SortStableFunc(listed, func(a, b Section) int {
		if a.Parent == "" && b.Parent == "" {
			if byGroup := cmp.Compare(a.Group.place(), b.Group.place()); byGroup != 0 {
				return byGroup
			}
		}
		return cmp.Or(strings.Compare(a.Parent, b.Parent), cmp.Compare(a.Order, b.Order),
			strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)))
	})
	return listed
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
	case []string, []any:
		return List
	case map[string]any:
		return Table
	case time.Duration:
		return Duration
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
		return nil, fmt.Errorf("%s takes on or off.", s.Title)
	case Number:
		var number any
		switch n := value.(type) {
		case int:
			number = n
		case int64:
			number = int(n)
		case float64:
			if _, fraction := s.Default.(float64); fraction {
				number = n
			} else if n == math.Trunc(n) {
				number = int(n)
			}
		}
		if number == nil {
			if _, fraction := s.Default.(float64); fraction {
				return nil, fmt.Errorf("%s must be a number.", s.Title)
			}
			return nil, fmt.Errorf("%s must be a whole number.", s.Title)
		}
		if err := s.within(number); err != nil {
			return nil, err
		}
		return number, nil
	case Text, Choice:
		var text string
		switch scalar := value.(type) {
		case string:
			text = scalar
		case int, int64, float64, bool:
			text = fmt.Sprint(scalar)
		default:
			return nil, fmt.Errorf("%s must be text.", s.Title)
		}
		if !s.option(text) {
			return nil, fmt.Errorf("%s must be one of: %s.", s.Title, s.allowed())
		}
		if err := s.started(text); err != nil {
			return nil, err
		}
		return text, nil
	case List:
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be a list.", s.Title)
		}
		// A list of records holds groups of named values, kept as they are
		if _, records := s.Default.([]any); records {
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok && s.Entry != nil && len(s.Entry.Fields) > 0 {
					return nil, fmt.Errorf("Each of the %s must be a set of named values.", strings.ToLower(s.Title))
				}
			}
			return items, nil
		}
		list := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of text.", s.Title)
			}
			if !s.option(text) {
				return nil, fmt.Errorf("%s can only hold: %s.", s.Title, s.allowed())
			}
			if !slices.Contains(list, text) {
				list = append(list, text)
			}
		}
		return list, nil
	case Duration:
		return s.checkLength(value)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a set of named values.", s.Title)
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
	shared layer
	// Profile in use, the one settings are read in unless another is named
	active string
	// Settings of each profile read so far, by name: its config.yaml, or what Remote Config holds for it
	profiles map[string]layer
	// Variables profiles read beyond their .env and this process's, nil reading those alone
	environment Environment
	// .env of each profile read so far, by path; what each has beside it, by name; the administrator's .env as loaded
	envMu    sync.Mutex
	dotenvs  map[string]envFile
	layers   map[string]envLayers
	adminEnv map[string]string
	// Backend the settings of this process come from once a profile is in use, chosen once as a deployment is
	backend string
	// The administrator's files as last read, by path
	adminMu sync.Mutex
	admin   map[string]adminFile
	// Remote Config of the profiles whose settings its plane holds
	plane *remote.Backend
	// Notices given, each told once
	toldMu sync.Mutex
	told   map[string]bool
	// Names of the variables that start a program of icebath's, which the .env writer refuses, nil before the provider host tells them
	launching func() []string
}

// Whether the notice is given for the first time, which tells it
func (c *core) once(parts ...string) bool {
	id := strings.Join(parts, "\x00")
	c.toldMu.Lock()
	defer c.toldMu.Unlock()
	if c.told[id] {
		return false
	}
	c.told[id] = true
	return true
}

// Settings of one profile over those every profile shares: the profile in use, or the one named with In. Values come from the environment
// first, then the file of their scope, then their defaults
type Config struct {
	*core
	// Profile the settings are of, empty for the one in use
	profile string
	// Variables come from the .env and this process alone, not the environment profiles read through
	plain bool
	// What one session sets over the files, nil for none
	session *Layer
}

func New(dir string) *Config {
	c := &Config{core: &core{dir: dir, global: map[string]bool{}, profiles: map[string]layer{}, dotenvs: map[string]envFile{},
		layers: map[string]envLayers{}, admin: map[string]adminFile{}, plane: remote.New(), told: map[string]bool{}}}
	c.shared = newFile(c.core, filepath.Join(dir, "config.yaml"))
	c.Register(general())
	return c
}

// Stops polling Remote Config
func (c *Config) Close() error {
	c.plane.Close()
	return nil
}

// Backend every profile's settings come from: file, or remote; an error naming the variable when it names neither. The variable is read from
// this process, the administrator's .env and the .env files of the profile in use, once that profile is known, as a deployment is chosen once
// for the process
func (c *Config) Backend() (string, error) {
	c.mu.RLock()
	name, active := c.backend, cmp.Or(c.active, c.profile)
	c.mu.RUnlock()
	if name == "" {
		if active != "" {
			name = strings.ToLower(strings.TrimSpace(c.In(active).Plain().Getenv(BackendEnv)))
		} else {
			name = strings.ToLower(strings.TrimSpace(cmp.Or(c.loadedAdminEnv()[BackendEnv], os.Getenv(BackendEnv))))
		}
		name = cmp.Or(name, "file")
		if active != "" {
			c.mu.Lock()
			c.backend = name
			c.mu.Unlock()
		}
	}
	switch name {
	case "file", "remote":
		return name, nil
	default:
		return "", fmt.Errorf("%s is set to %q, and settings can only be kept in \"file\" or \"remote\".", BackendEnv, name)
	}
}

// Variables no secret store may supply: while the profile's settings come from Remote Config, the one choosing it and those that decide which
// bearer goes to which plane
func (c *Config) ProtectedEnv() []string {
	if !c.remote() {
		return nil
	}
	return append([]string{BackendEnv}, remote.CredentialEnv...)
}

// Whether the profile's settings come from Remote Config
func (c *Config) remote() bool {
	name, err := c.Backend()
	return err == nil && name == "remote"
}

// Settings of the profile with the name, the one in use for no name. They share everything with these but the profile
func (c *Config) In(profile string) *Config {
	return &Config{core: c.core, profile: profile, plain: c.plain}
}

// Settings of the profile with the name, the one in use for no name, an error saying so when there is no profile of the name
func (c *Config) Of(profile string) (*Config, error) {
	if profile == "" {
		return c, nil
	}
	if !c.Exists(profile) {
		return nil, fmt.Errorf("There is no profile named %s. Run `icebath profile list` to see your profiles.", profile)
	}
	return c.In(profile), nil
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

// Reads the settings every profile shares, and again those of every profile read so far. A config.yaml that does not read or parse is told
// and served from its last good read, so only Remote Config failing can fail it
func (c *Config) Load() error {
	c.mu.RLock()
	layers := append([]layer{c.shared}, slices.Collect(maps.Values(c.profiles))...)
	c.mu.RUnlock()
	var errs []error
	for _, l := range layers {
		errs = append(errs, l.load())
	}
	c.forgetDotenv("")
	return errors.Join(errs...)
}

// Uses the settings of the profile with the name from now on, wherever no other profile is named; a profile whose .env does not read is
// refused, unless the administrator pins terminal.backend
func (c *Config) Use(name string) error {
	if !Named(name) {
		return fmt.Errorf("%q can't be the name of a profile. Use lower case letters, digits, dashes and underscores.", name)
	}
	if _, err := c.In(name).own(); err != nil {
		return err
	}
	if err := c.In(name).readableEnv(); err != nil {
		return err
	}
	c.mu.Lock()
	c.active = name
	c.mu.Unlock()
	// Once, as Remote Config first serves the profile in use: an administrator's config.yaml it takes nothing from
	if path := c.ignoredPins(); path != "" && c.once("ignored", path) {
		message := path + " has no effect. While ICEBATH_CONFIG_BACKEND is remote, settings and locks come only from the remote settings. Remove " +
			"the file."
		tell("⚠ "+message, message)
	}
	return nil
}

// Forgets the settings read of a profile, once it is deleted or renamed
func (c *Config) Forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.profiles, name)
	c.forgetDotenv(name)
	c.plane.Forget(name)
}

// Settings of the profile the settings are of, read the first time they are asked for: its config.yaml, or what Remote Config holds for
// it, which fails closed when the plane can't be reached
func (c *Config) own() (layer, error) {
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
	backend, err := c.Backend()
	if err != nil {
		return nil, err
	}
	if backend == "remote" {
		plain := c.In(name).Plain()
		profile, err := c.plane.Profile(context.Background(), name, plain.Getenv)
		if err != nil {
			return nil, err
		}
		own = &remoteLayer{profile: profile}
	} else {
		f := newFile(c.core, filepath.Join(c.ProfileDir(name), "config.yaml"))
		f.load()
		own = f
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if have, ok := c.profiles[name]; ok {
		return have, nil
	}
	c.profiles[name] = own
	return own, nil
}

// Where writes of the key go, as a person reads them: a config.yaml, or the profile's level of Remote Config
func (c *Config) Destination(key string) (string, error) {
	store, err := c.store(key)
	if err != nil {
		return "", err
	}
	return store.where(), nil
}

// Remote Config of the profile as it stands, empty when its settings come from its config.yaml
func (c *Config) RemoteState() string {
	store, err := c.own()
	if err != nil {
		return ""
	}
	if r, ok := store.(*remoteLayer); ok {
		return r.profile.Describe()
	}
	return ""
}

// File of the settings every profile shares and of the profile's own, empty for one Remote Config holds
func (c *Config) Files() (shared, own string) {
	shared = filepath.Join(c.dir, "config.yaml")
	if !c.remote() {
		own = filepath.Join(c.ProfileDir(c.Profile()), "config.yaml")
	}
	return shared, own
}

// Adds sections. A fixed section is checked as it comes: its key must be its place in the tree under a parent registered before it, only a
// section at the top may say it is global, and a key registered twice is a mistake of the code. All of them panic
func (c *Config) Register(registrant Registrant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fixed, ok := registrant.(Sections)
	if !ok {
		c.registry.Register(registrant)
		return
	}
	// One at a time, so a section's parent may be an earlier one of its own batch
	for _, section := range fixed {
		c.registry.Register(Sections{c.check(section)})
	}
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
		if _, _, taken := find(Sections{{Key: key, Settings: section.Settings[:i]}}, setting.Key); taken {
			panic("config: setting " + setting.Key + " is registered twice")
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

// Table setting a key below it names a part of, such as one entry or a field of one, with the dotted path of the part inside the table;
// false when no table setting is above the key
func (c *Config) Under(key string) (Setting, string, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(key)), ".")
	for n := len(parts) - 1; n > 0; n-- {
		if _, setting, ok := c.Setting(strings.Join(parts[:n], ".")); ok {
			if setting.Kind() != Table {
				break
			}
			return setting, strings.Join(parts[n:], "."), true
		}
	}
	return Setting{}, "", false
}

// Part of a value of tables under the dotted path, nil when it holds nothing there
func Dig(value any, path string) any {
	for _, part := range strings.Split(path, ".") {
		table, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = table[part]
	}
	return value
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

// Settings of the scope the key is kept in
func (c *Config) store(key string) (layer, error) {
	if c.Global(key) {
		return c.shared, nil
	}
	return c.own()
}

// Where the value of a setting comes from, and the environment variable when it is one
func (c *Config) Source(setting Setting) (Source, string) {
	if _, pinned := c.pinnedAt(setting.Key); pinned {
		return FromManaged, ""
	}
	for _, variable := range setting.Env {
		if value := c.Getenv(variable); strings.TrimSpace(value) != "" {
			if _, ok := c.fromEnv(setting, variable, value); ok {
				return FromEnvironment, variable
			}
		}
	}
	if value, ok := c.layered(setting.Key); ok {
		if value != nil {
			return FromFile, ""
		}
		return FromDefault, ""
	}
	store, err := c.store(setting.Key)
	if err != nil {
		return FromDefault, ""
	}
	if store.holds(setting.Key) {
		return FromFile, ""
	}
	return FromDefault, ""
}

// Value kept under the key, nil when there is none, without looking at environment variables or defaults: what the file of its scope holds,
// the administrator's pins over it. Sections that come and go read what they are made of with it
func (c *Config) Stored(key string) any {
	key = strings.ToLower(key)
	value := c.stored(key)
	if pin, pinned := c.pinnedAt(key); pinned {
		return overlay(value, pin)
	}
	return value
}

// Value the session's layer or the file of the key's scope holds, as written
func (c *Config) stored(key string) any {
	if value, ok := c.layered(key); ok {
		return value
	}
	store, err := c.store(key)
	if err != nil {
		return nil
	}
	return store.get(key)
}

// Value of the key: what the administrator pins, else the first environment variable of its setting set to more than spaces, else the file
// with the variables its text references read from the profile's environment, else the default of its setting. A pin of keys below a table
// setting adds them to the value the rest gives; a key below a table setting is the part of the table's value under it. A length of time is
// the number of its unit it comes to, its default for a value that spells none
func (c *Config) Get(key string) any {
	key = strings.ToLower(key)
	_, setting, known := c.Setting(key)
	if !known {
		if table, path, under := c.Under(key); under {
			return Dig(c.Get(table.Key), path)
		}
	}
	pin, pinned := c.pinnedAt(key)
	_, partial := pin.(map[string]any)
	partial = partial && (!known || setting.Kind() == Table)
	if pinned && !partial {
		return pinValue(setting, known, pin)
	}
	value := func() any {
		if known {
			for _, variable := range setting.Env {
				if value := c.Getenv(variable); strings.TrimSpace(value) != "" {
					if typed, ok := c.fromEnv(setting, variable, value); ok {
						return typed
					}
				}
			}
		}
		if value := c.stored(key); value != nil {
			value = c.expand(value, c.LookupEnv, func() string { return display(c.EnvPath()) })
			if !known || setting.Kind() != Duration {
				return value
			}
			if length, ok := setting.readLength(value); ok {
				return length
			}
		}
		if known {
			return setting.DefaultValue()
		}
		return nil
	}()
	if pinned {
		return overlay(value, pin)
	}
	return value
}

// Words a variable may set a toggle with
var toggleWords = map[string]bool{"1": true, "on": true, "true": true, "yes": true, "y": true, "enable": true, "enabled": true,
	"0": false, "off": false, "false": false, "no": false, "n": false, "disable": false, "disabled": false}

// Value of a variable as the setting takes it: a toggle's on or off, a number's number, a length of time's number of its unit, any other's
// text. One the setting can't take is told once and ignored, so the file or the default applies
func (c *Config) fromEnv(setting Setting, variable, text string) (any, bool) {
	word := strings.ToLower(strings.TrimSpace(text))
	takes := ""
	switch setting.Kind() {
	case Toggle:
		if on, ok := toggleWords[word]; ok {
			return on, true
		}
		takes = "takes on or off"
	case Number:
		if _, fraction := setting.Default.(float64); fraction {
			if f, err := strconv.ParseFloat(word, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
			takes = "takes a number"
		} else if n, err := strconv.Atoi(word); err == nil {
			return n, true
		} else {
			takes = "takes a whole number"
		}
	case Duration:
		if length, ok := setting.readLength(text); ok {
			return length, true
		}
		takes = "takes a length of time such as 30s or 5m"
	default:
		return text, true
	}
	if c.once("env", variable, text) {
		slog.Warn(fmt.Sprintf("The %s environment variable is set to %q, but %s %s. It is ignored.", variable, text, setting.Key, takes))
	}
	return nil, false
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

// Length of time the setting under the key holds, its number counting the setting's unit, seconds for a setting that names none; zero when
// it holds no length
func (c *Config) GetDuration(key string) time.Duration {
	_, setting, _ := c.Setting(key)
	length, _ := Length(c.Get(key), setting.unit())
	return length
}

// Whether a value decoded from YAML or JSON counts as given: true, a number other than zero, a text, list or map that is not empty, and
// anything else but nil
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return value.Float() != 0
	case reflect.Slice, reflect.Map, reflect.Array, reflect.String:
		return value.Len() > 0
	}
	return true
}

// Whether a variable's text says yes: 1, true, yes or on in any case, spaces around it ignored
func Yes(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Saves the value under the key in the settings of its scope, nil removing the key and all under it; a *LockedError when something above the
// person decides the key, or when the administrator pins keys below it that a value other than a mapping would replace, and the refusal of a
// config.yaml that does not read or parse rather than replacing it
func (c *Config) Set(key string, value any) error {
	key = strings.ToLower(key)
	if locked := c.Lock(key); locked != nil {
		return locked
	}
	if _, below := c.pinnedAt(key); below && value != nil {
		if _, mapping := value.(map[string]any); !mapping {
			return &LockedError{Key: key, By: ByAdministrator, Source: managedSource("config.yaml")}
		}
	}
	store, err := c.store(key)
	if err != nil {
		return err
	}
	return store.set(key, value)
}

// Why a write of the key can't be saved now, nil when it can: the refusal of a config.yaml of its scope that does not read or parse, which a
// write would replace
func (c *Config) Writable(key string) error {
	store, err := c.store(strings.ToLower(key))
	if err != nil {
		return err
	}
	return store.writable()
}

// Whether the settings of the key's scope hold a value under it, which unsetting it removes
func (c *Config) Holds(key string) bool {
	store, err := c.store(strings.ToLower(key))
	return err == nil && store.holds(strings.ToLower(key))
}

// Everything the file of a scope holds: the settings every profile shares, or the profile's own
func (c *Config) Held(global bool) map[string]any {
	if global {
		return c.shared.all()
	}
	store, err := c.own()
	if err != nil {
		return nil
	}
	return store.all()
}

// Writes settings to a config file at once, so a reader never sees half of them
func Write(path string, settings map[string]any) error {
	changed := viper.New()
	if err := changed.MergeConfigMap(settings); err != nil {
		return err
	}
	changed.SetConfigType(strings.TrimPrefix(filepath.Ext(path), "."))
	var data bytes.Buffer
	if err := changed.WriteConfigTo(&data); err != nil {
		return err
	}
	return fsx.Write(path, data.Bytes(), 0o600)
}

// Keys under one prefix, read and written by the names below it, such as the settings of one provider
type View struct {
	config *Config
	prefix string
	// Holds nothing, the profile's environment the one thing read through it
	bare bool
}

// View of the same profile that holds nothing and reads only its environment: what an account with no fields of its own is
func (v View) Bare() View {
	return View{config: v.config, bare: true}
}

// Variable of the environment of the view's profile, of this process for a view of no profile
func (v View) LookupEnv(name string) (string, bool) {
	if v.config == nil {
		return os.LookupEnv(name)
	}
	return v.config.LookupEnv(name)
}

// Every variable of the environment of the view's profile, of this process for a view of no profile
func (v View) Environ() map[string]string {
	if v.config == nil {
		return Process()
	}
	return v.config.Environ()
}

// Profile the view reads, empty for a view of no profile
func (v View) Profile() string {
	if v.config == nil {
		return ""
	}
	return v.config.Profile()
}

// Store the variable of the view's profile came from, empty for its .env, this process or a view of no profile
func (v View) EnvOrigin(name string) string {
	if v.config == nil {
		return ""
	}
	return v.config.EnvOrigin(name)
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
	if v.config == nil || v.bare {
		return nil
	}
	if name == "" {
		return v.config.Stored(v.prefix)
	}
	return v.config.Stored(v.Key(name))
}

func (v View) Get(name string) any {
	if v.config == nil || v.bare {
		return nil
	}
	return v.config.Get(v.Key(name))
}

func (v View) GetString(name string) string        { return cast.ToString(v.Get(name)) }
func (v View) GetInt(name string) int              { return cast.ToInt(v.Get(name)) }
func (v View) GetBool(name string) bool            { return cast.ToBool(v.Get(name)) }
func (v View) GetStringSlice(name string) []string { return cast.ToStringSlice(v.Get(name)) }

func (v View) Set(name string, value any) error {
	if v.config == nil || v.bare {
		return errors.New("config: nothing to keep " + name + " under")
	}
	return v.config.Set(v.Key(name), value)
}

// Removes everything under the prefix
func (v View) Remove() error {
	if v.config == nil || v.bare {
		return nil
	}
	return v.config.Set(v.prefix, nil)
}

// Whole number a value decoded from JSON or YAML holds: an integer, a number with nothing after its point, or text of one
func Whole(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), v == float64(int(v))
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}
