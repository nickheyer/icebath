package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var paletteRequiredKeys = []string{
	"banner_border", "banner_title", "banner_accent", "banner_dim", "banner_text",
	"ui_accent", "ui_label", "ui_ok", "ui_error", "ui_warn",
	"prompt", "input_rule", "response_border",
	"status_bar_bg", "status_bar_text", "status_bar_strong", "status_bar_dim",
	"status_bar_good", "status_bar_warn", "status_bar_bad", "status_bar_critical",
	"session_label", "session_border",
	"completion_menu_bg", "completion_menu_current_bg", "selection_bg",
	"shell_dollar", "voice_status_bg",
}

var paletteStrongFG = []string{
	"banner_title", "banner_accent", "banner_text", "ui_accent", "ui_label", "ui_ok", "ui_error",
	"prompt", "status_bar_strong", "status_bar_good", "status_bar_bad", "status_bar_critical", "shell_dollar",
}

var paletteSoftFG = []string{
	"banner_dim", "banner_border", "ui_warn", "input_rule", "response_border",
	"status_bar_dim", "status_bar_warn", "session_label", "session_border",
}

var (
	paletteOnStatusBar = []string{"status_bar_text", "status_bar_strong", "status_bar_dim"}
	paletteFills       = []string{"status_bar_bg", "completion_menu_bg", "completion_menu_current_bg", "selection_bg", "voice_status_bg"}
	paletteLightSkins  = []string{"daylight", "warm-lightmode"}
)

const (
	paletteStrongMin = 3.9
	paletteSoftMin   = 2.8
	paletteDarkPole  = "#101014"
	paletteLightPole = "#ffffff"
)

// WCAG relative luminance of a six digit hex color
func paletteLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	h := strings.TrimLeft(hex, "#")
	if len(h) != 6 {
		t.Fatalf("not a 6-digit hex: %q", hex)
	}
	var ch [3]float64
	for i := range 3 {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatalf("not a 6-digit hex: %q", hex)
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

// WCAG contrast ratio between two hex colors
func paletteContrast(t *testing.T, a, b string) float64 {
	la, lb := paletteLuminance(t, a), paletteLuminance(t, b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// Color text of a palette key, failing the test when the key is absent or not text
func paletteColor(t *testing.T, palette map[string]any, key string) string {
	t.Helper()
	v, ok := palette[key].(string)
	if !ok {
		t.Fatalf("palette key %s = %v", key, palette[key])
	}
	return v
}

func paletteCheckFills(t *testing.T, palette map[string]any, isLight bool, keys []string, problems *[]string) {
	for _, key := range keys {
		c := paletteColor(t, palette, key)
		lum := paletteLuminance(t, c)
		if isLight && lum < 0.4 {
			*problems = append(*problems, fmt.Sprintf("%s=%s is a dark fill (lum %.2f) in a light palette", key, c, lum))
		}
		if !isLight && lum > 0.35 {
			*problems = append(*problems, fmt.Sprintf("%s=%s is a light fill (lum %.2f) in a dark palette", key, c, lum))
		}
	}
}

func paletteCheckChip(t *testing.T, palette map[string]any, problems *[]string) {
	current, menu := paletteColor(t, palette, "completion_menu_current_bg"), paletteColor(t, palette, "completion_menu_bg")
	if chip := paletteContrast(t, current, menu); chip < 1.15 {
		*problems = append(*problems, fmt.Sprintf("completion_menu_current_bg=%s indistinguishable from completion_menu_bg (contrast %.2f)", current, chip))
	}
}

func TestBuiltinBasePalettes(t *testing.T) {
	for _, name := range BuiltinSkinNames() {
		skin, _ := builtinSkin(name)
		palette, _ := skin["colors"].(map[string]any)
		isLight := slices.Contains(paletteLightSkins, name)
		t.Run(name+":colors/complete", func(t *testing.T) {
			var missing []string
			for _, key := range paletteRequiredKeys {
				if _, ok := palette[key]; !ok {
					missing = append(missing, key)
				}
			}
			slices.Sort(missing)
			if len(missing) > 0 {
				t.Fatalf("%s.colors missing keys: %v", name, missing)
			}
		})
		t.Run(name+":colors/contrast", func(t *testing.T) {
			pole := paletteDarkPole
			if isLight {
				pole = paletteLightPole
			}
			var problems []string
			if !isLight {
				for _, group := range []struct {
					keys  []string
					floor float64
				}{{paletteStrongFG, paletteStrongMin}, {paletteSoftFG, paletteSoftMin}} {
					for _, key := range group.keys {
						c := paletteColor(t, palette, key)
						if ratio := paletteContrast(t, c, pole); ratio < group.floor {
							problems = append(problems, fmt.Sprintf("%s=%s contrast %.2f < %v vs %s", key, c, ratio, group.floor, pole))
						}
					}
				}
			}
			statusBG := paletteColor(t, palette, "status_bar_bg")
			for _, key := range paletteOnStatusBar {
				floor := paletteSoftMin
				if key == "status_bar_strong" {
					floor = paletteStrongMin
				}
				c := paletteColor(t, palette, key)
				if ratio := paletteContrast(t, c, statusBG); ratio < floor {
					problems = append(problems, fmt.Sprintf("%s=%s contrast %.2f < %v vs status_bar_bg %s", key, c, ratio, floor, statusBG))
				}
			}
			paletteCheckFills(t, palette, isLight, paletteFills, &problems)
			paletteCheckChip(t, palette, &problems)
			if len(problems) > 0 {
				t.Fatalf("%s.colors:\n  %s", name, strings.Join(problems, "\n  "))
			}
		})
	}
}

func TestBuiltinOverlayKeysAndFillPolarity(t *testing.T) {
	overlays := 0
	for _, name := range BuiltinSkinNames() {
		skin, _ := builtinSkin(name)
		for _, block := range []struct {
			key     string
			isLight bool
		}{{"light_colors", true}, {"dark_colors", false}} {
			palette, _ := skin[block.key].(map[string]any)
			if len(palette) == 0 {
				continue
			}
			overlays++
			t.Run(name+":"+block.key, func(t *testing.T) {
				var unknown []string
				for key := range palette {
					if !slices.Contains(paletteRequiredKeys, key) {
						unknown = append(unknown, key)
					}
				}
				slices.Sort(unknown)
				if len(unknown) > 0 {
					t.Fatalf("%s.%s has unknown keys: %v", name, block.key, unknown)
				}
				var problems []string
				var fills []string
				for _, key := range paletteFills {
					if _, ok := palette[key]; ok {
						fills = append(fills, key)
					}
				}
				paletteCheckFills(t, palette, block.isLight, fills, &problems)
				_, current := palette["completion_menu_current_bg"]
				_, menu := palette["completion_menu_bg"]
				if current && menu {
					paletteCheckChip(t, palette, &problems)
				}
				if len(problems) > 0 {
					t.Fatalf("%s.%s:\n  %s", name, block.key, strings.Join(problems, "\n  "))
				}
			})
		}
	}
	if overlays == 0 {
		t.Fatal("no built in skin carries a light or dark overlay")
	}
}
