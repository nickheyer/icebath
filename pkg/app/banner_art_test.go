package app

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
	lines := bannerArtLines(icebathHero)
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
	lines := bannerArtLines(icebathAgentLogo)
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
	for _, markup := range []string{icebathAgentLogo, icebathHero} {
		if !strings.Contains(markup, "[#f50325]") || !strings.Contains(markup, "[#e6f5fb]") {
			t.Fatal("art lost the ice letter or red accent colour")
		}
	}
}
