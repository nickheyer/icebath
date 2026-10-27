package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Name of the project directory and of the files inside it and the user configuration directory
const (
	ProjectDir  = ".icebath"
	ConfigFile  = "config.json"
	RulesFile   = "rules.json"
	HooksFile   = "hooks.json"
	SessionsDir = "sessions"
)

// Environment variables that override the chosen profile and model
const (
	ProfileEnv = "ICEBATH_PROFILE"
	ModelEnv   = "ICEBATH_MODEL"
)

// Profile and model pair naming where compaction and child agents run
type Target struct {
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
}

// Compaction settings
type Compaction struct {
	// Context tokens at which old turns are summarized, zero disables compaction
	Threshold int `json:"threshold,omitempty"`
	// Messages kept verbatim after the summary
	Keep int `json:"keep,omitempty"`
	// Profile and model writing summaries, the main model when empty
	Model *Target `json:"model,omitempty"`
	// Extra guidance appended to the summary instructions
	Instructions string `json:"instructions,omitempty"`
}

// Subagent settings
type Subagents struct {
	MaxDepth int  `json:"max_depth,omitempty"`
	Parallel bool `json:"parallel,omitempty"`
	// Seconds one child may run
	Timeout int `json:"timeout,omitempty"`
	// Profile and model children use, the main one when empty
	Model *Target `json:"model,omitempty"`
}

// Shell settings
type ShellConfig struct {
	// Seconds a command may run when the model names no timeout
	Timeout int `json:"timeout,omitempty"`
	// Seconds the model may ask for at most
	MaxTimeout int `json:"max_timeout,omitempty"`
	// KEY=VALUE pairs every command sees
	Env []string `json:"env,omitempty"`
}

// Web fetch settings
type WebConfig struct {
	// Disables the fetch tool
	Disabled bool `json:"disabled,omitempty"`
}

// Tool settings
type ToolsConfig struct {
	// Tool names withheld from the model and refused when called
	Disabled []string `json:"disabled,omitempty"`
}

// Appearance settings
type UIConfig struct {
	// Theme name such as blackwall, light, contrast or plain
	Theme string `json:"theme,omitempty"`
	// Motion scale from zero for snapping to one for full springs
	Motion *float64 `json:"motion,omitempty"`
	// Main slot share of the width
	Ratio float64 `json:"ratio,omitempty"`
	// Cascade lights the focused card's border with a flowing energy comet
	Cascade *bool `json:"cascade,omitempty"`
}

// Everything the shell app and the headless runner need, read from files, environment and flags
type Config struct {
	Profile string `json:"profile,omitempty"`
	Model   string `json:"model,omitempty"`
	// System prompt text, or the file it is read from
	System     string `json:"system,omitempty"`
	SystemFile string `json:"system_file,omitempty"`
	// Permission mode such as default, acceptEdits, plan or bypassPermissions
	Mode        string                     `json:"mode,omitempty"`
	MaxTokens   int                        `json:"max_tokens,omitempty"`
	MaxRounds   int                        `json:"max_rounds,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"`
	Extra       map[string]json.RawMessage `json:"extra,omitempty"`
	Compaction  Compaction                 `json:"compaction,omitempty"`
	Permissions policy.RuleSet             `json:"permissions,omitempty"`
	Hooks       agent.HookConfig           `json:"hooks,omitempty"`
	MCP         []ServerConfig             `json:"mcp,omitempty"`
	Subagents   Subagents                  `json:"subagents,omitempty"`
	Shell       ShellConfig                `json:"shell,omitempty"`
	Web         WebConfig                  `json:"web,omitempty"`
	Tools       ToolsConfig                `json:"tools,omitempty"`
	UI          UIConfig                   `json:"ui,omitempty"`
	// Values of the settings extensions register, by key
	Settings map[string]string `json:"settings,omitempty"`
	// Project root for hooks, rules and servers, the working directory when empty
	Root string `json:"root,omitempty"`
	// Directory sessions are written to, sessions under the configuration directory when empty
	SessionsDir string `json:"sessions_dir,omitempty"`
	// Extra profile lists loaded before the user list
	ProfileFiles []string `json:"profile_files,omitempty"`
	// Catalog files loaded over the built in one
	CatalogFiles []string `json:"catalog_files,omitempty"`
	// Files the configuration was read from, in load order
	Sources []string `json:"-"`
}

