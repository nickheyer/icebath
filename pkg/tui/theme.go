package tui

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Palette of the TUI, every slot a hex, rgb or ansi256 tone
type themeColors struct {
	Primary                 string `json:"primary"`
	Accent                  string `json:"accent"`
	Border                  string `json:"border"`
	Text                    string `json:"text"`
	Muted                   string `json:"muted"`
	CompletionBg            string `json:"completionBg"`
	CompletionCurrentBg     string `json:"completionCurrentBg"`
	CompletionMetaBg        string `json:"completionMetaBg"`
	CompletionMetaCurrentBg string `json:"completionMetaCurrentBg"`
	Label                   string `json:"label"`
	Ok                      string `json:"ok"`
	Error                   string `json:"error"`
	Warn                    string `json:"warn"`
	Tool                    string `json:"tool"`
	Thinking                string `json:"thinking"`
	SyntaxString            string `json:"syntaxString"`
	SyntaxNumber            string `json:"syntaxNumber"`
	SyntaxKeyword           string `json:"syntaxKeyword"`
	SyntaxComment           string `json:"syntaxComment"`
	Prompt                  string `json:"prompt"`
	SessionLabel            string `json:"sessionLabel"`
	SessionBorder           string `json:"sessionBorder"`
	StatusBg                string `json:"statusBg"`
	StatusFg                string `json:"statusFg"`
	StatusGood              string `json:"statusGood"`
	StatusWarn              string `json:"statusWarn"`
	StatusBad               string `json:"statusBad"`
	StatusCritical          string `json:"statusCritical"`
	SelectionBg             string `json:"selectionBg"`
	DiffAdded               string `json:"diffAdded"`
	DiffRemoved             string `json:"diffRemoved"`
	DiffAddedWord           string `json:"diffAddedWord"`
	DiffRemovedWord         string `json:"diffRemovedWord"`
	ShellDollar             string `json:"shellDollar"`
}

// Slot of a palette key
func (c *themeColors) slot(key string) *string {
	switch key {
	case "primary":
		return &c.Primary
	case "accent":
		return &c.Accent
	case "border":
		return &c.Border
	case "text":
		return &c.Text
	case "muted":
		return &c.Muted
	case "completionBg":
		return &c.CompletionBg
	case "completionCurrentBg":
		return &c.CompletionCurrentBg
	case "completionMetaBg":
		return &c.CompletionMetaBg
	case "completionMetaCurrentBg":
		return &c.CompletionMetaCurrentBg
	case "label":
		return &c.Label
	case "ok":
		return &c.Ok
	case "error":
		return &c.Error
	case "warn":
		return &c.Warn
	case "tool":
		return &c.Tool
	case "thinking":
		return &c.Thinking
	case "prompt":
		return &c.Prompt
	case "sessionLabel":
		return &c.SessionLabel
	case "sessionBorder":
		return &c.SessionBorder
	case "statusBg":
		return &c.StatusBg
	case "statusFg":
		return &c.StatusFg
	case "statusGood":
		return &c.StatusGood
	case "statusWarn":
		return &c.StatusWarn
	case "statusBad":
		return &c.StatusBad
	case "statusCritical":
		return &c.StatusCritical
	case "selectionBg":
		return &c.SelectionBg
	case "shellDollar":
		return &c.ShellDollar
	}
	return nil
}

// Agent identity the TUI shows
type themeBrand struct {
	Name       string `json:"name"`
	Icon       string `json:"icon"`
	Prompt     string `json:"prompt"`
	Welcome    string `json:"welcome"`
	Goodbye    string `json:"goodbye"`
	Tool       string `json:"tool"`
	HelpHeader string `json:"helpHeader"`
}

// Palette, brand and banner art of the TUI
type theme struct {
	Color      themeColors `json:"color"`
	Brand      themeBrand  `json:"brand"`
	BannerLogo string      `json:"bannerLogo"`
	BannerHero string      `json:"bannerHero"`
}

