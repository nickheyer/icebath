package termlab

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

// Solid image of the size and color
func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestImageHelpers(t *testing.T) {
	bg := color.RGBA{10, 20, 30, 255}
	a := solid(4, 4, bg)
	b := solid(4, 4, bg)
	b.SetRGBA(2, 1, color.RGBA{200, 0, 0, 255})
	all := a.Bounds()
	if !Uniform(a, all) || Uniform(b, all) || !Uniform(b, image.Rect(0, 2, 4, 4)) {
		t.Error("Uniform misjudged a solid image or one with a stray pixel")
	}
	if got := Differs(a, b, all); got != 1.0/16 {
		t.Errorf("Differs %v, want 1/16", got)
	}
	if got := Coverage(b, all, bg); got != 1.0/16 {
		t.Errorf("Coverage %v, want 1/16", got)
	}
	if got := DiffBounds(a, b, all); got != image.Rect(2, 1, 3, 2) {
		t.Errorf("DiffBounds %v", got)
	}
	if got := Sample(b, []image.Rectangle{image.Rect(2, 1, 3, 2), image.Rect(3, 3, 9, 9)}); len(got) != 2 || got[0] != (color.RGBA{200, 0, 0, 255}) || got[1] != bg {
		t.Errorf("Sample %v", got)
	}
}

func TestDecodePPM(t *testing.T) {
	img, err := decodePPM(bytes.NewReader(append([]byte("P6\n2 1\n255\n"), 1, 2, 3, 4, 5, 6)))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 2, 1) || img.RGBAAt(0, 0) != (color.RGBA{1, 2, 3, 255}) || img.RGBAAt(1, 0) != (color.RGBA{4, 5, 6, 255}) {
		t.Errorf("decoded %v %v %v", img.Bounds(), img.RGBAAt(0, 0), img.RGBAAt(1, 0))
	}
	if _, err := decodePPM(bytes.NewReader([]byte("P3\n1 1\n255\n0 0 0"))); err == nil {
		t.Error("a plain text pixmap decoded")
	}
}
