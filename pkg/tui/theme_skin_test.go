package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
)

func hexOf(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// One skin of the catalog the binary embeds, read from its source file since the TUI may not import the config package
type catalogSkin struct {
	Name        string            `json:"name"`
	Colors      map[string]string `json:"colors"`
	LightColors map[string]string `json:"light_colors"`
	DarkColors  map[string]string `json:"dark_colors"`
	Branding    map[string]string `json:"branding"`
}

// Built in skins by name and their names in catalog order
func skinCatalog(t *testing.T) (map[string]catalogSkin, []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "config", "builtin_skins.json"))
	if err != nil {
		t.Fatal(err)
	}
	skins := map[string]catalogSkin{}
	if err := json.Unmarshal(data, &skins); err != nil {
		t.Fatal(err)
	}
	var order []string
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.Token()
	for dec.More() {
		key, _ := dec.Token()
		order = append(order, key.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return skins, order
}

// Session theme of a built in skin as the gateway announces it
func builtinSkinTheme(t *testing.T, name string) (theme, catalogSkin) {
	t.Helper()
	skins, _ := skinCatalog(t)
	skin, ok := skins[name]
	if !ok || skin.Name != name {
		t.Fatalf("no built in skin %s", name)
	}
	return themeForSkin(&gatewaySkin{Name: skin.Name, Source: "builtin", Colors: skin.Colors, LightColors: skin.LightColors,
		DarkColors: skin.DarkColors, Branding: skin.Branding}, os.Getenv), skin
}

func darkTerminal(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ICEBATH_TUI_BACKGROUND", "COLORFGBG", "TERM_PROGRAM", "NO_COLOR"} {
		t.Setenv(k, "")
	}
	t.Setenv("ICEBATH_TUI_THEME", "dark")
	t.Setenv("TERM", "xterm-256color")
}

func TestBuiltinSkinsPaintWithTheToolkitThemeOfTheirName(t *testing.T) {
	darkTerminal(t)
	_, names := skinCatalog(t)
	if len(names) != len(uitheme.Names()) || names[0] != uitheme.DefaultName {
		t.Fatalf("skins %v, themes %v", names, uitheme.Names())
	}
	for _, name := range names {
		session, skin := builtinSkinTheme(t, name)
		want, ok := uitheme.Named(name)
		if !ok || session.Skin != name || !session.Builtin {
			t.Fatalf("%s: no toolkit theme, or the session theme lost its skin (%q, %v)", name, session.Skin, session.Builtin)
		}
		got := toolkitTheme(session, false)
		if hexOf(got.Accent) != hexOf(want.Accent) || hexOf(got.Ice) != hexOf(want.Ice) || hexOf(got.Text) != hexOf(want.Text) ||
			hexOf(got.Bg) != hexOf(want.Bg) || hexOf(got.Pulse) != hexOf(want.Pulse) || got.Plain != want.Plain || got.Border != want.Border {
			t.Fatalf("%s paints with another palette than its toolkit theme", name)
		}
		if want.Plain {
			continue
		}
		for key, slot := range map[string]color.Color{"banner_title": want.Accent, "banner_accent": want.Ice, "ui_accent": want.Ice, "banner_text": want.Text,
			"banner_dim": want.TextDim, "ui_ok": want.Alive, "ui_error": want.Alarm, "status_bar_bg": want.Surface, "status_bar_bad": want.Pulse, "banner_border": want.Ring[0]} {
			if c := strings.ToLower(skin.Colors[key]); c != hexOf(slot) {
				t.Errorf("%s.%s = %s, the toolkit theme has %s", name, key, c, hexOf(slot))
			}
		}
		if authored, has := skin.Colors["background"]; has && strings.ToLower(authored) != hexOf(want.Bg) {
			t.Errorf("%s authors background %v, the toolkit theme has %s", name, authored, hexOf(want.Bg))
		}
	}
}

func TestDefaultSkinFollowsTheTerminal(t *testing.T) {
	darkTerminal(t)
	session, skin := builtinSkinTheme(t, uitheme.DefaultName)
	if _, authored := skin.Colors["background"]; authored {
		t.Fatal("the default skin pins a background, so it cannot follow a light terminal")
	}
	if got := toolkitTheme(session, false); hexOf(got.Accent) != "#e3122b" || hexOf(got.Bg) != "#05070a" || got.Glyphs.Prompt != "$" {
		t.Fatalf("dark default is not Blackwall: %s on %s, prompt %q", hexOf(got.Accent), hexOf(got.Bg), got.Glyphs.Prompt)
	}
	if session.Color.Primary != "#e3122b" || session.Color.Accent != "#9cc7e8" || session.Color.Text != "#dfe9f2" {
		t.Fatalf("session tones %s %s %s", session.Color.Primary, session.Color.Accent, session.Color.Text)
	}
	t.Setenv("ICEBATH_TUI_THEME", "light")
	paper, _ := builtinSkinTheme(t, uitheme.DefaultName)
	light := uitheme.Light()
	if paper.Color.Text != hexOf(light.Text) || paper.Color.Primary != hexOf(light.Accent) {
		t.Fatalf("light terminal session tones %s %s", paper.Color.Text, paper.Color.Primary)
	}
	if got := toolkitTheme(paper, true); hexOf(got.Bg) != hexOf(light.Bg) || hexOf(got.Accent) != hexOf(light.Accent) {
		t.Fatalf("light terminal paints %s on %s", hexOf(got.Accent), hexOf(got.Bg))
	}
	t.Setenv("ICEBATH_TUI_THEME", "dark")
	for _, env := range [][2]string{{"TERM", "linux"}, {"TERM", "dumb"}, {"NO_COLOR", "1"}} {
		t.Setenv(env[0], env[1])
		if got := toolkitTheme(session, false); !got.Plain || got.Glyphs.Prompt != "$" || got.Border.TopLeft != "+" {
			t.Fatalf("%s=%s does not fall back to the plain theme", env[0], env[1])
		}
		if taro, _ := builtinSkinTheme(t, "taro"); toolkitTheme(taro, false).Plain {
			t.Fatalf("%s=%s overrides a skin the user picked", env[0], env[1])
		}
		t.Setenv(env[0], "")
	}
}