var (
	hex6Re    = regexp.MustCompile(`(?i)^#?([0-9a-f]{6})$`)
	hex3Re    = regexp.MustCompile(`(?i)^#?([0-9a-f]{3})$`)
	rgbFnRe   = regexp.MustCompile(`(?i)^rgba?\([` + jsWS + `]*(\d{1,3})[` + jsWS + `]*,[` + jsWS + `]*(\d{1,3})[` + jsWS + `]*,[` + jsWS + `]*(\d{1,3})`)
	ansi256Re = regexp.MustCompile(`^ansi256\((\d{1,3})\)$`)
	bgHex6Re  = regexp.MustCompile(`^[0-9a-f]{6}$`)
	bgHex3Re  = regexp.MustCompile(`^[0-9a-f]{3}$`)
	digitsRe  = regexp.MustCompile(`^\d+$`)
	trueRe    = regexp.MustCompile(`^(?:1|true|yes|on)$`)
	falseRe   = regexp.MustCompile(`^(?:0|false|no|off)$`)
)

// Channel rounded and clamped to a byte
func clampChannel(v float64) float64 { return math.Max(0, math.Min(255, jsRound(v))) }

// Red, green and blue of a hex, short hex or rgb function color, false for anything else
func parseColor(input string) ([3]float64, bool) {
	value := strings.TrimFunc(input, isJSSpace)
	if m := hex6Re.FindStringSubmatch(value); m != nil {
		n, _ := strconv.ParseInt(m[1], 16, 64)
		return [3]float64{float64(n >> 16 & 0xff), float64(n >> 8 & 0xff), float64(n & 0xff)}, true
	}
	if m := hex3Re.FindStringSubmatch(value); m != nil {
		var rgb [3]float64
		for i := range 3 {
			n, _ := strconv.ParseInt(strings.Repeat(m[1][i:i+1], 2), 16, 64)
			rgb[i] = float64(n)
		}
		return rgb, true
	}
	if m := rgbFnRe.FindStringSubmatch(value); m != nil {
		var rgb [3]float64
		for i := range 3 {
			n, _ := strconv.ParseFloat(m[i+1], 64)
			rgb[i] = clampChannel(n)
		}
		return rgb, true
	}
	return [3]float64{}, false
}

// Lowercase hex of channels clamped to bytes
func toHex(rgb [3]float64) string {
	return fmt.Sprintf("#%02x%02x%02x", int(clampChannel(rgb[0])), int(clampChannel(rgb[1])), int(clampChannel(rgb[2])))
}

// Blend of a toward b by t, a when either color does not parse
func mixColor(a, b string, t float64) string {
	pa, okA := parseColor(a)
	pb, okB := parseColor(b)
	if !okA || !okB {
		return a
	}
	return toHex([3]float64{pa[0] + (pb[0]-pa[0])*t, pa[1] + (pb[1]-pa[1])*t, pa[2] + (pb[2]-pa[2])*t})
}

func channelLuminance(value float64) float64 {
	n := value / 255
	if n <= 0.03928 {
		return n / 12.92
	}
	return math.Pow((n+0.055)/1.055, 2.4)
}

// WCAG relative luminance, false when the color does not parse
func relativeLuminance(color string) (float64, bool) {
	rgb, ok := parseColor(color)
	if !ok {
		return 0, false
	}
	return 0.2126*channelLuminance(rgb[0]) + 0.7152*channelLuminance(rgb[1]) + 0.0722*channelLuminance(rgb[2]), true
}

// WCAG contrast ratio of two colors, false when either does not parse
func contrastRatio(a, b string) (float64, bool) {
	la, okA := relativeLuminance(a)
	lb, okB := relativeLuminance(b)
	if !okA || !okB {
		return 0, false
	}
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05), true
}

// Contrast of a pair, 21 when either does not parse
func contrastOr21(a, b string) float64 {
	if r, ok := contrastRatio(a, b); ok {
		return r
	}
	return 21
}