// Reads one configuration file, ignoring keys it does not know
func ReadConfig(r io.Reader) (Config, error) {
	var c Config
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if c.Mode != "" {
		if _, err := policy.ParseMode(c.Mode); err != nil {
			return Config{}, err
		}
	}
	if err := c.Hooks.Validate(); err != nil {
		return Config{}, err
	}
	for key, value := range c.Settings {
		if s, ok := LookupSetting(key); ok {
			if err := s.Validate(value); err != nil {
				return Config{}, fmt.Errorf("config: settings: %w", err)
			}
		}
	}
	return c, nil
}

func ReadConfigFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	c, err := ReadConfig(f)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c.Sources = []string{path}
	return c, nil
}

// Lays over onto base, scalars replacing when set and lists joining
func Merge(base, over Config) Config {
	out := base
	if over.Profile != "" {
		out.Profile = over.Profile
	}
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.System != "" {
		out.System = over.System
	}
	if over.SystemFile != "" {
		out.SystemFile = over.SystemFile
	}
	if over.Mode != "" {
		out.Mode = over.Mode
	}
	if over.MaxTokens != 0 {
		out.MaxTokens = over.MaxTokens
	}
	if over.MaxRounds != 0 {
		out.MaxRounds = over.MaxRounds
	}
	if over.Temperature != nil {
		out.Temperature = over.Temperature
	}
	if len(over.Extra) > 0 {
		merged := make(map[string]json.RawMessage, len(base.Extra)+len(over.Extra))
		for k, v := range base.Extra {
			merged[k] = v
		}
		for k, v := range over.Extra {
			merged[k] = v
		}
		out.Extra = merged
	}
	if over.Compaction.Threshold != 0 {
		out.Compaction.Threshold = over.Compaction.Threshold
	}
	if over.Compaction.Keep != 0 {
		out.Compaction.Keep = over.Compaction.Keep
	}
	if over.Compaction.Model != nil {
		out.Compaction.Model = over.Compaction.Model
	}
	if over.Compaction.Instructions != "" {
		out.Compaction.Instructions = over.Compaction.Instructions
	}
	out.Permissions = policy.RuleSet{
		Allow: append(append([]string(nil), base.Permissions.Allow...), over.Permissions.Allow...),
		Deny:  append(append([]string(nil), base.Permissions.Deny...), over.Permissions.Deny...),
		Ask:   append(append([]string(nil), base.Permissions.Ask...), over.Permissions.Ask...),
	}
	if len(base.Hooks) > 0 || len(over.Hooks) > 0 {
		out.Hooks = agent.HookConfig{}
		for ev, groups := range base.Hooks {
			out.Hooks[ev] = append(out.Hooks[ev], groups...)
		}
		for ev, groups := range over.Hooks {
			out.Hooks[ev] = append(out.Hooks[ev], groups...)
		}
	}
	if len(over.MCP) > 0 {
		byName := map[string]int{}
		var merged []ServerConfig
		for _, s := range base.MCP {
			byName[s.Name] = len(merged)
			merged = append(merged, s)
		}
		for _, s := range over.MCP {
			if i, ok := byName[s.Name]; ok {
				merged[i] = s
				continue
			}
			byName[s.Name] = len(merged)
			merged = append(merged, s)
		}
		out.MCP = merged
	}
	if over.Subagents.MaxDepth != 0 {
		out.Subagents.MaxDepth = over.Subagents.MaxDepth
	}
	if over.Subagents.Parallel {
		out.Subagents.Parallel = true
	}
	if over.Subagents.Timeout != 0 {
		out.Subagents.Timeout = over.Subagents.Timeout
	}
	if over.Subagents.Model != nil {
		out.Subagents.Model = over.Subagents.Model
	}
	if over.Shell.Timeout != 0 {
		out.Shell.Timeout = over.Shell.Timeout
	}
	if over.Shell.MaxTimeout != 0 {
		out.Shell.MaxTimeout = over.Shell.MaxTimeout
	}
	if len(over.Shell.Env) > 0 {
		out.Shell.Env = append(append([]string(nil), base.Shell.Env...), over.Shell.Env...)
	}
	if over.Web.Disabled {
		out.Web.Disabled = true
	}
	if len(over.Tools.Disabled) > 0 {
		out.Tools.Disabled = slices.Compact(slices.Sorted(slices.Values(append(append([]string(nil), base.Tools.Disabled...), over.Tools.Disabled...))))
	}
	if over.UI.Theme != "" {
		out.UI.Theme = over.UI.Theme
	}
	if over.UI.Motion != nil {
		out.UI.Motion = over.UI.Motion
	}
	if over.UI.Ratio != 0 {
		out.UI.Ratio = over.UI.Ratio
	}
	if over.UI.Cascade != nil {
		out.UI.Cascade = over.UI.Cascade
	}
	if len(over.Settings) > 0 {
		merged := make(map[string]string, len(base.Settings)+len(over.Settings))
		maps.Copy(merged, base.Settings)
		maps.Copy(merged, over.Settings)
		out.Settings = merged
	}
	if over.Root != "" {
		out.Root = over.Root
	}
	if over.SessionsDir != "" {
		out.SessionsDir = over.SessionsDir
	}
	out.ProfileFiles = append(append([]string(nil), base.ProfileFiles...), over.ProfileFiles...)
	out.CatalogFiles = append(append([]string(nil), base.CatalogFiles...), over.CatalogFiles...)
	out.Sources = append(append([]string(nil), base.Sources...), over.Sources...)
	return out
}

