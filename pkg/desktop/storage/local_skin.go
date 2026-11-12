package storage

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

var (
	skinProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	skinFileName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

const (
	skinMaxConfigBytes = 1_000_000
	skinMaxSkinBytes   = 256_000
)

// Default colors block of the skin engine, the base a partial custom skin overlays
var defaultSkinColors = [][2]string{
	{"banner_border", "#CD7F32"}, {"banner_title", "#FFD700"}, {"banner_accent", "#FFBF00"}, {"banner_dim", "#B8860B"},
	{"banner_text", "#FFF8DC"}, {"ui_accent", "#FFBF00"}, {"ui_label", "#DAA520"}, {"ui_ok", "#4caf50"},
	{"ui_error", "#ef5350"}, {"ui_warn", "#ffa726"}, {"prompt", "#FFF8DC"}, {"input_rule", "#CD7F32"},
	{"response_border", "#FFD700"}, {"status_bar_bg", "#1a1a2e"}, {"status_bar_text", "#C0C0C0"},
	{"status_bar_strong", "#FFD700"}, {"status_bar_dim", "#8A7A4A"}, {"status_bar_good", "#8FBC8F"},
	{"status_bar_warn", "#FFD700"}, {"status_bar_bad", "#FF8C00"}, {"status_bar_critical", "#FF6B6B"},
	{"session_label", "#DAA520"}, {"session_border", "#8B8682"}, {"completion_menu_bg", "#1a1a2e"},
	{"completion_menu_current_bg", "#333355"}, {"selection_bg", "#3a3a55"}, {"shell_dollar", "#4dabf7"},
	{"voice_status_bg", "#1a1a2e"},
}

// Text of a regular file no larger than maxBytes, false for anything else or any failure
func skinReadText(file string, maxBytes int64) (string, bool) {
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxBytes {
		return "", false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	text, _ := pycodecs.DecodeReplace(raw, "utf-8")
	return text, true
}

// Top level mapping of a YAML source, nil when empty, malformed or not a mapping
func skinParseRecord(source string, ok bool) *Object {
	if !ok || source == "" {
		return nil
	}
	parsed, err := parseYAMLJS(source, 20)
	if err != nil {
		return nil
	}
	obj, _ := parsed.(*Object)
	return obj
}

// Trimmed string of at most 128 characters, false for anything else
func skinText(value any) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	t := jsTrim(s)
	return t, t != "" && jsLength(t) <= 128
}

// String entries of a mapping under the key and value size caps, nil when value is not a mapping
func skinStringMap(value any) *Object {
	m, ok := value.(*Object)
	if !ok {
		return nil
	}
	out := NewObject()
	for _, k := range m.Keys() {
		if s, ok := m.vals[k].(string); ok && jsLength(k) <= 128 && jsLength(s) <= 1024 {
			out.Set(k, s)
		}
	}
	return out
}

// Profile key as icebath --profile names it, default for anything unsafe for a path
func LocalSkinProfileKey(profile string) string {
	name := jsTrim(profile)
	if name != "" && name != "default" && skinProfileName.MatchString(name) {
		return name
	}
	return "default"
}

// Home of a profile under the root home, the root itself for default
func LocalSkinHome(hermesHome, profile string) string {
	name := LocalSkinProfileKey(profile)
	if name == "default" {
		return hermesHome
	}
	return filepath.Join(hermesHome, "profiles", name)
}

func skinNameOnly(name string) *Object {
	o := NewObject()
	o.Set("name", name)
	return o
}

// Skin payload of a custom skin file overlaid on the default colors, nil when the file is unusable
func skinFromFile(file, configuredName string) *Object {
	parsed := skinParseRecord(skinReadText(file, skinMaxSkinBytes))
	if parsed == nil {
		return nil
	}
	name, ok := skinText(parsed.vals["name"])
	if !ok {
		name = configuredName
	}
	skin := NewObject()
	skin.Set("name", name)
	if description, ok := skinText(parsed.vals["description"]); ok {
		skin.Set("description", description)
	}
	colors := NewObject()
	for _, kv := range defaultSkinColors {
		colors.Set(kv[0], kv[1])
	}
	if custom := skinStringMap(parsed.vals["colors"]); custom != nil {
		for _, k := range custom.Keys() {
			colors.Set(k, custom.vals[k])
		}
	}
	skin.Set("colors", colors)
	if dark := skinStringMap(parsed.vals["dark_colors"]); dark != nil {
		skin.Set("dark_colors", dark)
	}
	if light := skinStringMap(parsed.vals["light_colors"]); light != nil {
		skin.Set("light_colors", light)
	}
	return skin
}

// Configured display skin of a profile, a name only skin for built ins and nil for no or unsafe configuration
func ReadLocalDisplaySkin(hermesHome, profile string) *Object {
	home := LocalSkinHome(hermesHome, profile)
	config := skinParseRecord(skinReadText(filepath.Join(home, "config.yaml"), skinMaxConfigBytes))
	var display *Object
	if config != nil {
		display, _ = config.vals["display"].(*Object)
	}
	var skinValue any
	if display != nil {
		skinValue = display.vals["skin"]
	}
	configuredName, ok := skinText(skinValue)
	if !ok || !skinFileName.MatchString(configuredName) {
		return nil
	}
	skinsRoot := filepath.Join(home, "skins")
	file := filepath.Join(skinsRoot, configuredName+".yaml")
	realRoot, err := filepath.EvalSymlinks(skinsRoot)
	if err != nil {
		return skinNameOnly(configuredName)
	}
	realFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		return skinNameOnly(configuredName)
	}
	realRoot, _ = filepath.Abs(realRoot)
	realFile, _ = filepath.Abs(realFile)
	if filepath.Dir(realFile) != realRoot {
		return skinNameOnly(configuredName)
	}
	if skin := skinFromFile(file, configuredName); skin != nil {
		return skin
	}
	return skinNameOnly(configuredName)
}

// Pre paint skin payload of a window, its routed profile ahead of the primary profile, nil without a skin
func ReadLocalSkinPayload(hermesHome string, routedProfile *string, fallbackProfile string) *Object {
	chosen := fallbackProfile
	if routedProfile != nil {
		chosen = *routedProfile
	}
	profile := LocalSkinProfileKey(chosen)
	skin := ReadLocalDisplaySkin(hermesHome, profile)
	if skin == nil {
		return nil
	}
	payload := NewObject()
	payload.Set("profile", profile)
	payload.Set("skin", skin)
	return payload
}