// Color stepped away from the background in ten percent channel moves until it reaches the ratio
func liftForContrast(color, bg string, ratio float64) string {
	fg, ok := parseColor(color)
	bgLum, bgOK := relativeLuminance(bg)
	if !ok || !bgOK || contrastOr21(color, bg) >= ratio {
		return color
	}
	r, g, b := fg[0], fg[1], fg[2]
	cr := contrastOr21(toHex([3]float64{r, g, b}), bg)
	if bgLum > 0.5 {
		for cr < ratio && (r > 0 || g > 0 || b > 0) {
			r -= math.Ceil(r * 0.1)
			g -= math.Ceil(g * 0.1)
			b -= math.Ceil(b * 0.1)
			cr = contrastOr21(toHex([3]float64{r, g, b}), bg)
		}
	} else {
		for cr < ratio && (r < 255 || g < 255 || b < 255) {
			r = math.Min(255, r+math.Ceil((255-r)*0.1))
			g = math.Min(255, g+math.Ceil((255-g)*0.1))
			b = math.Min(255, b+math.Ceil((255-b)*0.1))
			cr = contrastOr21(toHex([3]float64{r, g, b}), bg)
		}
	}
	return toHex([3]float64{r, g, b})
}

// Gray of the same luma
func grayOf(color string) string {
	rgb, ok := parseColor(color)
	if !ok {
		return color
	}
	gray := clampChannel(0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2])
	return toHex([3]float64{gray, gray, gray})
}

// Color blended toward its gray by s
func desaturate(color string, s float64) string { return mixColor(color, grayOf(color), s) }

var xterm6Levels = [6]float64{0, 95, 135, 175, 215, 255}

const (
	ansiLightMaxLuminance    = 0.72
	ansiLightTargetLuminance = 0.34
	ansiLightMinSaturation   = 0.22
	ansiMutedBucket          = 245
)

var (
	ansiNormalizedForegrounds = []string{"text", "label", "ok", "error", "warn", "prompt", "statusFg", "statusGood", "statusWarn", "statusBad", "statusCritical", "shellDollar", "tool"}
	ansiMutedForegrounds      = []string{"muted", "sessionLabel", "sessionBorder", "thinking"}
)

// Red, green and blue of an xterm 256 color index past the sixteen system colors
func xtermEightBitRGB(n int) [3]float64 {
	if n >= 232 {
		v := float64(8 + (n-232)*10)
		return [3]float64{v, v, v}
	}
	if n >= 16 {
		o := n - 16
		return [3]float64{xterm6Levels[o/36%6], xterm6Levels[o/6%6], xterm6Levels[o%6]}
	}
	return [3]float64{}
}

func rgbLuminance(rgb [3]float64) float64 {
	l, _ := relativeLuminance(toHex(rgb))
	return l
}

// Hue, saturation and lightness, each 0 through 1
func rgbToHSL(red, green, blue float64) (float64, float64, float64) {
	rn, gn, bn := red/255, green/255, blue/255
	hi, lo := math.Max(rn, math.Max(gn, bn)), math.Min(rn, math.Min(gn, bn))
	lightness := (hi + lo) / 2
	if hi == lo {
		return 0, 0, lightness
	}
	delta := hi - lo
	saturation := delta / (hi + lo)
	if lightness > 0.5 {
		saturation = delta / (2 - hi - lo)
	}
	var hue float64
	switch hi {
	case rn:
		hue = (gn - bn) / delta
		if gn < bn {
			hue += 6
		}
	case gn:
		hue = (bn-rn)/delta + 2
	default:
		hue = (rn-gn)/delta + 4
	}
	return hue / 6, saturation, lightness
}

func circularDistance(a, b float64) float64 {
	d := math.Abs(a - b)
	return math.Min(d, 1-d)
}

// Nearest xterm 256 color the way Rich picks it, grays to the gray ramp
func richEightBitColorNumber(red, green, blue float64) int {
	_, saturation, lightness := rgbToHSL(red, green, blue)
	if saturation < 0.15 {
		gray := int(jsRound(lightness * 25))
		switch gray {
		case 0:
			return 16
		case 25:
			return 231
		}
		return 231 + gray
	}
	six := func(v float64) float64 {
		if v < 95 {
			return v / 95
		}
		return 1 + (v-95)/40
	}
	return 16 + 36*int(jsRound(six(red))) + 6*int(jsRound(six(green))) + int(jsRound(six(blue)))
}