// Configuration files a start under dir reads, the user one first
func configLayers(dir string) ([]string, error) {
	userDir, err := profiles.ConfigDir()
	if err != nil {
		return nil, err
	}
	layers := []string{filepath.Join(userDir, ConfigFile)}
	if dir != "" {
		layers = append(layers, filepath.Join(dir, ProjectDir, ConfigFile))
	}
	return layers, nil
}

// Reads the user configuration, then the project configuration under dir, then the environment, later layers winning
func Discover(dir string) (Config, error) {
	var out Config
	userDir, err := profiles.ConfigDir()
	if err != nil {
		return Config{}, err
	}
	layers, err := configLayers(dir)
	if err != nil {
		return Config{}, err
	}
	for _, path := range layers {
		c, err := ReadConfigFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Config{}, err
		}
		out = Merge(out, c)
	}
	side := []string{userDir}
	if dir != "" {
		side = append(side, filepath.Join(dir, ProjectDir))
	}
	for _, base := range side {
		rulesPath := filepath.Join(base, RulesFile)
		set, err := policy.LoadRulesFile(rulesPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return Config{}, fmt.Errorf("%s: %w", rulesPath, err)
		default:
			out = Merge(out, Config{Permissions: set, Sources: []string{rulesPath}})
		}
		hooksPath := filepath.Join(base, HooksFile)
		cfg, err := agent.LoadHooksFile(hooksPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return Config{}, fmt.Errorf("%s: %w", hooksPath, err)
		default:
			out = Merge(out, Config{Hooks: cfg, Sources: []string{hooksPath}})
		}
	}
	return out.WithEnv(), nil
}

// Applies the environment overrides
func (c Config) WithEnv() Config {
	if v := os.Getenv(ProfileEnv); v != "" {
		c.Profile = v
	}
	if v := os.Getenv(ModelEnv); v != "" {
		c.Model = v
	}
	return c
}

// Directory sessions are stored in, created on demand
func (c Config) SessionsPath() (string, error) {
	if c.SessionsDir != "" {
		return filepath.Abs(c.SessionsDir)
	}
	dir, err := profiles.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SessionsDir), nil
}

