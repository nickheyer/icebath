package theme

import (
	"image/color"
	"reflect"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func rgb(c color.Color) (uint8, uint8, uint8) {
	r, g, b, _ := c.RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
}

func TestMixEndpoints(t *testing.T) {
	a, b := Hex(0x102030), Hex(0xf0e0d0)
	if Mix(a, b, 0) != a || Mix(a, b, 1) != b {
		t.Fatal("endpoints must be preserved")
	}
	r, g, bl := rgb(Mix(a, b, 0.5))
	if r != 0x80 || g != 0x80 || bl != 0x80 {
		t.Fatalf("midpoint %x %x %x", r, g, bl)
	}
	if Mix(nil, b, 0.3) != b || Mix(a, nil, 0.3) != a || Mix(nil, nil, 0.3) != nil {
		t.Fatal("nil sides must fall back to the other side")
	}
}

func TestRingWraps(t *testing.T) {
	ring := Ring(24, Hex(0x5ff1ff), Hex(0xff4fd8))
	if len(ring) != 24 {
		t.Fatalf("len %d", len(ring))
	}
	fr, fg, fb := rgb(ring[0])
	lr, lg, lb := rgb(ring[len(ring)-1])
	if abs(int(fr)-int(lr)) > 40 || abs(int(fg)-int(lg)) > 40 || abs(int(fb)-int(lb)) > 40 {
		t.Fatalf("ring does not wrap, first %x%x%x last %x%x%x", fr, fg, fb, lr, lg, lb)
	}
	if len(Ring(5, Hex(0x010203))) != 5 || Ring(3) != nil {
		t.Fatal("degenerate rings")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestVariantsAndStyles(t *testing.T) {
	names := Names()
	if len(names) != 4 || names[0] != "blackwall" || names[3] != "plain" {
		t.Fatalf("%v", names)
	}
	for _, n := range names {
		th, ok := Named(n)
		if !ok || th.Name != n || th.Bg == nil || th.Text == nil || len(th.Ring) == 0 {
			t.Fatalf("%s %+v", n, th)
		}
	}
	if _, ok := Named("nope"); ok {
		t.Fatal("unknown theme named")
	}
	if !Plain().Plain || Light().Plain || Contrast().Plain {
		t.Fatal("variant traits")
	}
	th, err := (Style{Palette: "light", Border: "double", Motion: 0.5, Density: "compact"}).Resolve()
	if err != nil || th.Name != "light" || th.Border.TopLeft != "╔" || th.Density != Compact {
		t.Fatalf("%v %+v", err, th)
	}
	th, err = (Style{}).Resolve()
	if err != nil || th.Name != "blackwall" || th.Density != Comfortable {
		t.Fatalf("%v %+v", err, th)
	}
	if th, err = (Style{Density: "comfortable"}).Resolve(); err != nil || th.Density != Comfortable {
		t.Fatal(err)
	}
	for _, bad := range []Style{{Palette: "x"}, {Border: "x"}, {Density: "x"}, {Motion: 2}} {
		if _, err := bad.Resolve(); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	if len(Borders()) != 8 || Borders()[4] != "icebath" {
		t.Fatalf("%v", Borders())
	}
}

func TestGlyphSetsAreCompleteAndSingleCell(t *testing.T) {
	for _, g := range []Glyphs{IcebathGlyphs(), ASCIIGlyphs()} {
		v := reflect.ValueOf(g)
		for i := range v.NumField() {
			f := v.Field(i)
			name := v.Type().Field(i).Name
			switch f.Kind() {
			case reflect.String:
				if f.String() == "" {
					t.Fatalf("%s is empty", name)
				}
				if name != "Ellipsis" && ansi.StringWidth(f.String()) != 1 {
					t.Fatalf("%s %q is not one cell", name, f.String())
				}
			case reflect.Slice:
				if f.Len() == 0 {
					t.Fatalf("%s is empty", name)
				}
			}
		}
	}
	if len(IcebathGlyphs().Ramp) != 8 || len(ASCIIGlyphs().Ramp) != 8 {
		t.Fatal("ramps hold eight levels")
	}
	for chord, sym := range IcebathGlyphs().Chords {
		if ansi.StringWidth(sym) != 1 {
			t.Fatalf("chord %s symbol %q is not one cell", chord, sym)
		}
	}
	if len(ASCIIGlyphs().Chords) != 0 {
		t.Fatal("the ascii set spells chords out")
	}
	for _, r := range ASCIIGlyphs().Churn {
		if r > 0x7f {
			t.Fatalf("ascii churn holds %q", r)
		}
	}
	if Blackwall().Glyphs.Session != "❆" || Plain().Glyphs.Session != "~" || Blackwall().Border.TopLeft != "┍" {
		t.Fatal("themes carry their glyph set and border")
	}
}