// Xterm 256 color closest in hue that stays readable on a light ANSI background
func bestReadableAnsiColor(red, green, blue float64) int {
	hue, saturation, lightness := rgbToHSL(red, green, blue)
	best, bestScore := richEightBitColorNumber(red, green, blue), math.Inf(1)
	for n := 16; n <= 255; n++ {
		rgb := xtermEightBitRGB(n)
		if rgbLuminance(rgb) > ansiLightMaxLuminance {
			continue
		}
		ch, cs, cl := rgbToHSL(rgb[0], rgb[1], rgb[2])
		floorPenalty := 0.0
		if cs < ansiLightMinSaturation {
			floorPenalty = (ansiLightMinSaturation - cs) * 3
		}
		score := circularDistance(ch, hue)*4 + math.Abs(cs-math.Max(ansiLightMinSaturation, saturation))*0.8 + math.Abs(cl-math.Min(lightness, ansiLightTargetLuminance))*2 + floorPenalty
		if score < bestScore {
			best, bestScore = n, score
		}
	}
	return best
}

// Foreground mapped to an ansi256 tone readable on a light ANSI terminal
func normalizeAnsiForeground(color string) string {
	rgb, ok := parseColor(color)
	if !ok {
		return color
	}
	ansi := richEightBitColorNumber(rgb[0], rgb[1], rgb[2])
	if rgbLuminance(xtermEightBitRGB(ansi)) > ansiLightMaxLuminance {
		ansi = bestReadableAnsiColor(rgb[0], rgb[1], rgb[2])
	}
	return "ansi256(" + strconv.Itoa(ansi) + ")"
}

// Hex of a theme tone, empty when it names no color
func themeToneHex(tone string) string {
	if m := ansi256Re.FindStringSubmatch(strings.TrimFunc(tone, isJSSpace)); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n <= 255 {
			return toHex(xtermEightBitRGB(n))
		}
		return ""
	}
	if rgb, ok := parseColor(tone); ok {
		return toHex(rgb)
	}
	return ""
}

var defaultBrand = themeBrand{Name: "Icebath Agent", Icon: "☤", Prompt: "❯", Welcome: "Type your message or /help for commands.", Goodbye: "Goodbye! ☤", Tool: "┊", HelpHeader: "(^_^)? Commands"}

var jsWhitespaceRun = regexp.MustCompile(`[` + jsWS + `]+`)

// Prompt symbol on one line, the fallback when empty
func cleanPromptSymbol(s, fallback string) string {
	if cleaned := strings.TrimFunc(jsWhitespaceRun.ReplaceAllString(s, " "), isJSSpace); cleaned != "" {
		return cleaned
	}
	return fallback
}

// Seed colors a palette derives from, empty optional seeds derived
type themeSeeds struct {
	Accent, ActiveRow, Bg, Border, Error, Muted, Ok, Primary, Prompt, Selection string
	ShellDollar, StatusBad, StatusCritical, StatusGood, StatusWarn, Surface     string
	Text, Warn                                                                  string
}

// Derived neutral tones of a seed set
type themeTones struct {
	Muted, Label, StatusFg, Surface, ActiveRow, Selection, Border string
}

// Neutral tones blended from the accent, background and text seeds
func deriveTones(s themeSeeds) themeTones {
	bgLum, _ := relativeLuminance(s.Bg)
	isLight := bgLum > 0.5
	surfaceMix, fgMix := 0.09, 0.24
	if isLight {
		surfaceMix, fgMix = 0.045, 0.3
	}
	surface := mixColor(s.Bg, desaturate(s.Accent, 0.15), surfaceMix)
	t := themeTones{
		Muted:     desaturate(mixColor(s.Accent, s.Bg, 0.19), 0.16),
		Label:     desaturate(mixColor(s.Accent, s.Bg, 0.13), 0.16),
		StatusFg:  grayOf(mixColor(s.Text, s.Bg, fgMix)),
		Surface:   surface,
		ActiveRow: mixColor(surface, s.Accent, 0.25),
		Selection: mixColor(surface, s.Accent, 0.28),
		Border:    mixColor(s.Accent, s.Bg, 0.25),
	}
	if isLight {
		t.Muted = desaturate(s.Accent, 0.05)
		t.Label = desaturate(mixColor(s.Accent, s.Text, 0.03), 0.15)
		if s.ShellDollar != "" {
			t.Selection = mixColor(s.Bg, s.ShellDollar, 0.2)
		}
	}
	return t
}

