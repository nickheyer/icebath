package pyrich

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsoleColorSystemOverridesTheTerminalDetection(t *testing.T) {
	setColorSystem(t, "standard")
	var forced, detected bytes.Buffer
	(&Console{Out: &forced, Color: true, Width: 40, ColorSystem: "truecolor"}).PrintPanel(Panel{Body: "[#FFBF00]x[/]", BorderStyle: "#CD7F32"})
	(&Console{Out: &detected, Color: true, Width: 40}).PrintPanel(Panel{Body: "[#FFBF00]x[/]", BorderStyle: "#CD7F32"})
	if !strings.Contains(forced.String(), "\x1b[38;2;205;127;50m╭") || !strings.Contains(forced.String(), "\x1b[38;2;255;191;0mx") {
		t.Fatalf("forced truecolor console wrote %q", forced.String())
	}
	if strings.Contains(detected.String(), "38;2;") {
		t.Fatalf("standard terminal console wrote truecolor %q", detected.String())
	}
	if got := RenderSystem("[#FFBF00]x[/]", true, "truecolor"); got != "\x1b[38;2;255;191;0mx\x1b[0m" {
		t.Fatalf("RenderSystem wrote %q", got)
	}
}

func TestPanelBodyIsNotReprHighlightedAndTitleIsCropped(t *testing.T) {
	setColorSystem(t, "truecolor")
	var b bytes.Buffer
	(&Console{Out: &b, Color: true, Width: 40}).PrintPanel(Panel{Body: "value 123 and /a/path True", Title: "t 5"})
	if want := "╭──────────────── t 5 ─────────────────╮\n│ value 123 and /a/path True           │\n╰──────────────────────────────────────╯\n"; b.String() != want {
		t.Fatalf("panel\n got %q\nwant %q", b.String(), want)
	}
	b.Reset()
	(&Console{Out: &b, Color: true, Width: 20}).PrintPanel(Panel{Body: "x", Title: "a long title that overflows the panel"})
	if want := "╭─ a long title th─╮\n│ x                │\n╰──────────────────╯\n"; b.String() != want {
		t.Fatalf("cropped title\n got %q\nwant %q", b.String(), want)
	}
}