func TestUserSkinsLayTheirTonesOverABaseTheme(t *testing.T) {
	darkTerminal(t)
	custom := themeForSkin(&gatewaySkin{Name: "mine", Source: "user", Colors: map[string]string{"banner_title": "#112233", "ui_accent": "#445566"},
		Branding: map[string]string{"prompt_symbol": "λ"}}, os.Getenv)
	got := toolkitTheme(custom, false)
	if custom.Builtin || hexOf(got.Bg) != "#05070a" || got.Glyphs.Prompt != "λ" {
		t.Fatalf("an unknown user skin does not start from Blackwall: %s %q", hexOf(got.Bg), got.Glyphs.Prompt)
	}
	if hexOf(got.Accent) != themeToneHex(custom.Color.Primary) || hexOf(got.Ice) != themeToneHex(custom.Color.Accent) {
		t.Fatalf("user tones were not laid over the base: %s %s", hexOf(got.Accent), hexOf(got.Ice))
	}
	shadow := themeForSkin(&gatewaySkin{Name: "dracula", Source: "user", Colors: map[string]string{"banner_title": "#00ff00", "background": "#282a36"}}, os.Getenv)
	over := toolkitTheme(shadow, false)
	if hexOf(over.Bg) != "#282a36" || hexOf(over.Accent) != themeToneHex(shadow.Color.Primary) || hexOf(over.Accent) == hexOf(uitheme.Dracula().Accent) {
		t.Fatalf("a user file shadowing a built in skin keeps the built in accent: %s", hexOf(over.Accent))
	}
	if themesEqual(custom, shadow) || !themesEqual(custom, custom) {
		t.Fatal("theme equality ignores the skin")
	}
}

func artCtx(t *testing.T, name string, width int) renderCtx {
	t.Helper()
	session, _ := builtinSkinTheme(t, name)
	return renderCtx{t: session, ut: toolkitTheme(session, false), width: width}
}

func TestBannerArtFitsTheWindow(t *testing.T) {
	darkTerminal(t)
	wide := bannerArt(artCtx(t, uitheme.DefaultName, 120))
	if len(wide) != 6 {
		t.Fatalf("wide window shows %d rows, the wordmark has 6", len(wide))
	}
	for _, row := range wide {
		if w := ansi.StringWidth(row); w != transcriptIndent+77 {
			t.Fatalf("wordmark row is %d cells", w)
		}
	}
	if !strings.Contains(wide[0], "█") || strings.Contains(strings.Join(wide, ""), "[#") {
		t.Fatalf("wordmark kept its markup: %q", wide[0])
	}
	narrow := bannerArt(artCtx(t, uitheme.DefaultName, 60))
	if len(narrow) == 0 || len(narrow) >= 15 || ansi.StringWidth(narrow[0]) > transcriptIndent+30 {
		t.Fatalf("narrow window shows %d rows of the stacked mark", len(narrow))
	}
	if strings.Trim(ansi.Strip(narrow[0]), " ⠀") == "" || strings.Trim(ansi.Strip(narrow[len(narrow)-1]), " ⠀") == "" {
		t.Fatal("the stacked mark kept a blank edge row")
	}
	if rows := bannerArt(artCtx(t, uitheme.DefaultName, 24)); rows != nil {
		t.Fatalf("a window narrower than the art shows %d rows", len(rows))
	}
	compact := artCtx(t, uitheme.DefaultName, 120)
	compact.compact = true
	if bannerArt(compact) != nil || bannerArt(artCtx(t, "plain", 120)) != nil {
		t.Fatal("compact display or the plain theme paints art")
	}
	own := artCtx(t, uitheme.DefaultName, 120)
	own.t.BannerLogo = "[bold #ff0000]AB[/] [not a tag] [/x]c"
	rows := bannerArt(own)
	if len(rows) != 1 || ansi.Strip(rows[0]) != strings.Repeat(" ", transcriptIndent)+"AB [not a tag] c" {
		t.Fatalf("skin art rendered as %q", rows)
	}
}

func TestIntroCarriesTheArtTheMarkAndAFittedRule(t *testing.T) {
	darkTerminal(t)
	c := artCtx(t, uitheme.DefaultName, 120)
	lines := introRows(nil, c).lines
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = ansi.Strip(line)
	}
	if len(lines) < 9 || plain[6] != "" || !strings.HasPrefix(strings.TrimLeft(plain[7], " "), c.ut.Glyphs.Brand+" Icebath Agent") {
		t.Fatalf("intro rows %q", plain)
	}
	widest := 0
	for _, row := range plain[:len(plain)-1] {
		widest = max(widest, ansi.StringWidth(row)-transcriptIndent)
	}
	if rule := strings.TrimLeft(plain[len(plain)-1], " "); ansi.StringWidth(rule) != widest || strings.Trim(rule, "─") != "" {
		t.Fatalf("rule is %d cells under a %d cell block", ansi.StringWidth(rule), widest)
	}
	tight := introRows(nil, artCtx(t, uitheme.DefaultName, 30)).lines
	if rule := strings.TrimLeft(ansi.Strip(tight[len(tight)-1]), " "); ansi.StringWidth(rule) > 30-transcriptIndent*2 {
		t.Fatalf("rule overflows a 30 cell window: %d", ansi.StringWidth(rule))
	}
}
