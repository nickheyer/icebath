package pypil

import (
	"image"
	"image/color"
	"testing"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func TestThumbnailSizeMatchesPillowAspectRounding(t *testing.T) {
	for _, c := range []struct{ w, h, side, ww, wh int }{
		{2880, 1800, 1456, 1456, 910},
		{3024, 1964, 1456, 1456, 946},
		{1000, 3000, 1456, 485, 1456},
		{5000, 10, 1456, 1456, 3},
		{10, 5000, 1456, 3, 1456},
	} {
		w, h, ok := ThumbnailSize(c.w, c.h, c.side)
		if !ok || w != c.ww || h != c.wh {
			t.Fatalf("%dx%d in %d -> %dx%d %v want %dx%d", c.w, c.h, c.side, w, h, ok, c.ww, c.wh)
		}
	}
	if w, h, ok := ThumbnailSize(800, 600, 1456); ok || w != 800 || h != 600 {
		t.Fatalf("fitting image %dx%d %v", w, h, ok)
	}
}

func TestThumbnailKeepsSolidColourOpaqueAndWithAlpha(t *testing.T) {
	opaque := Thumbnail(solid(2880, 1800, color.NRGBA{10, 200, 30, 255}), 1456, false)
	if opaque.Rect != image.Rect(0, 0, 1456, 910) || opaque.NRGBAAt(700, 400) != (color.NRGBA{10, 200, 30, 255}) {
		t.Fatalf("opaque %v %v", opaque.Rect, opaque.NRGBAAt(700, 400))
	}
	clear := Thumbnail(solid(400, 300, color.NRGBA{0, 0, 0, 0}), 200, true)
	if clear.Rect != image.Rect(0, 0, 200, 150) || clear.NRGBAAt(100, 75).A != 0 {
		t.Fatalf("alpha %v %v", clear.Rect, clear.NRGBAAt(100, 75))
	}
	same := Thumbnail(solid(100, 50, color.NRGBA{1, 2, 3, 255}), 200, false)
	if same.Rect != image.Rect(0, 0, 100, 50) || same.NRGBAAt(5, 5) != (color.NRGBA{1, 2, 3, 255}) {
		t.Fatalf("inside the box %v", same.Rect)
	}
}

func TestToNRGBAConvertsGrayToStraightRGBA(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 2, 2))
	g.SetGray(1, 1, color.Gray{Y: 77})
	out := ToNRGBA(g)
	if out.NRGBAAt(1, 1) != (color.NRGBA{77, 77, 77, 255}) || out.NRGBAAt(0, 0) != (color.NRGBA{0, 0, 0, 255}) {
		t.Fatalf("gray conversion %v %v", out.NRGBAAt(1, 1), out.NRGBAAt(0, 0))
	}
}