// Seed when set, else the fallback
func seedOr(seed, fallback string) string {
	if seed != "" {
		return seed
	}
	return fallback
}

// Full palette of a seed set
func buildPalette(s themeSeeds, isLight bool) themeColors {
	tones := deriveTones(s)
	surface := seedOr(s.Surface, tones.Surface)
	activeRow := seedOr(s.ActiveRow, tones.ActiveRow)
	if s.ActiveRow == "" && s.Surface != "" {
		activeRow = mixColor(surface, s.Accent, 0.22)
	}
	muted, border := seedOr(s.Muted, tones.Muted), seedOr(s.Border, tones.Border)
	c := themeColors{
		Primary: s.Primary, Accent: s.Accent, Border: border, Text: s.Text, Muted: muted,
		CompletionBg: surface, CompletionCurrentBg: activeRow, CompletionMetaBg: surface, CompletionMetaCurrentBg: activeRow,
		Label: tones.Label, Ok: s.Ok, Error: s.Error, Warn: s.Warn, Tool: s.Accent, Thinking: muted,
		SyntaxString: s.Accent, SyntaxNumber: s.Text, SyntaxKeyword: border, SyntaxComment: muted,
		Prompt: seedOr(s.Prompt, s.Text), SessionLabel: muted, SessionBorder: muted, StatusBg: surface, StatusFg: tones.StatusFg,
		StatusGood: s.StatusGood, StatusWarn: s.StatusWarn, StatusBad: s.StatusBad, StatusCritical: s.StatusCritical,
		SelectionBg: seedOr(s.Selection, tones.Selection), ShellDollar: s.ShellDollar,
		DiffAdded: "rgb(220,255,220)", DiffRemoved: "rgb(255,220,220)", DiffAddedWord: "rgb(36,138,61)", DiffRemovedWord: "rgb(207,34,46)",
	}
	if isLight {
		c.DiffAdded, c.DiffRemoved, c.DiffAddedWord, c.DiffRemovedWord = "rgb(200,240,200)", "rgb(240,200,200)", "rgb(27,94,32)", "rgb(183,28,28)"
	}
	return c
}

var (
	darkSeeds  = themeSeeds{Accent: "#FFBF00", ActiveRow: "#333355", Bg: "#101014", Border: "#CD7F32", Error: "#ef5350", Ok: "#4caf50", Primary: "#FFD700", Prompt: "#FFF8DC", Selection: "#3a3a55", ShellDollar: "#4dabf7", StatusBad: "#FF8C00", StatusCritical: "#FF6B6B", StatusGood: "#8FBC8F", StatusWarn: "#FFD700", Surface: "#1a1a2e", Text: "#FFF8DC", Warn: "#ffa726"}
	lightSeeds = themeSeeds{Accent: "#956E00", Bg: "#ffffff", Border: "#A56628", Error: "#C14240", Ok: "#367E39", Primary: "#867000", Prompt: "#2B2014", ShellDollar: "#377BB3", StatusBad: "#A65A00", StatusCritical: "#B94D4D", StatusGood: "#5C7A5C", StatusWarn: "#867000", Text: "#3D2F13", Warn: "#956115"}
	darkTheme  = theme{Color: buildPalette(darkSeeds, false), Brand: defaultBrand}
	lightTheme = theme{Color: buildPalette(lightSeeds, true), Brand: defaultBrand}
)

