package tui

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	themeRedrawDelay      = 40 * time.Millisecond
	bootBackgroundRecheck = 250 * time.Millisecond
	macAppearanceDelay    = 1500 * time.Millisecond
)

var paintableHexRe = regexp.MustCompile(`(?i)^#[0-9a-f]{6}$`)

// Whether a terminal default color can be painted with this hex
func isPaintableHex(hex string) bool { return paintableHexRe.MatchString(hex) }

// Skin the gateway announced, its colors split by polarity, branding and banner art
type gatewaySkin struct {
	Colors      map[string]string
	LightColors map[string]string
	DarkColors  map[string]string
	Branding    map[string]string
	BannerLogo  string
	BannerHero  string
	ToolPrefix  string
	HelpHeader  string
}

// String entries of a payload object, null and other values left out
func stringMap(v any) map[string]string {
	raw, _ := v.(map[string]any)
	out := make(map[string]string, len(raw))
	for k, value := range raw {
		if s, isStr := value.(string); isStr {
			out[k] = s
		}
	}
	return out
}

// Skin of a gateway.ready or skin.changed payload
func gatewaySkinOf(p map[string]any) *gatewaySkin {
	if p == nil {
		return nil
	}
	return &gatewaySkin{
		Colors: stringMap(p["colors"]), LightColors: stringMap(p["light_colors"]), DarkColors: stringMap(p["dark_colors"]), Branding: stringMap(p["branding"]),
		BannerLogo: paramStr(p["banner_logo"]), BannerHero: paramStr(p["banner_hero"]), ToolPrefix: paramStr(p["tool_prefix"]), HelpHeader: paramStr(p["help_header"]),
	}
}

// Theme state shared by the skin events, theme commands and terminal color probes
type themeSync struct {
	lastSkin      *gatewaySkin
	lastCommitted *theme
	configPinned  bool
	syncStarted   bool
	resolved      bool
	reportedBg    *string
	reportedFg    *string
	terminalBg    string
	terminalFg    string
}

// Theme of a skin, its polarity colors laid over the base colors
func themeForSkin(s *gatewaySkin) theme {
	paired := s.DarkColors
	if skinIsLight(s.Colors, os.Getenv) {
		paired = s.LightColors
	}
	colors := s.Colors
	if len(paired) > 0 {
		colors = make(map[string]string, len(s.Colors)+len(paired))
		for k, v := range s.Colors {
			colors[k] = v
		}
		for k, v := range paired {
			colors[k] = v
		}
	}
	return fromSkin(colors, s.Branding, s.BannerLogo, s.BannerHero, s.ToolPrefix, s.HelpHeader, os.Getenv)
}

// Whether two themes paint the same, every color and the brand name, prompt and banners
func themesEqual(a, b theme) bool {
	return a.Color == b.Color && a.Brand.Name == b.Brand.Name && a.Brand.Prompt == b.Brand.Prompt && a.BannerLogo == b.BannerLogo && a.BannerHero == b.BannerHero
}

// Makes a theme current and saves it for the next boot, a full repaint shortly after when it changed
func (s *tuiStore) commitTheme(t theme) tea.Cmd {
	prev := s.ui.Theme
	if s.themes.lastCommitted != nil {
		prev = *s.themes.lastCommitted
	}
	changed := !themesEqual(prev, t)
	s.themes.lastCommitted = &t
	s.ui.Theme = t
	pin := ""
	if s.themes.configPinned {
		if p := os.Getenv("ICEBATH_TUI_THEME"); p == "light" || p == "dark" {
			pin = p
		}
	}
	writeBootTheme(t, os.Getenv("ICEBATH_TUI_BACKGROUND"), pin)
	if !changed {
		return nil
	}
	return tea.Tick(themeRedrawDelay, func(time.Time) tea.Msg { return tea.ClearScreen() })
}

// Paints the terminal default colors the skin authored, the theme text over an authored background
func (s *tuiStore) paintTerminalDefaults(t theme) {
	background := ""
	if s.themes.lastSkin != nil {
		background = s.themes.lastSkin.Colors["background"]
	}
	s.themes.terminalBg, s.themes.terminalFg = "", ""
	if isPaintableHex(background) {
		s.themes.terminalBg, s.themes.terminalFg = background, themeToneHex(t.Color.Text)
		if !isPaintableHex(s.themes.terminalFg) {
			s.themes.terminalFg = ""
		}
	}
}

// Applies a gateway skin to the theme and the terminal colors
func (s *tuiStore) applySkin(skin *gatewaySkin) tea.Cmd {
	s.themes.lastSkin = skin
	t := themeForSkin(skin)
	cmd := s.commitTheme(t)
	s.paintTerminalDefaults(t)
	return cmd
}

// Derives the theme again from the current detection signals
func (s *tuiStore) reapplyTheme() tea.Cmd {
	t := defaultThemeForCurrentBackground(os.Getenv)
	if s.themes.lastSkin != nil {
		t = themeForSkin(s.themes.lastSkin)
	}
	cmd := s.commitTheme(t)
	s.paintTerminalDefaults(t)
	return cmd
}

