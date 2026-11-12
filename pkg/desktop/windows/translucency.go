package windows

import (
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	TranslucencyMin          = 0
	TranslucencyMax          = 100
	TranslucencyStep         = 1
	TranslucencyOpacityFloor = 0.3
	TranslucencyCurve        = 2
	WindowsGlassMinBuild     = 22621
	DefaultGlassMaterial     = "under-window"
	DefaultGlassScope        = "sidebar"
)

var (
	GlassMaterials             = []string{"under-window", "popover", "titlebar", "header"}
	GlassScopes                = []string{"window", "sidebar"}
	WindowsBackgroundMaterials = []string{"acrylic", "tabbed", "mica", "none"}
	windowsMaterialByFrost     = map[string]string{"under-window": "acrylic", "popover": "tabbed", "titlebar": "mica", "header": "mica"}
	windowsGlassMaterials      = func() []string {
		var out []string
		seen := map[string]bool{}
		for _, m := range GlassMaterials {
			if !seen[windowsMaterialByFrost[m]] {
				seen[windowsMaterialByFrost[m]] = true
				out = append(out, m)
			}
		}
		return out
	}()
)

// TranslucencyState is the resolved glass or clear setting of one appearance.
type TranslucencyState struct {
	Intensity int    `json:"intensity"`
	Fade      int    `json:"fade"`
	Mode      string `json:"mode"`
	Material  string `json:"material"`
	Scope     string `json:"scope"`
}

// TranslucencyValues is a partial set of per appearance values, nil fields fall through.
type TranslucencyValues struct {
	Intensity *int    `json:"intensity,omitempty"`
	Fade      *int    `json:"fade,omitempty"`
	Material  *string `json:"material,omitempty"`
	Scope     *string `json:"scope,omitempty"`
}

// TranslucencyBook stores the mode and the base, light and dark value layers.
type TranslucencyBook struct {
	Mode  string             `json:"mode"`
	Base  TranslucencyValues `json:"base"`
	Light TranslucencyValues `json:"light"`
	Dark  TranslucencyValues `json:"dark"`
}

type fullValues struct {
	intensity, fade int
	material, scope string
}

var defaultValues = map[string]map[string]fullValues{
	"mac": {
		"light": {29, 0, "header", DefaultGlassScope},
		"dark":  {29, 0, "titlebar", DefaultGlassScope},
	},
	"windows": {
		"light": {29, 0, "under-window", DefaultGlassScope},
		"dark":  {29, 0, "under-window", DefaultGlassScope},
	},
}

func defaultsFor(appearance string, isWindows bool) fullValues {
	key := "mac"
	if isWindows {
		key = "windows"
	}
	return defaultValues[key][appearance]
}

// DefaultTranslucencyValues returns the stock values of an appearance on macOS or Windows
func DefaultTranslucencyValues(appearance string, isWindows bool) TranslucencyState {
	d := defaultsFor(appearance, isWindows)
	return TranslucencyState{Intensity: d.intensity, Fade: d.fade, Material: d.material, Scope: d.scope}
}

// ClampIntensity rounds a numeric value into 0 to 100 and maps anything else to 0
func ClampIntensity(value any) int {
	n := jsNumberLoose(value)
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return TranslucencyMin
	}
	return int(math.Min(TranslucencyMax, math.Max(TranslucencyMin, jsRoundFloat(n))))
}

func jsRoundFloat(v float64) float64 { return math.Floor(v + 0.5) }

func jsNumberLoose(v any) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case float64:
		return t
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

// TranslucencySupportedOn is true where the window can be made translucent
func TranslucencySupportedOn(platform string) bool {
	return platform == "darwin" || platform == "win32"
}

// GlassSupportedOn is true on macOS and on Windows builds with backdrop materials
func GlassSupportedOn(platform, release string) bool {
	if platform == "darwin" {
		return true
	}
	if platform != "win32" {
		return false
	}
	parts := strings.Split(release, ".")
	if len(parts) < 3 {
		return false
	}
	build, ok := leadingInt(parts[2])
	return ok && build >= WindowsGlassMinBuild
}

func leadingInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n")
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// NormalizeMode keeps glass or clear, defaulting to clear for legacy intensity and glass otherwise when supported
func NormalizeMode(value any, glassSupported bool, legacyIntensity int) string {
	if !glassSupported {
		return "clear"
	}
	if s, ok := value.(string); ok && (s == "glass" || s == "clear") {
		return s
	}
	if legacyIntensity > 0 {
		return "clear"
	}
	return "glass"
}

