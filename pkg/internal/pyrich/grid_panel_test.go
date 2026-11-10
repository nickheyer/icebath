package pyrich

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGridInPaddedPanelMatchesRich15(t *testing.T) {
	raw, err := os.ReadFile("testdata/grid_panel_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string   `json:"name"`
		Width  int      `json:"width"`
		System string   `json:"system"`
		Left   []string `json:"left"`
		Right  []string `json:"right"`
		Title  string   `json:"title"`
		Border string   `json:"border"`
		Want   string   `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases {
		setColorSystem(t, c.System)
		grid := NewGrid(2, Column{Header: "left", Justify: "left"}, Column{Header: "right", Justify: "left"})
		grid.AddRow(strings.Join(c.Left, "\n"), strings.Join(c.Right, "\n"))
		var b bytes.Buffer
		(&Console{Out: &b, Color: c.System != "none", Width: c.Width}).PrintPanel(Panel{Content: grid, Title: c.Title, BorderStyle: c.Border, PadX: 2})
		got := oscLinkIDRE.ReplaceAllString(b.String(), "\x1b]8;id=;")
		want := oscLinkIDRE.ReplaceAllString(c.Want, "\x1b]8;id=;")
		if got != want {
			t.Errorf("%s width %d %s\n got %q\nwant %q", c.Name, c.Width, c.System, got, want)
		}
	}
}

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
