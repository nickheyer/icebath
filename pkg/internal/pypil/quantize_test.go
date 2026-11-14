package pypil

import (
	"encoding/hex"
	"image"
	"testing"
)

type quantLcg struct{ s uint32 }

// Next byte of the generator the recording script uses to build its images
func (g *quantLcg) byte() uint8 {
	g.s = (g.s*1103515245 + 12345) & 0x7FFFFFFF
	return uint8(g.s >> 16)
}

func TestQuantizeMedianCutRefusesBadColourCountsAndEmptyImages(t *testing.T) {
	img := &image.NRGBA{Pix: []byte{1, 2, 3, 4}, Stride: 4, Rect: image.Rect(0, 0, 1, 1)}
	for _, colors := range []int{0, -1, 257} {
		if _, err := QuantizeMedianCut(img, colors); err == nil || err.Error() != "bad number of colors" {
			t.Fatalf("colors %d error = %v, want bad number of colors", colors, err)
		}
	}
	for _, colors := range []int{1, 256} {
		q, err := QuantizeMedianCut(img, colors)
		if err != nil || len(q.Pix) != 1 || q.Pix[0] != 0 || hex.EncodeToString(q.Palette) != "010203" {
			t.Fatalf("colors %d result = %+v, %v", colors, q, err)
		}
	}
	empty := &image.NRGBA{Rect: image.Rect(0, 0, 0, 3)}
	if _, err := QuantizeMedianCut(empty, 255); err == nil || err.Error() != "quantization error" {
		t.Fatalf("zero width error = %v, want quantization error", err)
	}
}