// Applies the display.tui_theme pin, light or dark pinning the mode and anything else clearing a pin it set
func (s *tuiStore) applyConfiguredTuiTheme(raw any) tea.Cmd {
	mode := ""
	if raw != nil {
		mode = strings.ToLower(strings.TrimFunc(jsValueString(raw), isJSSpace))
	}
	current := os.Getenv("ICEBATH_TUI_THEME")
	if mode == "light" || mode == "dark" {
		s.themes.configPinned = true
		if current == mode {
			return nil
		}
		_ = os.Setenv("ICEBATH_TUI_THEME", mode)
	} else {
		if current == "" || !s.themes.configPinned {
			return nil
		}
		s.themes.configPinned = false
		_ = os.Unsetenv("ICEBATH_TUI_THEME")
	}
	return s.reapplyTheme()
}

// Representative background of the polarity a reported foreground implies, empty when ambiguous
func polarityBackgroundFromForeground(hex string) string {
	luminance, ok := relativeLuminance(hex)
	if !ok || hex == "#000000" || hex == "#ffffff" {
		return ""
	}
	if luminance >= 0.45 {
		return "#1e1e1e"
	}
	if luminance <= 0.2 {
		return "#ffffff"
	}
	return ""
}

// Terminal background re checked after a boot seeded hint was dropped
type bootBackgroundRecheckMsg struct{}

// macOS appearance probe that fires when the terminal answered no color query
type macAppearanceProbeMsg struct{}

// Queries the terminal's default colors once so the theme follows the real background
func (s *tuiStore) syncThemeToTerminalBackground() tea.Cmd {
	if s.themes.syncStarted {
		return nil
	}
	s.themes.syncStarted = true
	cmds := []tea.Cmd{tea.RequestBackgroundColor, tea.RequestForegroundColor, tea.Tick(macAppearanceDelay, func(time.Time) tea.Msg { return macAppearanceProbeMsg{} })}
	if s.themes.reportedBg != nil {
		cmds = append(cmds, s.backgroundListener(*s.themes.reportedBg))
	}
	if s.themes.reportedFg != nil {
		cmds = append(cmds, s.foregroundListener(*s.themes.reportedFg))
	}
	return tea.Batch(cmds...)
}

// Records the terminal's first background answer and hands it to the listener once syncing started
func (s *tuiStore) onTerminalBackground(hex string) tea.Cmd {
	if s.themes.reportedBg != nil {
		return nil
	}
	s.themes.reportedBg = &hex
	if !s.themes.syncStarted {
		return nil
	}
	return s.backgroundListener(hex)
}

// Records the terminal's first foreground answer and hands it to the listener once syncing started
func (s *tuiStore) onTerminalForeground(hex string) tea.Cmd {
	if s.themes.reportedFg != nil {
		return nil
	}
	s.themes.reportedFg = &hex
	if !s.themes.syncStarted {
		return nil
	}
	return s.foregroundListener(hex)
}

// Background answer, a pure black one from a transparent profile deferring to the foreground
func (s *tuiStore) backgroundListener(hex string) tea.Cmd {
	if hex == "#000000" {
		if invalidateBootBackground() {
			return tea.Tick(bootBackgroundRecheck, func(time.Time) tea.Msg { return bootBackgroundRecheckMsg{} })
		}
		return nil
	}
	s.themes.resolved = true
	_ = os.Setenv("ICEBATH_TUI_BACKGROUND", hex)
	return s.reapplyTheme()
}

// Re derives the theme when no answer resolved the background since the boot hint was dropped
func (s *tuiStore) onBootBackgroundRecheck() tea.Cmd {
	if s.themes.resolved {
		return nil
	}
	return s.reapplyTheme()
}

// Foreground answer, inferring the polarity when no background answer or pin decided it
func (s *tuiStore) foregroundListener(hex string) tea.Cmd {
	if s.themes.resolved || os.Getenv("ICEBATH_TUI_THEME") != "" || os.Getenv("ICEBATH_TUI_LIGHT") != "" {
		return nil
	}
	inferred := polarityBackgroundFromForeground(hex)
	if inferred == "" {
		return nil
	}
	s.themes.resolved = true
	_ = os.Setenv("ICEBATH_TUI_BACKGROUND", inferred)
	return s.reapplyTheme()
}

// Whether the macOS appearance setting still has to decide the background
func (s *tuiStore) macAppearanceUndecided() bool {
	return !s.themes.resolved && os.Getenv("ICEBATH_TUI_BACKGROUND") == "" && os.Getenv("ICEBATH_TUI_THEME") == ""
}

// Reads the macOS appearance when no probe answered and nothing pins the mode
func (s *tuiStore) onMacAppearanceProbe() tea.Cmd {
	if !s.macAppearanceUndecided() || runtime.GOOS != "darwin" || os.Getenv("ICEBATH_TUI_LIGHT") != "" || os.Getenv("COLORFGBG") != "" {
		return nil
	}
	return func() tea.Msg {
		out, err := exec.CommandContext(context.Background(), "defaults", "read", "-g", "AppleInterfaceStyle").Output()
		return asyncDoneMsg{run: func() tea.Cmd {
			if !s.macAppearanceUndecided() {
				return nil
			}
			s.themes.resolved = true
			background := "#ffffff"
			if err == nil && strings.TrimFunc(string(out), isJSSpace) == "Dark" {
				background = "#1e1e1e"
			}
			_ = os.Setenv("ICEBATH_TUI_BACKGROUND", background)
			return s.reapplyTheme()
		}}
	}
}
