package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var builtinDashboardThemes = []*pyjson.Object{
	pyjson.ObjectOf("name", "default", "label", "Hermes Teal", "description", "Classic dark teal — the canonical Hermes look"),
	pyjson.ObjectOf("name", "default-large", "label", "Hermes Teal (Large)", "description", "Hermes Teal with bigger fonts and roomier spacing"),
	pyjson.ObjectOf("name", "nous-blue", "label", "Nous Blue", "description", "Light mode — vivid Nous-blue accents on cream canvas"),
	pyjson.ObjectOf("name", "midnight", "label", "Midnight", "description", "Deep blue-violet with cool accents"),
	pyjson.ObjectOf("name", "ember", "label", "Ember", "description", "Warm crimson and bronze — forge vibes"),
	pyjson.ObjectOf("name", "mono", "label", "Mono", "description", "Clean grayscale — minimal and focused"),
	pyjson.ObjectOf("name", "cyberpunk", "label", "Cyberpunk", "description", "Neon green on black — matrix terminal"),
	pyjson.ObjectOf("name", "rose", "label", "Rosé", "description", "Soft pink and warm ivory — easy on the eyes"),
}

// Default typography of a theme in declaration order
func themeDefaultTypography() *pyjson.Object {
	return pyjson.ObjectOf(
		"fontSans", `system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif`,
		"fontMono", `ui-monospace, "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace`,
		"baseSize", "15px", "lineHeight", "1.55", "letterSpacing", "0")
}

var (
	themeOverrideKeys = map[string]bool{"card": true, "cardForeground": true, "popover": true, "popoverForeground": true,
		"primary": true, "primaryForeground": true, "secondary": true, "secondaryForeground": true, "muted": true,
		"mutedForeground": true, "accent": true, "accentForeground": true, "destructive": true,
		"destructiveForeground": true, "success": true, "warning": true, "border": true, "input": true, "ring": true}
	themeNamedAssetKeys   = []string{"bg", "hero", "logo", "crest", "sidebar", "header"}
	themeComponentBuckets = map[string]bool{"card": true, "header": true, "footer": true, "sidebar": true, "tab": true,
		"progress": true, "badge": true, "backdrop": true, "page": true}
	themeLayoutVariants = map[string]bool{"standard": true, "cockpit": true, "tiled": true}
)

const themeCustomCSSMax = 32 * 1024

// Mapping value of a YAML dict with its keys in document order
type yamlDict struct {
	keys []string
	vals map[string]any
}

// Dict view of a YAML value, false for anything that is not a mapping
func asYAMLDict(v any) (yamlDict, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return yamlDict{vals: map[string]any{}}, false
	}
	keys := config.MappingKeys(m)
	if len(keys) != len(m) {
		keys = keys[:0]
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	return yamlDict{keys: keys, vals: m}, true
}

// Dict field of a mapping, empty when absent or not a dict
func themeDictField(data yamlDict, key string) yamlDict {
	d, _ := asYAMLDict(data.vals[key])
	return d
}

// True for a string with non whitespace content
func nonemptyStr(v any) bool {
	s, ok := v.(string)
	return ok && pystr.Strip(s) != ""
}

// True for a key made of alphanumerics, dashes and underscores
func cssIdent(key string) bool {
	s := strings.NewReplacer("-", "", "_", "").Replace(key)
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsAlnum(r) {
			return false
		}
	}
	return true
}

// Layer spec from bare hex or a hex and alpha dict, nil on garbage
func parseThemeLayer(value any, defaultHex string, defaultAlpha float64) *pyjson.Object {
	switch v := value.(type) {
	case nil:
		return pyjson.ObjectOf("hex", defaultHex, "alpha", defaultAlpha)
	case string:
		return pyjson.ObjectOf("hex", v, "alpha", defaultAlpha)
	case map[string]any:
		hexVal, ok := defaultHex, true
		if raw, has := v["hex"]; has {
			hexVal, ok = raw.(string)
		}
		if !ok {
			return nil
		}
		alpha := defaultAlpha
		if raw, has := v["alpha"]; has {
			if f, err := pyint.Float(raw); err == nil {
				alpha = f
			}
		}
		return pyjson.ObjectOf("hex", hexVal, "alpha", max(0.0, min(1.0, alpha)))
	}
	return nil
}

// First present value among the keys of the dicts, nil when none holds it
func dictGetEither(key string, dicts ...yamlDict) any {
	for _, d := range dicts {
		if v, ok := d.vals[key]; ok {
			return v
		}
	}
	return nil
}

// Python str of a YAML scalar value
func yamlScalarStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyStrOf(v)
}

