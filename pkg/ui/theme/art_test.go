package theme

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var bannerTagRE = regexp.MustCompile(`\[#[0-9a-f]{6}\]|\[/\]`)

func bannerArtLines(markup string) []string {
	lines := strings.Split(markup, "\n")
	for i, line := range lines {
		lines[i] = bannerTagRE.ReplaceAllString(line, "")
	}
	return lines
}

func TestBannerHeroIsThirtyBrailleCellsByFifteenRows(t *testing.T) {
	lines := bannerArtLines(Hero)
	if len(lines) != 15 {
		t.Fatalf("hero has %d rows", len(lines))
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) != 30 {
			t.Fatalf("hero row %q is not 30 cells", line)
		}
		for _, r := range line {
			if r < 0x2800 || r > 0x28ff {
				t.Fatalf("hero row %q holds a non braille cell", line)
			}
		}
	}
}

func TestBannerLogoFitsTheWideTerminalItPrintsIn(t *testing.T) {
	lines := bannerArtLines(Logo)
	if len(lines) != 6 {
		t.Fatalf("logo has %d rows", len(lines))
	}
	width := utf8.RuneCountInString(lines[0])
	if width >= 95 {
		t.Fatalf("logo is %d cells, the banner prints it from 95 columns", width)
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("ragged logo row %q", line)
		}
	}
	for _, markup := range []string{Logo, Hero} {
		if !strings.Contains(markup, "["+LogoBar+"]") || !strings.Contains(markup, "["+LogoInk+"]") {
			t.Fatal("art lost the ice letter or red accent colour")
		}
	}
}

func TestEveryNamedThemeIsCompleteAndListed(t *testing.T) {
	names := Names()
	if len(names) != 7 || !strings.Contains(strings.Join(names, " "), DefaultName) {
		t.Fatalf("names %v", names)
	}
	for _, name := range names {
		th, ok := Named("  " + strings.ToUpper(name) + " ")
		if !ok || th.Name != name {
			t.Fatalf("%s resolves to %q, %v", name, th.Name, ok)
		}
		for slot, c := range map[string]any{"bg": th.Bg, "surface": th.Surface, "text": th.Text, "dim": th.TextDim, "steel": th.Steel,
			"steel bright": th.SteelBright, "ghost": th.Ghost, "accent": th.Accent, "ice": th.Ice, "pulse": th.Pulse, "alive": th.Alive, "alarm": th.Alarm} {
			if c == nil {
				t.Fatalf("%s has no %s", name, slot)
			}
		}
		if len(th.Ring) == 0 || th.Glyphs.Brand == "" || th.Glyphs.Prompt == "" || th.Border.TopLeft == "" {
			t.Fatalf("%s lacks a ring, glyphs or border", name)
		}
	}
	if _, ok := Named("gold"); ok {
		t.Fatal("an unknown name resolved")
	}
	if plain := Plain(); !plain.Plain || plain.Glyphs.Brand != "#" || plain.Border.TopLeft != "+" {
		t.Fatalf("plain is not the ASCII theme: %q %q", plain.Glyphs.Brand, plain.Border.TopLeft)
	}
}