const (
	displayMinContrast       = 1.45
	semanticMinContrast      = 2.2
	lightDisplayMinContrast  = 1.18
	lightSemanticMinContrast = 1.6
	lightBgMinLuminance      = 0.4
	darkBgMaxLuminance       = 0.35
	lumaLightThreshold       = 0.6
)

var (
	displayForegrounds  = []string{"primary", "accent", "text", "label", "prompt", "statusFg", "border", "muted", "sessionLabel", "sessionBorder", "shellDollar"}
	semanticForegrounds = []string{"ok", "error", "warn", "statusGood", "statusWarn", "statusBad", "statusCritical"}
	adaptiveBackgrounds = []string{"completionBg", "completionCurrentBg", "completionMetaBg", "completionMetaCurrentBg", "statusBg", "selectionBg"}
)

// Foregrounds lifted to stay readable on the background, backgrounds of the wrong polarity reset to the base palette
func adaptColorsToBackground(colors themeColors, isLight bool, base themeColors, bg string) themeColors {
	out := colors
	displayFloor, semanticFloor := displayMinContrast, semanticMinContrast
	if isLight {
		displayFloor, semanticFloor = lightDisplayMinContrast, lightSemanticMinContrast
	}
	for _, k := range displayForegrounds {
		*out.slot(k) = liftForContrast(*out.slot(k), bg, displayFloor)
	}
	for _, k := range semanticForegrounds {
		*out.slot(k) = liftForContrast(*out.slot(k), bg, semanticFloor)
	}
	for _, k := range adaptiveBackgrounds {
		l, ok := relativeLuminance(*out.slot(k))
		if !ok {
			continue
		}
		if (isLight && l < lightBgMinLuminance) || (!isLight && l > darkBgMaxLuminance) {
			*out.slot(k) = *base.slot(k)
		}
	}
	return out
}

// Luma of a hex background hint, false when it is no hex
func backgroundLuminance(raw string) (float64, bool) {
	v := strings.ToLower(strings.TrimFunc(raw, isJSSpace))
	hex := strings.TrimPrefix(v, "#")
	var rgb [3]int64
	switch {
	case v == "":
		return 0, false
	case bgHex6Re.MatchString(hex):
		for i := range 3 {
			rgb[i], _ = strconv.ParseInt(hex[i*2:i*2+2], 16, 64)
		}
	case bgHex3Re.MatchString(hex):
		for i := range 3 {
			rgb[i], _ = strconv.ParseInt(strings.Repeat(hex[i:i+1], 2), 16, 64)
		}
	default:
		return 0, false
	}
	return (0.2126*float64(rgb[0]) + 0.7152*float64(rgb[1]) + 0.0722*float64(rgb[2])) / 255, true
}

// Background contrast is measured against, the cached terminal answer or the palette's own
func referenceBackground(isLight bool, env func(string) string) string {
	cached := strings.TrimFunc(env("ICEBATH_TUI_BACKGROUND"), isJSSpace)
	if _, ok := backgroundLuminance(cached); cached != "" && ok {
		if strings.HasPrefix(cached, "#") {
			return cached
		}
		return "#" + cached
	}
	if isLight {
		return "#ffffff"
	}
	return "#101014"
}

var lightDefaultTermPrograms = map[string]bool{"Apple_Terminal": true}

// Whether the terminal is light, by the light flag, the theme pin, the background hint, COLORFGBG, then the terminal program
func detectLightMode(env func(string) string) bool {
	lightFlag := strings.ToLower(strings.TrimFunc(env("ICEBATH_TUI_LIGHT"), isJSSpace))
	if trueRe.MatchString(lightFlag) {
		return true
	}
	if falseRe.MatchString(lightFlag) {
		return false
	}
	switch strings.ToLower(strings.TrimFunc(env("ICEBATH_TUI_THEME"), isJSSpace)) {
	case "light":
		return true
	case "dark":
		return false
	}
	if hint, ok := backgroundLuminance(env("ICEBATH_TUI_BACKGROUND")); ok {
		return hint >= lumaLightThreshold
	}
	if colorfgbg := strings.TrimFunc(env("COLORFGBG"), isJSSpace); colorfgbg != "" {
		fields := strings.Split(colorfgbg, ";")
		if last := fields[len(fields)-1]; digitsRe.MatchString(last) {
			bg, _ := strconv.ParseFloat(last, 64)
			if bg == 7 || bg == 15 {
				return true
			}
			if bg >= 0 && bg < 16 {
				return false
			}
		}
	}
	return lightDefaultTermPrograms[strings.TrimFunc(env("TERM_PROGRAM"), isJSSpace)]
}