// Theme YAML normalised to the wire format the theme provider expects, nil when unusable
func normaliseThemeDefinition(raw any) *pyjson.Object {
	data, ok := asYAMLDict(raw)
	if !ok || !nonemptyStr(data.vals["name"]) {
		return nil
	}
	name := data.vals["name"]
	paletteSrc, colorsSrc := themeDictField(data, "palette"), themeDictField(data, "colors")
	layer := func(key, defaultHex string, defaultAlpha float64) *pyjson.Object {
		v, has := paletteSrc.vals[key]
		if !has {
			v = colorsSrc.vals[key]
		}
		if parsed := parseThemeLayer(v, defaultHex, defaultAlpha); parsed != nil {
			return parsed
		}
		return pyjson.ObjectOf("hex", defaultHex, "alpha", defaultAlpha)
	}
	rawNoise, has := paletteSrc.vals["noiseOpacity"]
	if !has {
		rawNoise = data.vals["noiseOpacity"]
	}
	noise := 1.0
	if rawNoise != nil {
		if f, err := pyint.Float(rawNoise); err == nil {
			noise = f
		}
	}
	warmGlow := any("rgba(255, 189, 56, 0.35)")
	if pyTruthyYAML(paletteSrc.vals["warmGlow"]) {
		warmGlow = paletteSrc.vals["warmGlow"]
	} else if pyTruthyYAML(data.vals["warmGlow"]) {
		warmGlow = data.vals["warmGlow"]
	}
	palette := pyjson.ObjectOf("background", layer("background", "#041c1c", 1.0), "midground", layer("midground", "#ffe6cb", 1.0),
		"foreground", layer("foreground", "#ffffff", 0.0), "warmGlow", warmGlow, "noiseOpacity", noise)
	typoSrc := themeDictField(data, "typography")
	typography := themeDefaultTypography()
	for _, key := range []string{"fontSans", "fontMono", "fontDisplay", "fontUrl", "baseSize", "lineHeight", "letterSpacing"} {
		if nonemptyStr(typoSrc.vals[key]) {
			typography.Set(key, typoSrc.vals[key])
		}
	}
	layoutSrc := themeDictField(data, "layout")
	layout := pyjson.ObjectOf("radius", "0.5rem", "density", "comfortable")
	if nonemptyStr(layoutSrc.vals["radius"]) {
		layout.Set("radius", layoutSrc.vals["radius"])
	}
	if d, ok := layoutSrc.vals["density"].(string); ok && (d == "compact" || d == "comfortable" || d == "spacious") {
		layout.Set("density", d)
	}
	colorOverrides := pyjson.NewObject()
	overrides := themeDictField(data, "colorOverrides")
	for _, k := range overrides.keys {
		if themeOverrideKeys[k] && nonemptyStr(overrides.vals[k]) {
			colorOverrides.Set(k, overrides.vals[k])
		}
	}
	assetsSrc := themeDictField(data, "assets")
	assetsOut := pyjson.NewObject()
	for _, k := range themeNamedAssetKeys {
		if nonemptyStr(assetsSrc.vals[k]) {
			assetsOut.Set(k, assetsSrc.vals[k])
		}
	}
	customAssets := pyjson.NewObject()
	customSrc := themeDictField(assetsSrc, "custom")
	for _, k := range customSrc.keys {
		if cssIdent(k) && nonemptyStr(customSrc.vals[k]) {
			customAssets.Set(k, customSrc.vals[k])
		}
	}
	if customAssets.Len() > 0 {
		assetsOut.Set("custom", customAssets)
	}
	var customCSS any
	if nonemptyStr(data.vals["customCSS"]) {
		r := []rune(data.vals["customCSS"].(string))
		if len(r) > themeCustomCSSMax {
			r = r[:themeCustomCSSMax]
		}
		customCSS = string(r)
	}
	componentStyles := pyjson.NewObject()
	buckets := themeDictField(data, "componentStyles")
	for _, bucket := range buckets.keys {
		props, ok := asYAMLDict(buckets.vals[bucket])
		if !themeComponentBuckets[bucket] || !ok {
			continue
		}
		clean := pyjson.NewObject()
		for _, prop := range props.keys {
			value := props.vals[prop]
			switch value.(type) {
			case string, int, int64, float64, bool:
			default:
				continue
			}
			if s := yamlScalarStr(value); cssIdent(prop) && pystr.Strip(s) != "" {
				clean.Set(prop, s)
			}
		}
		if clean.Len() > 0 {
			componentStyles.Set(bucket, clean)
		}
	}
	layoutVariant, ok := data.vals["layoutVariant"].(string)
	if !ok || !themeLayoutVariants[layoutVariant] {
		layoutVariant = "standard"
	}
	label := data.vals["label"]
	if !pyTruthyYAML(label) {
		label = name
	}
	description, has := data.vals["description"]
	if !has {
		description = ""
	}
	result := pyjson.ObjectOf("name", name, "label", label, "description", description, "palette", palette,
		"typography", typography, "layout", layout, "layoutVariant", layoutVariant)
	if colorOverrides.Len() > 0 {
		result.Set("colorOverrides", colorOverrides)
	}
	if assetsOut.Len() > 0 {
		result.Set("assets", assetsOut)
	}
	if customCSS != nil {
		result.Set("customCSS", customCSS)
	}
	if componentStyles.Len() > 0 {
		result.Set("componentStyles", componentStyles)
	}
	return yamlToJSON(result).(*pyjson.Object)
}

