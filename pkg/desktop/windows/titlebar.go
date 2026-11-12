package windows

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"sync"
)

const (
	OverlayFallbackWidth   = 144
	MacOSTahoeDarwinMajor  = 25
	TitlebarHeight         = 34
	macTrafficLightsHeight = 14
	titleBarOverlayColor   = "rgba(1, 0, 0, 0)"
)

// WindowButtonPosition is where macOS draws the traffic lights inside the hidden titlebar.
var WindowButtonPosition = struct{ X, Y int }{12, TitlebarHeight/2 - macTrafficLightsHeight/2}

// NativeOverlayWidth is the width the native window controls take, zero on macOS
func NativeOverlayWidth(isMac bool) int {
	if isMac {
		return 0
	}
	return OverlayFallbackWidth
}

// TitleBarOverlay is the native titlebar overlay configuration, Disabled under WSLg.
type TitleBarOverlay struct {
	Disabled    bool    `json:"-"`
	Color       string  `json:"color,omitempty"`
	Height      float64 `json:"height"`
	SymbolColor string  `json:"symbolColor,omitempty"`
}

// TitleBarOverlayInput carries what the overlay options depend on.
type TitleBarOverlayInput struct {
	Platform       string
	DarwinMajor    int
	TitlebarHeight float64
	Color          string
	Foreground     string
	Dark           bool
	ZoomFactor     float64
}

// TitleBarOverlayOptions returns the overlay for the platform, none under WSLg, height only on macOS
func TitleBarOverlayOptions(in TitleBarOverlayInput) TitleBarOverlay {
	if in.Platform == "" {
		in.Platform = "linux"
	}
	switch in.Platform {
	case "wslg":
		return TitleBarOverlay{Disabled: true}
	case "mac":
		return TitleBarOverlay{Height: MacTitleBarOverlayHeight(in.DarwinMajor, in.TitlebarHeight)}
	}
	symbol := in.Foreground
	if symbol == "" {
		symbol = "#242424"
		if in.Dark {
			symbol = "#f7f7f7"
		}
	}
	return TitleBarOverlay{Color: in.Color, Height: ScaledOverlayHeight(in.TitlebarHeight, in.ZoomFactor), SymbolColor: symbol}
}

// ScaledOverlayHeight scales a positive titlebar height by a positive zoom factor, at least one pixel
func ScaledOverlayHeight(titlebarHeight float64, zoomFactor float64) float64 {
	if math.IsNaN(titlebarHeight) || math.IsInf(titlebarHeight, 0) || titlebarHeight <= 0 {
		return titlebarHeight
	}
	factor := zoomFactor
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor <= 0 {
		factor = 1
	}
	return math.Max(1, math.Floor(titlebarHeight*factor+0.5))
}

// MacTitleBarOverlayHeight is zero from macOS Tahoe on and the titlebar height before
func MacTitleBarOverlayHeight(darwinMajor int, titlebarHeight float64) float64 {
	if darwinMajor >= MacOSTahoeDarwinMajor {
		return 0
	}
	return titlebarHeight
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// IsHexColor accepts only six digit hex colors with a leading hash
func IsHexColor(v any) bool {
	s, ok := v.(string)
	return ok && hexColor.MatchString(s)
}

// TitleBarTheme is the renderer palette the titlebar follows.
type TitleBarTheme struct {
	Background string `json:"background"`
	Foreground string `json:"foreground"`
}

// ThemeSources are the accepted native theme modes.
var ThemeSources = map[string]bool{"dark": true, "light": true, "system": true}

// Chrome holds the renderer titlebar theme and the persisted native theme source.
type Chrome struct {
	mu          sync.Mutex
	theme       *TitleBarTheme
	themeSource string
	themePath   string
	log         func(string)
}

// NewChrome loads the native theme source from native-theme.json at themePath
func NewChrome(themePath string, log func(string)) *Chrome {
	c := &Chrome{themePath: themePath, log: log}
	c.themeSource = c.readThemeSource()
	return c
}

func (c *Chrome) readThemeSource() string {
	raw, err := os.ReadFile(c.themePath)
	if err == nil {
		var v map[string]any
		if json.Unmarshal(raw, &v) == nil {
			if s, ok := v["themeSource"].(string); ok && ThemeSources[s] {
				return s
			}
		}
	}
	return "system"
}

// ThemeSource returns the native theme mode in force
func (c *Chrome) ThemeSource() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.themeSource
}

// SetNativeTheme switches and persists a valid mode, reporting whether it changed
func (c *Chrome) SetNativeTheme(mode any) bool {
	s, ok := mode.(string)
	if !ok || !ThemeSources[s] {
		return false
	}
	c.mu.Lock()
	if c.themeSource == s {
		c.mu.Unlock()
		return false
	}
	c.themeSource = s
	c.mu.Unlock()
	body, _ := json.MarshalIndent(map[string]string{"themeSource": s}, "", "  ")
	if err := os.WriteFile(c.themePath, body, 0o644); err != nil {
		c.log("[theme] write native theme failed: " + err.Error())
	}
	return true
}

// SetTitleBarTheme accepts a payload with hex background and foreground, reporting whether it applied
func (c *Chrome) SetTitleBarTheme(payload any) bool {
	rec, ok := payload.(map[string]any)
	if !ok || !IsHexColor(rec["background"]) || !IsHexColor(rec["foreground"]) {
		return false
	}
	c.mu.Lock()
	c.theme = &TitleBarTheme{Background: rec["background"].(string), Foreground: rec["foreground"].(string)}
	c.mu.Unlock()
	return true
}

// WindowBackgroundColor is the renderer background when known, else the dark or light default
func (c *Chrome) WindowBackgroundColor(dark bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.theme != nil && IsHexColor(c.theme.Background) {
		return c.theme.Background
	}
	if dark {
		return "#111111"
	}
	return "#f7f7f7"
}

// Overlay returns the titlebar overlay for a window at zoomFactor on platform
func (c *Chrome) Overlay(platform string, darwinMajor int, dark bool, zoomFactor float64) TitleBarOverlay {
	c.mu.Lock()
	fg := ""
	if c.theme != nil && IsHexColor(c.theme.Foreground) {
		fg = c.theme.Foreground
	}
	c.mu.Unlock()
	return TitleBarOverlayOptions(TitleBarOverlayInput{
		Platform: platform, DarwinMajor: darwinMajor, TitlebarHeight: TitlebarHeight,
		Color: titleBarOverlayColor, Foreground: fg, Dark: dark, ZoomFactor: zoomFactor,
	})
}
