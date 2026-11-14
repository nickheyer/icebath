package pypil

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"os"
	"testing"
)

type quantLcg struct{ s uint32 }

// Next byte of the generator the recording script uses to build its images
func (g *quantLcg) byte() uint8 {
	g.s = (g.s*1103515245 + 12345) & 0x7FFFFFFF
	return uint8(g.s >> 16)
}

// Rebuilds the RGBA bytes of a recorded case from its generator kind and seed
func quantCaseRGBA(kind string, w, h int, seed uint32, k int) []byte {
	out := make([]byte, 0, w*h*4)
	g := &quantLcg{s: seed}
	switch kind {
	case "lcg":
		for i := 0; i < w*h*4; i++ {
			out = append(out, g.byte())
		}
	case "few":
		pal := make([][3]uint8, k)
		for i := range pal {
			pal[i] = [3]uint8{g.byte(), g.byte(), g.byte()}
		}
		for i := 0; i < w*h; i++ {
			c := pal[int(g.byte())%k]
			out = append(out, c[0], c[1], c[2], g.byte())
		}
	case "grad":
		alphas := [4]uint8{0, 32, 33, 255}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				out = append(out, uint8(x*255/max(1, w-1)), uint8(y*255/max(1, h-1)), uint8(x*7+y*13), alphas[(x+2*y)%4])
			}
		}
	case "unique":
		for i := 0; i < w*h; i++ {
			out = append(out, uint8(i), uint8(i>>8), uint8((i>>16)*37), 255)
		}
	case "solid":
		for i := 0; i < w*h; i++ {
			out = append(out, uint8(seed), uint8(seed>>8), uint8(seed>>16), 200)
		}
	}
	return out
}

func TestQuantizeMedianCutMatchesPillow(t *testing.T) {
	raw, err := os.ReadFile("testdata/quantize_mediancut.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name        string `json:"name"`
			Kind        string `json:"kind"`
			W           int    `json:"w"`
			H           int    `json:"h"`
			Seed        uint32 `json:"seed"`
			K           int    `json:"k"`
			InputSHA256 string `json:"input_sha256"`
			Palette     string `json:"palette"`
			PixSHA256   string `json:"pix_sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) != 14 {
		t.Fatalf("recorded cases = %d, want 14", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data := quantCaseRGBA(c.Kind, c.W, c.H, c.Seed, c.K)
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != c.InputSHA256 {
				t.Fatalf("input sha256 = %s, want %s", got, c.InputSHA256)
			}
			img := &image.NRGBA{Pix: data, Stride: c.W * 4, Rect: image.Rect(0, 0, c.W, c.H)}
			q, err := QuantizeMedianCut(img, 255)
			if err != nil {
				t.Fatal(err)
			}
			if q.W != c.W || q.H != c.H {
				t.Fatalf("size = %dx%d, want %dx%d", q.W, q.H, c.W, c.H)
			}
			if got := hex.EncodeToString(q.Palette); got != c.Palette {
				t.Fatalf("palette = %s\nwant      %s", got, c.Palette)
			}
			pix := sha256.Sum256(q.Pix)
			if got := hex.EncodeToString(pix[:]); got != c.PixSHA256 {
				t.Fatalf("index sha256 = %s, want %s", got, c.PixSHA256)
			}
		})
	}
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
