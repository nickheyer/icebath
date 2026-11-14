package pet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"os"
	"testing"
)

type sixelLcg struct{ s uint32 }

// Next byte of the generator the recording script uses to build its frames
func (g *sixelLcg) byte() uint8 {
	g.s = (g.s*1103515245 + 12345) & 0x7FFFFFFF
	return uint8(g.s >> 16)
}

// Rebuilds the RGBA bytes of a recorded frame from its generator kind and seed
func sixelCaseRGBA(kind string, w, h int, seed uint32, k int) []byte {
	out := make([]byte, 0, w*h*4)
	g := &sixelLcg{s: seed}
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

func TestEncodeSixelMatchesHermesOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/sixel.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name        string  `json:"name"`
			Kind        string  `json:"kind"`
			W           int     `json:"w"`
			H           int     `json:"h"`
			Seed        uint32  `json:"seed"`
			K           int     `json:"k"`
			InputSHA256 string  `json:"input_sha256"`
			Length      int     `json:"length"`
			SHA256      string  `json:"sha256"`
			Text        *string `json:"text"`
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
			data := sixelCaseRGBA(c.Kind, c.W, c.H, c.Seed, c.K)
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != c.InputSHA256 {
				t.Fatalf("input sha256 = %s, want %s", got, c.InputSHA256)
			}
			got := encodeSixel(&image.NRGBA{Pix: data, Stride: c.W * 4, Rect: image.Rect(0, 0, c.W, c.H)})
			if c.Text != nil && got != *c.Text {
				t.Fatalf("sixel = %q\nwant    %q", got, *c.Text)
			}
			if len(got) != c.Length {
				t.Fatalf("length = %d, want %d", len(got), c.Length)
			}
			out := sha256.Sum256([]byte(got))
			if h := hex.EncodeToString(out[:]); h != c.SHA256 {
				t.Fatalf("sixel sha256 = %s, want %s", h, c.SHA256)
			}
		})
	}
}