// Python truthiness of a YAML value
func pyTruthyYAML(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		return len(x) > 0
	case int:
		return x != 0
	}
	return pyTruthy(v)
}

// JSON shaped copy of a value holding YAML mappings, mappings as ordered objects
func yamlToJSON(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys {
			out.Set(k, yamlToJSON(x.Vals[k]))
		}
		return out
	case map[string]any:
		d, _ := asYAMLDict(x)
		out := pyjson.NewObject()
		for _, k := range d.keys {
			out.Set(k, yamlToJSON(x[k]))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = yamlToJSON(e)
		}
		return out
	}
	return v
}

// Normalised user themes from the launch home dashboard-themes folder in name order
func discoverUserThemes() []*pyjson.Object {
	themesDir := filepath.Join(home.Process(), "dashboard-themes")
	if st, err := os.Stat(themesDir); err != nil || !st.IsDir() {
		return []*pyjson.Object{}
	}
	files, _ := filepath.Glob(filepath.Join(themesDir, "*.yaml"))
	sort.Strings(files)
	result := []*pyjson.Object{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		data, err := config.SafeLoad(raw)
		if err != nil {
			continue
		}
		if normalised := normaliseThemeDefinition(data); normalised != nil {
			result = append(result, normalised)
		}
	}
	return result
}

// Hex of a palette layer, the default when the layer is absent or not a dict
func layerHex(palette *pyjson.Object, key, def string) any {
	if layer, ok := palette.GetOr(key, nil).(*pyjson.Object); ok {
		return layer.GetOr("hex", def)
	}
	return def
}

// Critical CSS style block of the active user theme, empty for a built in or unknown theme
func renderActiveThemeBootstrapCSS(ctx context.Context) string {
	cfg, err := delegationLoadConfig(ctx)
	if err != nil {
		webServerLog().Debug("theme bootstrap render failed", "error", err)
		return ""
	}
	activeRaw, ok := config.CfgGet(cfg, "dashboard", "theme")
	if !ok {
		activeRaw = "default"
	}
	active, ok := activeRaw.(string)
	if !ok || active == "" {
		return ""
	}
	for _, b := range builtinDashboardThemes {
		if b.Vals["name"] == active {
			return ""
		}
	}
	for _, theme := range discoverUserThemes() {
		if theme.Vals["name"] != active {
			continue
		}
		palette, _ := theme.GetOr("palette", nil).(*pyjson.Object)
		if palette == nil {
			palette = pyjson.NewObject()
		}
		typo, _ := theme.GetOr("typography", nil).(*pyjson.Object)
		if typo == nil {
			typo = pyjson.NewObject()
		}
		defaults := themeDefaultTypography()
		fontSans := typo.GetOr("fontSans", nil)
		if !pyTruthy(fontSans) {
			fontSans = defaults.Vals["fontSans"]
		}
		baseSize := typo.GetOr("baseSize", nil)
		if !pyTruthy(baseSize) {
			baseSize = defaults.Vals["baseSize"]
		}
		esc := func(v any) string { return strings.ReplaceAll(yamlScalarStr(v), "</", `<\/`) }
		return `<style id="hermes-theme-bootstrap">` +
			":root{" +
			"--background-base:" + esc(layerHex(palette, "background", "#0a0a0a")) + ";" +
			"--midground-base:" + esc(layerHex(palette, "midground", "#e5e5e5")) + ";" +
			"--theme-font-sans:" + esc(fontSans) + ";" +
			"--theme-base-size:" + esc(baseSize) + ";" +
			"}" +
			"html,body{background-color:var(--background-base);" +
			"color:var(--midground-base);" +
			"font-family:var(--theme-font-sans);" +
			"font-size:var(--theme-base-size);}" +
			"</style>"
	}
	return ""
}
