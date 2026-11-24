package config

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

//go:embed builtin_skins.json
var builtinSkinsJSON string

// DefaultSkinName is the built in skin a config without display.skin gets, also reachable as "default"
const DefaultSkinName = "blackwall"

// Complete skin configuration, every mapping keeping its file or definition key order
type SkinConfig struct {
	Name string
	// Where the definition came from, builtin or user
	Source      string
	Description any
	Colors      map[string]any
	LightColors map[string]any
	DarkColors  map[string]any
	Spinner     map[string]any
	Branding    map[string]any
	ToolPrefix  any
	ToolEmojis  map[string]any
	BannerLogo  any
	BannerHero  any
	CustomCSS   string
}

// Filter every GetColor result passes through, the one shot chat light mode remap
var skinColorFilter atomic.Pointer[func(string) string]

// InstallSkinColorFilter routes every GetColor result through fn, nil removing the filter
func InstallSkinColorFilter(fn func(string) string) {
	if fn == nil {
		skinColorFilter.Store(nil)
		return
	}
	skinColorFilter.Store(&fn)
}

// Color under key as text, fallback when the skin lacks it, both through the installed filter
func (s *SkinConfig) GetColor(key, fallback string) string {
	value := fallback
	if v, ok := s.Colors[key]; ok {
		value = PyStr(v)
	}
	if f := skinColorFilter.Load(); f != nil {
		return (*f)(value)
	}
	return value
}

// Branding string under key, fallback when the skin lacks it
func (s *SkinConfig) GetBranding(key, fallback string) string {
	if v, ok := s.Branding[key]; ok {
		return PyStr(v)
	}
	return fallback
}

// Spinner wing pairs of two, any other entry skipped
func (s *SkinConfig) GetSpinnerWings() [][2]string {
	var out [][2]string
	wings, _ := s.Spinner["wings"].([]any)
	for _, pair := range wings {
		if p, ok := pair.([]any); ok && len(p) == 2 {
			out = append(out, [2]string{PyStr(p[0]), PyStr(p[1])})
		}
	}
	return out
}

// Ordered decode of a JSON object into maps whose key order MappingKeys reports
func orderedFromJSON(v any) any {
	switch x := v.(type) {
	case *jsonutil.Object:
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			m[k] = orderedFromJSON(x.Value(k))
		}
		SetMappingKeys(m, x.Keys())
		return m
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = orderedFromJSON(item)
		}
		return out
	}
	return v
}

var (
	builtinSkinsOnce  sync.Once
	builtinSkins      map[string]any
	builtinSkinOrder  []string
	skinStateMu       sync.Mutex
	activeSkin        *SkinConfig
	activeSkinName    = "default"
	activeSkinsByHome = map[string]skinEntry{}
)

type skinEntry struct {
	name string
	skin *SkinConfig
}

// Built in skin definitions by name and their definition order
func builtinSkinTable() (map[string]any, []string) {
	builtinSkinsOnce.Do(func() {
		doc, err := jsonutil.Decode(builtinSkinsJSON)
		if err != nil {
			panic(err)
		}
		builtinSkins = orderedFromJSON(doc).(map[string]any)
		builtinSkinOrder = MappingKeys(builtinSkins)
	})
	return builtinSkins, builtinSkinOrder
}

func builtinSkin(name string) (map[string]any, bool) {
	if name == "default" {
		name = DefaultSkinName
	}
	table, _ := builtinSkinTable()
	skin, ok := table[name].(map[string]any)
	return skin, ok
}

// Home key of the routed home override, empty when the process home is in effect
func routedHomeKey(ctx context.Context) string {
	if home.Override(ctx) == "" {
		return ""
	}
	return home.Key(home.Dir(ctx))
}

func skinsDir(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "skins") }

// Skin mapping of a YAML file that names itself, nil on any failure
func loadSkinFromYAML(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Debug("Failed to load skin", "path", path, "error", err)
		return nil
	}
	doc, err := FastSafeLoad([]byte(strings.TrimPrefix(string(data), "\uFEFF")))
	if err != nil {
		pkgLog().Debug("Failed to load skin", "path", path, "error", err)
		return nil
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	if _, named := m["name"]; !named {
		return nil
	}
	return m
}

// Ordered union of two mappings, b winning on shared keys
func mergeOrdered(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	keys := append([]string{}, MappingKeys(a)...)
	for _, k := range keys {
		out[k] = a[k]
	}
	for _, k := range MappingKeys(b) {
		if _, seen := out[k]; !seen {
			keys = append(keys, k)
		}
		out[k] = b[k]
	}
	SetMappingKeys(out, keys)
	return out
}

