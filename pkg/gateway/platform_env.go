package gateway

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One env override step a platform declares, run in order after the config loads
type EnvStep interface {
	Apply(ctx context.Context, cfg *GatewayConfig) error
}

// Plain function step
type EnvStepFunc func(ctx context.Context, cfg *GatewayConfig) error

func (f EnvStepFunc) Apply(ctx context.Context, cfg *GatewayConfig) error { return f(ctx, cfg) }

// Comma separated items stripped with blanks dropped
func CSVList(value string) []any {
	out := []any{}
	for _, part := range strings.Split(value, ",") {
		if p := pystr.Strip(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Parser of a base ten int falling back to def on malformed or blank input
func IntOr(def int) func(string) any {
	return func(raw string) any {
		if n, ok := pyint.Parse(pystr.Strip(raw)); ok {
			return pyint.Native(n)
		}
		return def
	}
}

// Value with trailing slashes removed
func StripSlash(value string) any { return strings.TrimRight(value, "/") }

// Value stripped and lower cased
func StripLower(value string) any { return pystr.Lower(pystr.Strip(value)) }

// Whether the lower cased value is a truthy token, surrounding blanks not stripped
func TruthyToken(value string) any {
	switch pystr.Lower(value) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Shared truthy coercion of an env string, blanks stripped
func TruthyValue(value string) any {
	v, _ := pystr.BoolToken(value)
	return v
}

// JSON value of the text, else its comma or newline separated items
func MentionPatterns(value string) any {
	if v, err := pyjson.Loads(value); err == nil {
		return v
	}
	return CSVList(strings.ReplaceAll(value, "\n", ","))
}

// Key and raw text pair of a csv extra
type CSVSpec struct{ Key, Raw string }

// Sets each key to the csv list of its raw text when the list is non empty
func CSVExtras(extra map[string]any, spec []CSVSpec) {
	for _, s := range spec {
		if items := CSVList(s.Raw); len(items) > 0 {
			extra[s.Key] = items
		}
	}
}

// First truthy value among env names, a single name returning its value as is
func EnvFirst(ctx context.Context, envs []string) string {
	if len(envs) == 1 {
		return getenvStr(ctx, envs[0], "")
	}
	for _, e := range envs {
		if v := getenvStr(ctx, e, ""); v != "" {
			return v
		}
	}
	return ""
}

// One env backed extra value, AnyOf naming alternatives read as the first truthy one
type EnvSpec struct {
	Key     string
	Env     string
	AnyOf   []string
	Default string
	Fn      func(string) any
	Int     bool
}

// Writes each spec whose env value is truthy, Int specs parsing an int and skipping a malformed one
func EnvExtras(ctx context.Context, extra map[string]any, spec []EnvSpec, strip bool) {
	for _, s := range spec {
		value := getenvStr(ctx, s.Env, "")
		if strip {
			value = pystr.Strip(value)
		}
		if value == "" {
			continue
		}
		switch {
		case s.Int:
			if n, ok := pyint.Parse(value); ok {
				extra[s.Key] = pyint.Native(n)
			}
		case s.Fn != nil:
			extra[s.Key] = s.Fn(value)
		default:
			extra[s.Key] = value
		}
	}
}

// Sets the home channel from the env base and its name and thread id when the platform is configured
func EnvHomeChannel(ctx context.Context, cfg *GatewayConfig, platform Platform, envBase string, strip bool) {
	chatID := getenvStr(ctx, envBase, "")
	if strip {
		chatID = pystr.Strip(chatID)
	}
	pc, ok := cfg.Platforms[platform]
	if chatID == "" || !ok {
		return
	}
	h := &HomeChannel{Platform: platform, ChatID: chatID, Name: getenvStr(ctx, envBase+"_NAME", "Home")}
	if t := getenvStr(ctx, envBase+"_THREAD_ID", ""); t != "" {
		h.ThreadID = &t
	}
	h.PostInit()
	pc.HomeChannel = h
}

// Sets the reply mode from env when it names off, first or all
func EnvReplyMode(ctx context.Context, cfg *GatewayConfig, platform Platform, env string) {
	mode := pystr.Lower(getenvStr(ctx, env, ""))
	if mode == "off" || mode == "first" || mode == "all" {
		platformSetDefault(cfg, platform).ReplyToMode = mode
	}
}

// Config of a platform, created with the defaults when absent
func platformSetDefault(cfg *GatewayConfig, platform Platform) *PlatformConfig {
	pc, ok := cfg.Platforms[platform]
	if !ok {
		pc = NewPlatformConfig()
		cfg.Platforms[platform] = pc
	}
	return pc
}

// Home channel step whose platform and env base stay readable to the profile channel scan
type HomeStep struct {
	Platform Platform
	EnvBase  string
	Strip    bool
}

func (s HomeStep) Apply(ctx context.Context, cfg *GatewayConfig) error {
	EnvHomeChannel(ctx, cfg, s.Platform, s.EnvBase, s.Strip)
	return nil
}

// Reply mode step whose platform and env stay readable to the profile channel scan
type ReplyModeStep struct {
	Platform Platform
	Env      string
}

func (s ReplyModeStep) Apply(ctx context.Context, cfg *GatewayConfig) error {
	EnvReplyMode(ctx, cfg, s.Platform, s.Env)
	return nil
}

// Enables a platform on env credentials unless config.yaml disabled it, warning once
func EnableFromEnv(ctx context.Context, cfg *GatewayConfig, platform Platform) *PlatformConfig {
	return enableFromEnv(ctx, cfg, platform, false, true)
}

// Credential gated platform enable, Creds groups all truthy with a group's names any of
type EnvCred struct {
	Platform         Platform
	Creds            [][]string
	Token            string
	Fixed            []EnvSpec
	Optional         []EnvSpec
	OptionalStripped []EnvSpec
	WarnMissingEnv   string
	WarnMissingMsg   string
	Then             func(ctx context.Context, cfg *GatewayConfig, pc *PlatformConfig) error
	Home             string
	HomeStrip        bool
}

func (c *EnvCred) Apply(ctx context.Context, cfg *GatewayConfig) error {
	for _, group := range c.Creds {
		if EnvFirst(ctx, group) == "" {
			return nil
		}
	}
	if c.WarnMissingEnv != "" && getenvStr(ctx, c.WarnMissingEnv, "") == "" {
		slog.Warn(c.WarnMissingMsg)
	}
	pc := EnableFromEnv(ctx, cfg, c.Platform)
	if c.Token != "" {
		if token := getenvStr(ctx, c.Token, ""); token != "" {
			pc.Token = token
		}
	}
	extra := pc.Extra
	for _, f := range c.Fixed {
		var value string
		if f.AnyOf != nil {
			if value = EnvFirst(ctx, f.AnyOf); value == "" {
				value = f.Default
			}
		} else {
			value = getenvStr(ctx, f.Env, f.Default)
		}
		if f.Fn != nil {
			extra[f.Key] = f.Fn(value)
		} else {
			extra[f.Key] = value
		}
	}
	EnvExtras(ctx, extra, c.Optional, false)
	EnvExtras(ctx, extra, c.OptionalStripped, true)
	if c.Then != nil {
		if err := c.Then(ctx, cfg, pc); err != nil {
			return err
		}
	}
	if c.Home != "" {
		EnvHomeChannel(ctx, cfg, c.Platform, c.Home, c.HomeStrip)
	}
	return nil
}

// Entries a bundled plugin registers under a name other than its directory, with the declaration prefix
var bundledEnvSiblings = map[string][2]string{"wecom_callback": {"wecom", "CALLBACK_"}}

// Env declarations a bundled platform's gateway_env module makes under one prefix
type BundledEnvRows struct {
	Overrides         []EnvStep
	EnableCredentials []string
	Authz             map[string]any
}

var bundledEnvModules = struct {
	sync.Mutex
	byDir map[string]map[string]BundledEnvRows
}{byDir: map[string]map[string]BundledEnvRows{}}

// Records the gateway_env declarations of a bundled platform directory, called from the adapter package init
func RegisterBundledEnv(directory, prefix string, rows BundledEnvRows) {
	bundledEnvModules.Lock()
	defer bundledEnvModules.Unlock()
	if bundledEnvModules.byDir[directory] == nil {
		bundledEnvModules.byDir[directory] = map[string]BundledEnvRows{}
	}
	bundledEnvModules.byDir[directory][prefix] = rows
}

// Declarations of a bundled platform directory, nil when it is no bundled plugin or declares none
func bundledSibling(directory string) map[string]BundledEnvRows {
	names, _ := bundledPlatformScanCached()
	if !names[directory] {
		return nil
	}
	bundledEnvModules.Lock()
	defer bundledEnvModules.Unlock()
	return bundledEnvModules.byDir[directory]
}

// Directory and prefix holding a bundled platform's declarations
func bundledEnvLocation(name string) (string, string) {
	if loc, ok := bundledEnvSiblings[name]; ok {
		return loc[0], loc[1]
	}
	return name, ""
}

// Env overrides and enable credentials a bundled platform declares, ok false when it has no module
func bundledEnvRows(name string) (overrides []EnvStep, credentials []string, ok bool) {
	directory, prefix := bundledEnvLocation(name)
	module := bundledSibling(directory)
	if module == nil {
		return nil, nil, false
	}
	rows := module[prefix]
	return append([]EnvStep{}, rows.Overrides...), append([]string{}, rows.EnableCredentials...), true
}

// Authorization env names a bundled platform declares, empty for other names
func BundledAuthz(name string) map[string]any {
	directory, prefix := bundledEnvLocation(name)
	out := map[string]any{}
	if module := bundledSibling(directory); module != nil {
		for k, v := range module[prefix].Authz {
			out[k] = v
		}
	}
	return out
}

// Authorization field of the entry, else the bundled plugin's declaration, never looser than the bundled adapter
func AuthzField(name any, field string, entry *PlatformEntry) any {
	var value any
	if entry != nil {
		value = entryKeyword(entry, field)
	}
	if pyrepr.Truthy(value) {
		return value
	}
	text := ""
	if pyrepr.Truthy(name) {
		text = pyrepr.Str(name)
	}
	return BundledAuthz(pystr.Lower(pystr.Strip(text)))[field]
}

// Value of the entry field a keyword names, nil for an unknown keyword
func entryKeyword(entry *PlatformEntry, keyword string) any {
	v := reflect.ValueOf(entry).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Tag.Get("py") == keyword {
			return v.Field(i).Interface()
		}
	}
	return nil
}