// Whether a light Apple Terminal without truecolor needs the ansi256 palette
func shouldNormalizeAnsiLightTheme(env func(string) string, isLight bool) bool {
	colorTerm := strings.ToLower(strings.TrimFunc(env("COLORTERM"), isJSSpace))
	return strings.TrimFunc(env("TERM_PROGRAM"), isJSSpace) == "Apple_Terminal" && colorTerm != "truecolor" && colorTerm != "24bit" && isLight
}

// Theme with its foregrounds mapped to readable ansi256 tones on a light ANSI terminal
func normalizeThemeForAnsiLightTerminal(t theme, env func(string) string, isLight bool) theme {
	if !shouldNormalizeAnsiLightTheme(env, isLight) {
		return t
	}
	for _, k := range ansiNormalizedForegrounds {
		*t.Color.slot(k) = normalizeAnsiForeground(*t.Color.slot(k))
	}
	for _, k := range ansiMutedForegrounds {
		*t.Color.slot(k) = "ansi256(" + strconv.Itoa(ansiMutedBucket) + ")"
	}
	return t
}

// Skinless theme for the background the environment reports
func defaultThemeForCurrentBackground(env func(string) string) theme {
	isLight := detectLightMode(env)
	base := darkTheme
	if isLight {
		base = lightTheme
	}
	return normalizeThemeForAnsiLightTerminal(base, env, isLight)
}

// Theme of the first frame, the last session's boot theme or the default for the background
func startupTheme() theme {
	if boot := loadBootTheme(); boot.theme != nil {
		return *boot.theme
	}
	return defaultThemeForCurrentBackground(os.Getenv)
}

// Background a skin authored, false when it set none
func authoredBackground(raw string) (string, bool) {
	v := strings.TrimFunc(raw, isJSSpace)
	if _, ok := backgroundLuminance(v); !ok {
		return "", false
	}
	if strings.HasPrefix(v, "#") {
		return v, true
	}
	return "#" + v, true
}

// Whether a skin is light by its authored background, else by the terminal
func skinIsLight(colors map[string]string, env func(string) string) bool {
	if authored, ok := backgroundLuminance(colors["background"]); ok {
		return authored >= lumaLightThreshold
	}
	return detectLightMode(env)
}

