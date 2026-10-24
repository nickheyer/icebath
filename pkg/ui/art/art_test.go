package art

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestMarksKeepTheirShape(t *testing.T) {
	for _, plain := range []bool{false, true} {
		lines := Mark(plain)
		if len(lines) < 3 || Width(lines) < 20 {
			t.Fatalf("plain %v mark %v", plain, lines)
		}
		for _, l := range lines {
			for _, r := range l {
				if plain && r > 0x7f {
					t.Fatalf("plain mark holds %q", r)
				}
			}
		}
	}
	if Width(nil) != 0 {
		t.Fatal("empty width")
	}
}

func TestPaintSweepsColorsAcrossColumns(t *testing.T) {
	sweep := []color.Color{color.RGBA{R: 1, A: 255}, color.RGBA{B: 1, A: 255}}
	lines := Paint([]string{"ab cd"}, sweep)
	if len(lines) != 1 || ansi.Strip(lines[0]) != "ab cd" {
		t.Fatalf("%q", lines)
	}
	if !strings.Contains(lines[0], "38;2;1;0;0") || !strings.Contains(lines[0], "38;2;0;0;1") {
		t.Fatalf("both sweep colors paint %q", lines[0])
	}
	if got := Paint([]string{"x"}, nil); got[0] != "x" {
		t.Fatal("no sweep leaves the text alone")
	}
}