// Skin config from a built in or user mapping, the default skin filling missing colors, spinner and branding
func buildSkinConfig(data map[string]any, source string) *SkinConfig {
	def, _ := builtinSkin(DefaultSkinName)
	name := PyStr(getOr(data, "name", "unknown"))
	section := func(key string) map[string]any {
		value, present := data[key]
		if m, ok := value.(map[string]any); ok {
			return m
		}
		if present && value != nil {
			pkgLog().Warn("Skin has invalid section type; ignoring section", "skin", name, "section", key, "type", ErrorKind(value))
		}
		return map[string]any{}
	}
	merged := func(key string) map[string]any {
		base, _ := def[key].(map[string]any)
		return mergeOrdered(base, section(key))
	}
	css := []rune(strings.TrimSpace(PyStr(getOr(data, "customCSS", ""))))
	if len(css) > 32768 {
		css = css[:32768]
	}
	return &SkinConfig{
		Name: name, Source: source, Description: getOr(data, "description", ""), Colors: merged("colors"),
		LightColors: section("light_colors"), DarkColors: section("dark_colors"),
		Spinner: merged("spinner"), Branding: merged("branding"),
		ToolPrefix: getOr(data, "tool_prefix", getOr(def, "tool_prefix", "┊")),
		ToolEmojis: section("tool_emojis"), BannerLogo: getOr(data, "banner_logo", ""),
		BannerHero: getOr(data, "banner_hero", ""), CustomCSS: string(css),
	}
}

// One listed skin with its name, description and source
type SkinListing struct {
	Name        any
	Description any
	Source      string
}

// Built in skins then user skins whose names no earlier entry took
func ListSkins(ctx context.Context) []SkinListing {
	table, order := builtinSkinTable()
	var out []SkinListing
	for _, name := range order {
		out = append(out, SkinListing{Name: name, Description: getOr(table[name].(map[string]any), "description", ""), Source: "builtin"})
	}
	dir := skinsDir(ctx)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(files)
	for _, f := range files {
		data := loadSkinFromYAML(f)
		if len(data) == 0 {
			continue
		}
		name := getOr(data, "name", strings.TrimSuffix(filepath.Base(f), ".yaml"))
		taken := false
		for _, s := range out {
			if pyEq(s.Name, name) {
				taken = true
				break
			}
		}
		if !taken {
			out = append(out, SkinListing{Name: name, Description: getOr(data, "description", ""), Source: "user"})
		}
	}
	return out
}

// Skin by name, a user file first, then the built in, then the default
func LoadSkin(ctx context.Context, name string) *SkinConfig {
	var data map[string]any
	userFile := filepath.Join(skinsDir(ctx), name+".yaml")
	if fi, err := os.Stat(userFile); err == nil && fi.Mode().IsRegular() {
		data = loadSkinFromYAML(userFile)
	}
	builtin, isBuiltin := builtinSkin(name)
	if len(data) == 0 && !isBuiltin {
		pkgLog().Warn("Skin not found, using default", "skin", name)
	}
	switch {
	case len(data) > 0:
		return buildSkinConfig(data, "user")
	case isBuiltin:
		return buildSkinConfig(builtin, "builtin")
	}
	def, _ := builtinSkin(DefaultSkinName)
	return buildSkinConfig(def, "builtin")
}

// Active skin of the routed home or the process, initialized from the profile config on first use
func GetActiveSkin(ctx context.Context) *SkinConfig {
	if key := routedHomeKey(ctx); key != "" {
		skinStateMu.Lock()
		entry, ok := activeSkinsByHome[key]
		skinStateMu.Unlock()
		if !ok {
			cfg, err := LoadReadonly(ctx, home.Dir(ctx), processLookup)
			if err != nil {
				cfg = Tree{}
			}
			InitSkinFromConfig(ctx, cfg)
			skinStateMu.Lock()
			entry = activeSkinsByHome[key]
			skinStateMu.Unlock()
		}
		return entry.skin
	}
	skinStateMu.Lock()
	name, skin := activeSkinName, activeSkin
	skinStateMu.Unlock()
	if skin != nil {
		return skin
	}
	skin = LoadSkin(ctx, name)
	skinStateMu.Lock()
	if activeSkin == nil {
		activeSkin = skin
	}
	skin = activeSkin
	skinStateMu.Unlock()
	return skin
}

// Switches the active skin of the routed home or the process and returns it
func SetActiveSkin(ctx context.Context, name string) *SkinConfig {
	skin := LoadSkin(ctx, name)
	skinStateMu.Lock()
	defer skinStateMu.Unlock()
	if key := routedHomeKey(ctx); key != "" {
		activeSkinsByHome[key] = skinEntry{name: name, skin: skin}
		return skin
	}
	activeSkinName, activeSkin = name, skin
	return skin
}

// Activates display.skin of a config, default when it is unset, blank or not a string
func InitSkinFromConfig(ctx context.Context, cfg map[string]any) {
	name := "default"
	if display, ok := cfg["display"].(map[string]any); ok {
		if s, isStr := getOr(display, "skin", "default").(string); isStr && strings.TrimSpace(s) != "" {
			name = strings.TrimSpace(s)
		}
	}
	SetActiveSkin(ctx, name)
}

// Branding value of the active skin under key, the fallback when the skin lacks it
func activeBranding(ctx context.Context, key, fallback string) string {
	return GetActiveSkin(ctx).GetBranding(key, fallback)
}

// Name of the active skin, default for a routed home that never chose one
func GetActiveSkinName(ctx context.Context) string {
	skinStateMu.Lock()
	defer skinStateMu.Unlock()
	if key := routedHomeKey(ctx); key != "" {
		if entry, ok := activeSkinsByHome[key]; ok {
			return entry.name
		}
		return "default"
	}
	return activeSkinName
}

func GetActiveGoodbye(ctx context.Context, fallback string) string {
	return activeBranding(ctx, "goodbye", fallback)
}