// Theme of a skin's colors, branding and banner art over the base palette of its polarity
func fromSkin(colors map[string]string, branding map[string]string, bannerLogo, bannerHero, toolPrefix, helpHeader string, env func(string) string) theme {
	isLight := skinIsLight(colors, env)
	bg, authored := authoredBackground(colors["background"])
	if !authored {
		bg = referenceBackground(isLight, env)
	}
	base, d := darkSeeds, darkTheme
	if isLight {
		base, d = lightSeeds, lightTheme
	}
	c := func(keys ...string) string {
		for _, k := range keys {
			if v, has := colors[k]; has {
				return v
			}
		}
		return ""
	}
	pick := func(fallback string, keys ...string) string {
		for _, k := range keys {
			if v, has := colors[k]; has {
				return v
			}
		}
		return fallback
	}
	seeds := themeSeeds{
		Accent:         pick(base.Accent, "ui_accent", "banner_accent"),
		Bg:             bg,
		Border:         pick(base.Border, "ui_border", "banner_border"),
		Error:          pick(base.Error, "ui_error"),
		Ok:             pick(base.Ok, "ui_ok"),
		Primary:        pick(base.Primary, "ui_primary", "banner_title"),
		Prompt:         pick(base.Prompt, "prompt", "banner_text"),
		ShellDollar:    pick(base.ShellDollar, "shell_dollar"),
		StatusBad:      pick(base.StatusBad, "status_bar_bad"),
		StatusCritical: pick(base.StatusCritical, "status_bar_critical", "ui_error"),
		StatusGood:     pick(base.StatusGood, "status_bar_good", "ui_ok"),
		StatusWarn:     pick(base.StatusWarn, "status_bar_warn", "ui_warn"),
		Text:           pick(base.Text, "ui_text", "banner_text"),
		Warn:           pick(base.Warn, "ui_warn"),
	}
	if len(colors) == 0 {
		seeds.ActiveRow, seeds.Muted, seeds.Selection, seeds.Surface = base.ActiveRow, base.Muted, base.Selection, base.Surface
	}
	derived := buildPalette(seeds, isLight)
	surface := pick(derived.CompletionBg, "completion_menu_bg", "background")
	activeRow := derived.CompletionCurrentBg
	if v, has := colors["completion_menu_current_bg"]; has {
		activeRow = v
	} else if c("completion_menu_bg") != "" {
		activeRow = mixColor(surface, seeds.Accent, 0.22)
	}
	assembled := derived
	assembled.Muted = pick(derived.Muted, "banner_dim")
	assembled.Label = pick(derived.Label, "ui_label")
	assembled.CompletionBg, assembled.CompletionCurrentBg = surface, activeRow
	assembled.CompletionMetaBg = pick(surface, "completion_menu_meta_bg")
	assembled.CompletionMetaCurrentBg = pick(activeRow, "completion_menu_meta_current_bg")
	assembled.SessionLabel = pick(derived.SessionLabel, "session_label", "banner_dim")
	assembled.SessionBorder = pick(derived.SessionBorder, "session_border", "banner_dim")
	assembled.StatusBg = pick(surface, "status_bar_bg")
	assembled.StatusFg = pick(derived.StatusFg, "status_bar_text", "ui_text", "banner_text")
	assembled.SelectionBg = pick(derived.SelectionBg, "selection_bg", "completion_menu_current_bg")
	assembled.Tool = pick(derived.Tool, "ui_tool")
	assembled.Thinking = pick(derived.Thinking, "ui_thinking", "banner_dim")
	assembled.DiffAdded = pick(derived.DiffAdded, "diff_added")
	assembled.DiffRemoved = pick(derived.DiffRemoved, "diff_removed")
	assembled.DiffAddedWord = pick(derived.DiffAddedWord, "diff_added_word")
	assembled.DiffRemovedWord = pick(derived.DiffRemovedWord, "diff_removed_word")
	assembled.SyntaxString = pick(derived.SyntaxString, "syntax_string")
	assembled.SyntaxNumber = pick(derived.SyntaxNumber, "syntax_number")
	assembled.SyntaxKeyword = pick(derived.SyntaxKeyword, "syntax_keyword")
	assembled.SyntaxComment = pick(derived.SyntaxComment, "syntax_comment", "banner_dim")
	adapted := assembled
	if !shouldNormalizeAnsiLightTheme(env, isLight) {
		adapted = adaptColorsToBackground(assembled, isLight, derived, bg)
	}
	brand := d.Brand
	if v, has := branding["agent_name"]; has {
		brand.Name = v
	}
	brand.Prompt = cleanPromptSymbol(branding["prompt_symbol"], d.Brand.Prompt)
	if v, has := branding["welcome"]; has {
		brand.Welcome = v
	}
	if v, has := branding["goodbye"]; has {
		brand.Goodbye = v
	}
	if toolPrefix != "" {
		brand.Tool = toolPrefix
	}
	if helpHeader != "" {
		brand.HelpHeader = helpHeader
	}
	if v, has := branding["help_header"]; has {
		brand.HelpHeader = v
	}
	return normalizeThemeForAnsiLightTerminal(theme{Color: adapted, Brand: brand, BannerLogo: bannerLogo, BannerHero: bannerHero}, env, isLight)
}