// System prompt from the inline text or the named file
func (c Config) SystemPrompt() (string, error) {
	if c.SystemFile != "" {
		data, err := os.ReadFile(c.SystemFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return c.System, nil
}

// Value of an extension setting, empty while unset
func (c Config) Setting(key string) string { return c.Settings[key] }

// Reads a duration given in seconds
func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// Parses a numeric or bare value for a setting override such as max_tokens=4096
func ParseSetting(s string) (string, string, error) {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" || v == "" {
		return "", "", fmt.Errorf("expected KEY=VALUE, got %q", s)
	}
	return k, v, nil
}

// Applies a KEY=VALUE override to the configuration, an empty value unsetting the setting
func (c *Config) Set(key, value string) error {
	_, err := c.settingField(key, value)
	return err
}

// Applies a setting and returns the field it fills in a configuration file, an empty value removing the field
func (c *Config) settingField(key, value string) (Field, error) {
	path, err := settingPath(key)
	if err != nil {
		return Field{}, err
	}
	if value == "" {
		c.unset(key)
		return Field{Path: path, Unset: true}, nil
	}
	switch key {
	case "max_tokens":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return Field{}, fmt.Errorf("max_tokens must be a positive integer, got %q", value)
		}
		c.MaxTokens = n
		return Field{Path: path, Value: n}, nil
	case "max_rounds":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return Field{}, fmt.Errorf("max_rounds must be a positive integer, got %q", value)
		}
		c.MaxRounds = n
		return Field{Path: path, Value: n}, nil
	case "temperature":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 {
			return Field{}, fmt.Errorf("temperature must be a non negative number, got %q", value)
		}
		c.Temperature = &f
		return Field{Path: path, Value: f}, nil
	case "compact_threshold":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return Field{}, fmt.Errorf("compact_threshold must be a non negative integer, got %q", value)
		}
		c.Compaction.Threshold = n
		return Field{Path: path, Value: n}, nil
	case "mode":
		if _, err := policy.ParseMode(value); err != nil {
			return Field{}, err
		}
		c.Mode = value
		return Field{Path: path, Value: value}, nil
	case "theme":
		c.UI.Theme = value
		return Field{Path: path, Value: value}, nil
	case "motion":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 1 {
			return Field{}, fmt.Errorf("motion must be between 0 and 1, got %q", value)
		}
		c.UI.Motion = &f
		return Field{Path: path, Value: f}, nil
	case "cascade":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return Field{}, fmt.Errorf("cascade must be true or false, got %q", value)
		}
		c.UI.Cascade = &b
		return Field{Path: path, Value: b}, nil
	}
	if s, ok := LookupSetting(key); ok {
		if err := s.Validate(value); err != nil {
			return Field{}, err
		}
		values := maps.Clone(c.Settings)
		if values == nil {
			values = map[string]string{}
		}
		values[key] = value
		c.Settings = values
		return Field{Path: path, Value: value}, nil
	}
	name := strings.TrimPrefix(key, "extra.")
	extra := maps.Clone(c.Extra)
	if extra == nil {
		extra = map[string]json.RawMessage{}
	}
	raw := json.RawMessage(value)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(value)
	}
	extra[name] = raw
	c.Extra = extra
	return Field{Path: path, Value: raw}, nil
}

// Path of the field a setting fills in a configuration file
func settingPath(key string) ([]string, error) {
	switch key {
	case "max_tokens", "max_rounds", "temperature", "mode":
		return []string{key}, nil
	case "compact_threshold":
		return []string{"compaction", "threshold"}, nil
	case "theme", "motion", "cascade":
		return []string{"ui", key}, nil
	}
	if name, ok := strings.CutPrefix(key, "extra."); ok && name != "" {
		return []string{"extra", name}, nil
	}
	if _, ok := LookupSetting(key); ok {
		return []string{"settings", key}, nil
	}
	return nil, fmt.Errorf("unknown setting %q", key)
}

// Clears the value a setting holds so its default applies
func (c *Config) unset(key string) {
	switch key {
	case "max_tokens":
		c.MaxTokens = 0
	case "max_rounds":
		c.MaxRounds = 0
	case "temperature":
		c.Temperature = nil
	case "compact_threshold":
		c.Compaction.Threshold = 0
	case "mode":
		c.Mode = ""
	case "theme":
		c.UI.Theme = ""
	case "motion":
		c.UI.Motion = nil
	case "cascade":
		c.UI.Cascade = nil
	default:
		if _, ok := LookupSetting(key); ok {
			values := maps.Clone(c.Settings)
			delete(values, key)
			c.Settings = values
			return
		}
		extra := maps.Clone(c.Extra)
		delete(extra, strings.TrimPrefix(key, "extra."))
		c.Extra = extra
	}
}