// NormalizeMaterial keeps a known glass material or returns the default
func NormalizeMaterial(value any) string {
	if s, ok := value.(string); ok && slices.Contains(GlassMaterials, s) {
		return s
	}
	return DefaultGlassMaterial
}

// NormalizeScope keeps a known glass scope or returns the default
func NormalizeScope(value any) string {
	if s, ok := value.(string); ok && slices.Contains(GlassScopes, s) {
		return s
	}
	return DefaultGlassScope
}

func asRecord(payload any) map[string]any {
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// NormalizeState turns any payload into a complete translucency state
func NormalizeState(payload any, glassSupported bool) TranslucencyState {
	rec := asRecord(payload)
	intensity := ClampIntensity(rec["intensity"])
	return TranslucencyState{
		Intensity: intensity,
		Fade:      ClampIntensity(rec["fade"]),
		Mode:      NormalizeMode(rec["mode"], glassSupported, intensity),
		Material:  NormalizeMaterial(rec["material"]),
		Scope:     NormalizeScope(rec["scope"]),
	}
}

// DefaultTranslucencyState is the stock state of an appearance
func DefaultTranslucencyState(appearance string, glassSupported, isWindows bool) TranslucencyState {
	s := DefaultTranslucencyValues(appearance, isWindows)
	s.Mode = NormalizeMode(nil, glassSupported, 0)
	return s
}

func normalizeValues(payload any) TranslucencyValues {
	rec := asRecord(payload)
	var out TranslucencyValues
	if v, ok := rec["intensity"]; ok {
		n := ClampIntensity(v)
		out.Intensity = &n
	}
	if v, ok := rec["fade"]; ok {
		n := ClampIntensity(v)
		out.Fade = &n
	}
	if v, ok := rec["material"]; ok {
		s := NormalizeMaterial(v)
		out.Material = &s
	}
	if v, ok := rec["scope"]; ok {
		s := NormalizeScope(v)
		out.Scope = &s
	}
	return out
}

// NormalizeBook reads a stored book, migrating a flat legacy state into its base layer
func NormalizeBook(payload any, glassSupported bool) TranslucencyBook {
	rec := asRecord(payload)
	_, hasBase := rec["base"]
	_, hasLight := rec["light"]
	_, hasDark := rec["dark"]
	migrating := !hasBase && !hasLight && !hasDark
	if migrating {
		return TranslucencyBook{
			Mode: NormalizeMode(rec["mode"], glassSupported, ClampIntensity(rec["intensity"])),
			Base: normalizeValues(rec),
		}
	}
	return TranslucencyBook{
		Mode:  NormalizeMode(rec["mode"], glassSupported, 0),
		Base:  normalizeValues(rec["base"]),
		Light: normalizeValues(rec["light"]),
		Dark:  normalizeValues(rec["dark"]),
	}
}

func (b TranslucencyBook) slot(appearance string) TranslucencyValues {
	if appearance == "dark" {
		return b.Dark
	}
	return b.Light
}

// ResolveTranslucency layers the appearance slot over base over the stock values
func ResolveTranslucency(b TranslucencyBook, appearance string, isWindows bool) TranslucencyState {
	fb := defaultsFor(appearance, isWindows)
	s := b.slot(appearance)
	pickI := func(a, c *int, d int) int {
		if a != nil {
			return *a
		}
		if c != nil {
			return *c
		}
		return d
	}
	pickS := func(a, c *string, d string) string {
		if a != nil {
			return *a
		}
		if c != nil {
			return *c
		}
		return d
	}
	return TranslucencyState{
		Mode:      b.Mode,
		Intensity: pickI(s.Intensity, b.Base.Intensity, fb.intensity),
		Fade:      pickI(s.Fade, b.Base.Fade, fb.fade),
		Material:  pickS(s.Material, b.Base.Material, fb.material),
		Scope:     pickS(s.Scope, b.Base.Scope, fb.scope),
	}
}

// SetTranslucencyValues merges a normalized patch into the appearance slot of a copy of the book
func SetTranslucencyValues(b TranslucencyBook, appearance string, patch any) TranslucencyBook {
	cur := b.slot(appearance)
	p := normalizeValues(patch)
	if p.Intensity != nil {
		cur.Intensity = p.Intensity
	}
	if p.Fade != nil {
		cur.Fade = p.Fade
	}
	if p.Material != nil {
		cur.Material = p.Material
	}
	if p.Scope != nil {
		cur.Scope = p.Scope
	}
	if appearance == "dark" {
		b.Dark = cur
	} else {
		b.Light = cur
	}
	return b
}

func opacityRamp(lever int) float64 {
	ratio := float64(ClampIntensity(lever)) / TranslucencyMax
	floor := float64(TranslucencyOpacityFloor)
	return 1 - (1-floor)*math.Pow(ratio, TranslucencyCurve)
}

// WindowOpacityFor ramps opacity from intensity in clear mode and from fade while glass is active
func WindowOpacityFor(s TranslucencyState) float64 {
	if s.Mode != "glass" {
		return opacityRamp(s.Intensity)
	}
	if GlassActive(s) {
		return opacityRamp(s.Fade)
	}
	return opacityRamp(0)
}

// GlassActive is true in glass mode with a positive intensity
func GlassActive(s TranslucencyState) bool { return s.Mode == "glass" && s.Intensity > 0 }

// GlassSurfaceKeep is how much of the surface stays painted at an intensity
func GlassSurfaceKeep(intensity any) int { return TranslucencyMax - ClampIntensity(intensity) }

// VibrancyFor is the macOS vibrancy material, sidebar when glass is off
func VibrancyFor(s TranslucencyState) string {
	if GlassActive(s) {
		return s.Material
	}
	return "sidebar"
}

// BackgroundMaterialFor is the Windows backdrop material, none when glass is off
func BackgroundMaterialFor(s TranslucencyState) string {
	if GlassActive(s) {
		return windowsMaterialByFrost[s.Material]
	}
	return "none"
}

// GlassMaterialsFor lists the pickable materials, one per distinct backdrop on Windows
func GlassMaterialsFor(isWindows bool) []string {
	if isWindows {
		return slices.Clone(windowsGlassMaterials)
	}
	return slices.Clone(GlassMaterials)
}

// HudFrost is the HUD backdrop pairing.
type HudFrost struct {
	BackgroundMaterial string  `json:"backgroundMaterial"`
	Vibrancy           *string `json:"vibrancy"`
}

// HudFrostFor returns the HUD frost when the band shows and glass is active, none otherwise
func HudFrostFor(s TranslucencyState, showing bool) HudFrost {
	if showing && GlassActive(s) {
		m := s.Material
		return HudFrost{Vibrancy: &m, BackgroundMaterial: BackgroundMaterialFor(s)}
	}
	return HudFrost{BackgroundMaterial: "none"}
}

// GlassMaterialForPicker maps a material to the Windows rung sharing its backdrop
func GlassMaterialForPicker(material string, isWindows bool) string {
	if !isWindows {
		return material
	}
	for _, rung := range windowsGlassMaterials {
		if windowsMaterialByFrost[rung] == windowsMaterialByFrost[material] {
			return rung
		}
	}
	return DefaultGlassMaterial
}

// WindowBackingOptions returns the opaque background color unless glass is active
func WindowBackingOptions(s TranslucencyState, themedColor string) (string, bool) {
	if GlassActive(s) {
		return "", false
	}
	return themedColor, true
}

// WindowBackgroundMaterialOptions returns the backdrop material on Windows with glass support and active glass
func WindowBackgroundMaterialOptions(s TranslucencyState, isWindows, glassSupported bool) (string, bool) {
	if isWindows && glassSupported && GlassActive(s) {
		return BackgroundMaterialFor(s), true
	}
	return "", false
}

// OpacityNeedsSetting is true when either the next or the current opacity is below one
func OpacityNeedsSetting(next, current float64) bool { return next < 1 || current < 1 }

// NativeOpacityFor is the window opacity when translucency is supported and one otherwise
func NativeOpacityFor(s TranslucencyState, supported bool) float64 {
	if supported {
		return WindowOpacityFor(s)
	}
	return 1
}

// WindowOpacityOptions returns the opacity to set at creation, only when below one
func WindowOpacityOptions(s TranslucencyState, supported bool) (float64, bool) {
	o := NativeOpacityFor(s, supported)
	return o, OpacityNeedsSetting(o, 1)
}

// BackdropWindow is a Windows window whose backdrop material and background follow translucency.
type BackdropWindow interface {
	SetBackgroundMaterial(material string)
	SetBackgroundColor(color string)
}

// ApplyWindowsBackdrop updates the material and the backing color when either changed
func ApplyWindowsBackdrop(win BackdropWindow, s TranslucencyState, backingChanged, materialChanged bool, themedColor string) {
	if materialChanged {
		win.SetBackgroundMaterial(BackgroundMaterialFor(s))
	}
	if backingChanged || materialChanged {
		if GlassActive(s) {
			win.SetBackgroundColor("#00000000")
		} else {
			win.SetBackgroundColor(themedColor)
		}
	}
}
