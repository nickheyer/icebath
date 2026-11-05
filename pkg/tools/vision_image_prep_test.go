package tools

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"golang.org/x/image/bmp"
)

// Pillow 12 output for the deterministic fixtures below, resized with Image.LANCZOS
var (
	pilRGBDown  = []uint8{30, 38, 7, 112, 49, 28, 202, 61, 85, 64, 72, 168, 55, 162, 23, 144, 173, 44, 197, 185, 101, 70, 196, 184, 78, 73, 39, 182, 84, 60, 157, 96, 117, 53, 107, 200}
	pilRGBUp    = []uint8{2, 11, 1, 17, 13, 2, 49, 17, 5, 74, 21, 12, 99, 24, 22, 124, 27, 33, 150, 31, 49, 168, 35, 68, 223, 38, 88, 211, 41, 111, 45, 45, 136, 2, 49, 174, 51, 51, 196, 15, 79, 11, 32, 81, 12, 64, 85, 15, 89, 89, 22, 114, 92, 32, 140, 95, 43, 165, 99, 59, 182, 103, 78, 244, 106, 98, 238, 109, 121, 63, 113, 146, 14, 117, 184, 67, 119, 206, 31, 182, 21, 48, 184, 22, 80, 188, 25, 105, 192, 32, 130, 195, 42, 155, 198, 53, 181, 202, 69, 204, 206, 88, 239, 209, 108, 208, 212, 131, 67, 216, 156, 36, 220, 194, 81, 222, 216, 47, 124, 31, 64, 126, 32, 96, 130, 35, 121, 134, 42, 146, 137, 52, 167, 140, 63, 197, 144, 79, 252, 148, 98, 150, 151, 118, 0, 154, 141, 24, 158, 166, 81, 162, 204, 90, 164, 226, 62, 27, 41, 79, 29, 42, 111, 33, 45, 136, 37, 52, 161, 40, 62, 183, 43, 73, 212, 47, 89, 255, 51, 108, 171, 54, 128, 23, 57, 151, 42, 61, 176, 95, 65, 214, 106, 67, 236}
	pilLDown    = []uint8{38, 98, 190, 155, 108, 193, 149, 43, 197, 159, 43, 122}
	pilRGBADown = []uint8{22, 30, 0, 34, 119, 52, 33, 137, 194, 59, 76, 176, 72, 59, 182, 60, 45, 167, 17, 73, 148, 172, 51, 185, 191, 172, 82, 96, 36, 215, 215, 77, 84, 69, 40, 121, 169, 82, 62, 191, 149, 87, 112, 70, 64, 105, 208, 131}
	pilPDown    = []uint8{8, 14, 4, 10, 2, 8, 14, 4, 12, 2, 8, 14}
)

// Two frame 4x4 animated WebP written by Pillow, lossless and lossy with alpha
var (
	webpAnimLossless = mustB64("UklGRoQAAABXRUJQVlA4WAoAAAACAAAAAwAAAwAAQU5JTQYAAAAAAAAAAABBTk1GKAAAAAAAAAAAAAMAAAMAAGQAAAJWUDhMDwAAAC8DwAAABxD9j/4HIqL/AQBBTk1GKAAAAAAAAAAAAAMAAAMAAGQAAABWUDhMDwAAAC8DwAAABxDR//4HIqL/AQA=")
	webpAnimLossy    = mustB64("UklGRtwAAABXRUJQVlA4WAoAAAASAAAAAwAAAwAAQU5JTQYAAAAAAAAAAABBTk1GXAAAAAAAAAAAAAMAAAMAAGQAAAJBTFBICgAAAAEHUMCICERE/wNWUDggMgAAADABAJ0BKgQABAABQCYloAADcAD+8ut///mwP/bz/wR6Af//0uD//pcH//S4P/SkAAAAQU5NRkwAAAAAAAAAAAADAAADAABkAAAAVlA4IDQAAAA0AQCdASoEAAQAAAAmJaAAA3AA/ukiH//3nz//ufP/+58/6M///yn7//I4//8jj/5QIAAA")
)

// The deterministic 9x7 colour fixture shared with the Pillow reference run
func pilFixtureRGB() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 9, 7))
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8((x*37 + y*11) % 256), uint8((x*5 + y*53) % 256), uint8((x*x*3 + y*7) % 256), 255})
		}
	}
	return img
}

// RGB bytes of an RGBA buffer without the alpha channel
func rgbBytes(img *image.RGBA) []uint8 {
	var out []uint8
	for i := 0; i < len(img.Pix); i += 4 {
		out = append(out, img.Pix[i], img.Pix[i+1], img.Pix[i+2])
	}
	return out
}

func TestLanczosMatchesPillow(t *testing.T) {
	rgb := &raster{img: pilFixtureRGB(), mode: "RGB"}
	if got := rgbBytes(resizeRaster(rgb, 4, 3).img.(*image.RGBA)); !bytes.Equal(got, pilRGBDown) {
		t.Fatalf("RGB downscale\n got %v\nwant %v", got, pilRGBDown)
	}
	if got := rgbBytes(resizeRaster(rgb, 13, 5).img.(*image.RGBA)); !bytes.Equal(got, pilRGBUp) {
		t.Fatalf("RGB mixed scale\n got %v\nwant %v", got, pilRGBUp)
	}
	gray := image.NewGray(image.Rect(0, 0, 9, 7))
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			gray.SetGray(x, y, color.Gray{uint8((x*29 + y*31) % 256)})
		}
	}
	if got := resizeRaster(&raster{img: gray, mode: "L"}, 4, 3).img.(*image.Gray).Pix; !bytes.Equal(got, pilLDown) {
		t.Fatalf("L downscale\n got %v\nwant %v", got, pilLDown)
	}
	rgba := image.NewNRGBA(image.Rect(0, 0, 9, 7))
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			rgba.SetNRGBA(x, y, color.NRGBA{uint8((x*37 + y*11) % 256), uint8((x*5 + y*53) % 256), uint8((x*x*3 + y*7) % 256), uint8((x*41 + y*17) % 256)})
		}
	}
	if got := resizeRaster(&raster{img: rgba, mode: "RGBA"}, 4, 3).img.(*image.NRGBA).Pix; !bytes.Equal(got, pilRGBADown) {
		t.Fatalf("RGBA premultiplied downscale\n got %v\nwant %v", got, pilRGBADown)
	}
	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.RGBA{uint8(i), uint8(i), uint8(i), 255}
	}
	pal := image.NewPaletted(image.Rect(0, 0, 9, 7), palette)
	for y := 0; y < 7; y++ {
		for x := 0; x < 9; x++ {
			pal.SetColorIndex(x, y, uint8((x*3+y*5)%16))
		}
	}
	if got := resizeRaster(&raster{img: pal, mode: "P"}, 4, 3).img.(*image.Paletted).Pix; !bytes.Equal(got, pilPDown) {
		t.Fatalf("P nearest\n got %v\nwant %v", got, pilPDown)
	}
}

func TestUnsupportedInlineImageMediaType(t *testing.T) {
	cases := map[string]string{
		"data:image/png;base64,AAAA":     "",
		"data:image/jpg;base64,AAAA":     "",
		"data:image/webp;base64,AAAA":    "",
		"data:image/svg+xml;base64,AAAA": "image/svg+xml",
		"DATA:IMAGE/BMP;base64,AAAA":     "image/bmp",
		"data:image/;base64,AAAA":        "image/unknown",
		"https://example.com/a.bmp":      "",
	}
	for in, want := range cases {
		if got := UnsupportedInlineImageMediaType(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestDetermineMimeType(t *testing.T) {
	cases := map[string]string{"photo.jpg": "image/jpeg", "photo.jpeg": "image/jpeg", "screenshot.png": "image/png",
		"anim.gif": "image/gif", "modern.webp": "image/webp", "file.xyz": "image/jpeg", "PIC.PNG": "image/png"}
	for in, want := range cases {
		if got := determineMimeType(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestDetectImageMimeTypeFromBytes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"valid png", isrcPNG, "image/png"},
		{"truncated png", isrcPNG[:len(isrcPNG)-22], ""},
		{"corrupt png", isrcCorruptPNG, ""},
		{"jpeg magic", append([]byte{0xff, 0xd8, 0xff}, make([]byte, 16)...), "image/jpeg"},
		{"gif87", []byte("GIF87a........"), "image/gif"},
		{"gif89", []byte("GIF89a........"), "image/gif"},
		{"bmp", []byte("BM............"), "image/bmp"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"svg has no magic", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), ""},
		{"text", []byte("hello"), ""},
	}
	for _, c := range cases {
		if got := detectImageMimeTypeFromBytes(c.data); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	bomb := append([]byte{}, pngSignature...)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 20000)
	binary.BigEndian.PutUint32(ihdr[4:8], 20000)
	ihdr[8], ihdr[9] = 8, 2
	var buf bytes.Buffer
	buf.Write(bomb)
	writePNGChunk(&buf, "IHDR", ihdr)
	writePNGChunk(&buf, "IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})
	writePNGChunk(&buf, "IEND", nil)
	if got := detectImageMimeTypeFromBytes(buf.Bytes()); got != "" {
		t.Fatalf("decompression bomb sniffed as %q", got)
	}
	if msg := validateRasterBytes(buf.Bytes(), "bomb.png", 100, 100_000_000); !strings.Contains(msg, "decompression bomb") {
		t.Fatalf("bomb validation: %q", msg)
	}
}

// Animated GIF of 4x4 solid frames
func animatedGIF(t *testing.T, colors ...color.RGBA) []byte {
	t.Helper()
	anim := &gif.GIF{}
	for _, c := range colors {
		frame := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{c, color.RGBA{0, 0, 0, 255}})
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	red   = color.RGBA{255, 0, 0, 255}
	green = color.RGBA{0, 255, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
)

func TestValidateAnimatedGIF(t *testing.T) {
	two := animatedGIF(t, red, blue)
	if msg := validateRasterBytes(two, "ok.gif", 100, 100_000_000); msg != "" {
		t.Fatalf("valid animation rejected: %q", msg)
	}
	if msg := validateRasterBytes(two[:len(two)-3], "t.gif", 2, 32); !strings.Contains(msg, "decode") {
		t.Fatalf("truncated second frame: %q", msg)
	}
	three := animatedGIF(t, red, green, blue)
	corruptThird := append([]byte{}, three...)
	corruptThird = corruptThird[:len(corruptThird)-6]
	corruptThird = append(corruptThird, 0x00, ';')
	if msg := validateRasterBytes(corruptThird, "f.gif", 2, 100_000_000); msg != "Image validation rejected animation: frame 3 exceeds the maximum 2 validated frames." {
		t.Fatalf("frame limit must stop before decoding frame 3: %q", msg)
	}
	if msg := validateRasterBytes(two, "p.gif", 100, 31); msg != "Image validation rejected animation: aggregate decoded pixel count would reach 32 at frame 2, exceeding the maximum 31." {
		t.Fatalf("aggregate pixel limit: %q", msg)
	}
	r, err := openRaster(two)
	if err != nil || r.mode != "P" || r.img.Bounds().Dx() != 4 {
		t.Fatalf("first frame: %v %+v", err, r)
	}
}

// zlib stream of a 4x4 8 bit RGB image filled with one colour
func pngRowsRGB(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	var raw bytes.Buffer
	for y := 0; y < 4; y++ {
		raw.WriteByte(0)
		for x := 0; x < 4; x++ {
			raw.Write([]byte{c.R, c.G, c.B})
		}
	}
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(raw.Bytes())
	w.Close()
	return z.Bytes()
}

// Two frame 4x4 APNG, the second frame payload optionally corrupted with valid CRCs
func apngTwoFrames(t *testing.T, corruptSecond bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(pngSignature)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 4)
	binary.BigEndian.PutUint32(ihdr[4:8], 4)
	ihdr[8], ihdr[9] = 8, 2
	writePNGChunk(&buf, "IHDR", ihdr)
	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl[0:4], 2)
	writePNGChunk(&buf, "acTL", actl)
	fctl := func(seq uint32) []byte {
		b := make([]byte, 26)
		binary.BigEndian.PutUint32(b[0:4], seq)
		binary.BigEndian.PutUint32(b[4:8], 4)
		binary.BigEndian.PutUint32(b[8:12], 4)
		binary.BigEndian.PutUint16(b[20:22], 1)
		binary.BigEndian.PutUint16(b[22:24], 10)
		return b
	}
	writePNGChunk(&buf, "fcTL", fctl(0))
	writePNGChunk(&buf, "IDAT", pngRowsRGB(t, red))
	writePNGChunk(&buf, "fcTL", fctl(1))
	data := pngRowsRGB(t, blue)
	if corruptSecond {
		data = []byte{0x78, 0x9c, 0xff, 0xff, 0xff, 0xff}
	}
	fdat := binary.BigEndian.AppendUint32(nil, 2)
	writePNGChunk(&buf, "fdAT", append(fdat, data...))
	writePNGChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func TestValidateAPNG(t *testing.T) {
	good := apngTwoFrames(t, false)
	if got := detectImageMimeTypeFromBytes(good); got != "image/png" {
		t.Fatalf("apng sniff %q", got)
	}
	if msg := validateRasterBytes(good, "a.png", 100, 100_000_000); msg != "" {
		t.Fatalf("valid apng rejected: %q", msg)
	}
	if msg := validateRasterBytes(good, "a.png", 1, 100_000_000); !strings.Contains(msg, "frame 2 exceeds the maximum 1") {
		t.Fatalf("apng frame limit: %q", msg)
	}
	if msg := validateRasterBytes(apngTwoFrames(t, true), "b.png", 100, 100_000_000); !strings.HasPrefix(msg, "Image could not be fully decoded") {
		t.Fatalf("corrupt fdAT accepted: %q", msg)
	}
}

func TestValidateAnimatedWebP(t *testing.T) {
	for name, data := range map[string][]byte{"lossless": webpAnimLossless, "lossy alpha": webpAnimLossy} {
		if msg := validateRasterBytes(data, name, 100, 100_000_000); msg != "" {
			t.Fatalf("%s: valid animation rejected: %q", name, msg)
		}
		if msg := validateRasterBytes(data, name, 1, 100_000_000); !strings.Contains(msg, "frame 2 exceeds the maximum 1") {
			t.Fatalf("%s: frame limit: %q", name, msg)
		}
		if msg := validateRasterBytes(data, name, 100, 20); !strings.Contains(msg, "would reach 32 at frame 2") {
			t.Fatalf("%s: pixel limit: %q", name, msg)
		}
	}
	r, err := openRaster(webpAnimLossy)
	if err != nil || r.mode != "RGBA" || r.img.Bounds().Dx() != 4 {
		t.Fatalf("first animated frame: %v", err)
	}
	if c := color.NRGBAModel.Convert(r.img.At(1, 1)).(color.NRGBA); c.R < 200 || c.A > 200 {
		t.Fatalf("first frame should be translucent red, got %+v", c)
	}
}

// JPEG bytes of a solid colour
func solidJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Two picture MPO built from two JPEGs with an APP2 MPF index
func mpoTwoPictures(t *testing.T, second []byte) []byte {
	t.Helper()
	first := solidJPEG(t, 8, 8, red)
	mpf := func(offB, sizeA uint32) []byte {
		var b bytes.Buffer
		b.WriteString("MPF\x00")
		b.WriteString("II*\x00")
		binary.Write(&b, binary.LittleEndian, uint32(8))
		binary.Write(&b, binary.LittleEndian, uint16(2))
		binary.Write(&b, binary.LittleEndian, [6]uint16{0xB001, 4, 1, 0, 2, 0})
		binary.Write(&b, binary.LittleEndian, [2]uint16{0xB002, 7})
		binary.Write(&b, binary.LittleEndian, uint32(32))
		binary.Write(&b, binary.LittleEndian, uint32(8+2+24+4))
		binary.Write(&b, binary.LittleEndian, uint32(0))
		binary.Write(&b, binary.LittleEndian, [4]uint32{0x20030000, sizeA, 0, 0})
		binary.Write(&b, binary.LittleEndian, [4]uint32{0, uint32(len(second)), offB, 0})
		return b.Bytes()
	}
	payload := mpf(0, 0)
	segLen := len(payload) + 2
	offB := 2 + 2 + segLen + len(first) - 2
	payload = mpf(uint32(offB-10), uint32(offB))
	var out bytes.Buffer
	out.Write(first[:2])
	out.Write([]byte{0xFF, 0xE2, byte(segLen >> 8), byte(segLen)})
	out.Write(payload)
	out.Write(first[2:])
	out.Write(second)
	return out.Bytes()
}

func TestValidateMPO(t *testing.T) {
	good := mpoTwoPictures(t, solidJPEG(t, 6, 5, blue))
	offs, ok := mpoOffsets(good)
	if !ok || len(offs) != 2 || offs[0] != 0 {
		t.Fatalf("mp index %v %v", offs, ok)
	}
	if msg := validateRasterBytes(good, "m.jpg", 100, 100_000_000); msg != "" {
		t.Fatalf("valid MPO rejected: %q", msg)
	}
	if msg := validateRasterBytes(good, "m.jpg", 1, 100_000_000); !strings.Contains(msg, "frame 2 exceeds the maximum 1") {
		t.Fatalf("MPO frame limit: %q", msg)
	}
	if msg := validateRasterBytes(good, "m.jpg", 100, 64+29); !strings.Contains(msg, "would reach 94 at frame 2") {
		t.Fatalf("MPO frames sized by their own headers: %q", msg)
	}
	broken := solidJPEG(t, 6, 5, blue)
	if msg := validateRasterBytes(mpoTwoPictures(t, broken[:len(broken)/2]), "b.jpg", 100, 100_000_000); !strings.HasPrefix(msg, "Image could not be fully decoded") {
		t.Fatalf("truncated second picture accepted: %q", msg)
	}
}

// PNG file of a solid colour at a size
func writeSolidPNG(t *testing.T, path string, w, h int, c color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

// Size of a decoded image file
func fileImageSize(t *testing.T, path string) (int, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w, h, _, err := rasterConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	return w, h
}

func TestCropImageRegion(t *testing.T) {
	dir := t.TempDir()
	src := writeSolidPNG(t, filepath.Join(dir, "src.png"), 100, 50, color.RGBA{200, 30, 30, 255})
	offset := map[string]int{}
	out, mime, msg := cropImageRegion(src, []any{10.0, 10.0, 60.0, 40.0}, offset)
	if msg != "" || mime != "image/png" {
		t.Fatalf("crop: %q", msg)
	}
	if w, h := fileImageSize(t, out); w != 50 || h != 30 {
		t.Fatalf("crop size %dx%d", w, h)
	}
	if offset["x"] != 10 || offset["y"] != 10 || offset["width"] != 50 || offset["height"] != 30 {
		t.Fatalf("offset %v", offset)
	}
	if !strings.HasPrefix(filepath.Base(out), "src_region_") {
		t.Fatalf("crop name %q", out)
	}
	out, _, msg = cropImageRegion(src, []any{-10.0, -10.0, 200.0, 200.0}, nil)
	if w, h := fileImageSize(t, out); msg != "" || w != 100 || h != 50 {
		t.Fatalf("clamp %dx%d %q", w, h, msg)
	}
	for _, region := range [][]any{{200.0, 200.0, 300.0, 300.0}, {60.0, 40.0, 10.0, 10.0}} {
		out, _, msg = cropImageRegion(src, region, nil)
		if out != "" || !strings.Contains(msg, "100") || !strings.Contains(msg, "50") {
			t.Fatalf("zero area %v: %q", region, msg)
		}
	}
	for _, bad := range []any{[]any{1.0, 2.0, 3.0}, "10,10,60,40", []any{1.0, 2.0, 3.0, "x"}, nil, []any{true, 0.0, 5.0, 5.0}} {
		out, _, msg = cropImageRegion(src, bad, nil)
		if out != "" || !strings.HasPrefix(msg, "Invalid region") {
			t.Fatalf("malformed %v: %q", bad, msg)
		}
	}
}

func TestImageExceedsDimension(t *testing.T) {
	dir := t.TempDir()
	tall := writeSolidPNG(t, filepath.Join(dir, "tall.png"), 1200, 12000, color.RGBA{40, 40, 40, 255})
	if !imageExceedsDimension(tall, VisionEmbedMaxDimension) {
		t.Fatal("tall image not flagged")
	}
	small := writeSolidPNG(t, filepath.Join(dir, "small.png"), 100, 100, color.RGBA{40, 40, 40, 255})
	if imageExceedsDimension(small, VisionEmbedMaxDimension) {
		t.Fatal("small image flagged")
	}
	corrupt := filepath.Join(dir, "corrupt.png")
	os.WriteFile(corrupt, []byte("not an image at all"), 0o644)
	if imageExceedsDimension(corrupt, VisionEmbedMaxDimension) {
		t.Fatal("unreadable image flagged")
	}
}

func TestNormalizeToSupportedImage(t *testing.T) {
	h := t.TempDir()
	env := &Env{Home: h, Config: config.Defaults(), Values: &sync.Map{}}
	dir := t.TempDir()
	pngPath := writeSolidPNG(t, filepath.Join(dir, "ok.png"), 4, 4, red)
	if out, mime, msg := normalizeToSupportedImage(env, pngPath, "image/png"); out != pngPath || mime != "image/png" || msg != "" {
		t.Fatalf("supported image changed: %q %q %q", out, mime, msg)
	}
	img := image.NewPaletted(image.Rect(0, 0, 5, 3), color.Palette{red, blue})
	var bmpBuf bytes.Buffer
	if err := bmp.Encode(&bmpBuf, img); err != nil {
		t.Fatal(err)
	}
	bmpPath := filepath.Join(dir, "p.bmp")
	os.WriteFile(bmpPath, bmpBuf.Bytes(), 0o644)
	out, mime, msg := normalizeToSupportedImage(env, bmpPath, "image/bmp")
	if msg != "" || mime != "image/png" || filepath.Dir(out) != filepath.Join(h, "cache", "vision") {
		t.Fatalf("bmp conversion: %q %q %q", out, mime, msg)
	}
	if w, hh := fileImageSize(t, out); w != 5 || hh != 3 {
		t.Fatalf("bmp converted size %dx%d", w, hh)
	}
	if st, err := os.Stat(filepath.Dir(out)); err != nil || (os.Getenv("ICEBATH_HOME_MODE") == "" && !containerOrChmodSkipped() && st.Mode().Perm() != 0o700) {
		t.Fatalf("cache dir mode %v %v", st.Mode(), err)
	}
	webpPath := filepath.Join(dir, "a.webp")
	os.WriteFile(webpPath, webpAnimLossless, 0o644)
	if out, _, _ := normalizeToSupportedImage(env, webpPath, "image/webp"); out != webpPath {
		t.Fatal("webp converted for a cloud provider")
	}
	env.Values.Store("runtime_main", map[string]any{"provider": "llamacpp", "model": "local"})
	out, mime, msg = normalizeToSupportedImage(env, webpPath, "image/webp")
	if msg != "" || mime != "image/png" || out == webpPath {
		t.Fatalf("managed runtime must transcode webp: %q %q %q", out, mime, msg)
	}
	if _, _, msg := normalizeToSupportedImage(env, filepath.Join(dir, "missing.tiff"), "image/tiff"); !strings.Contains(msg, "'image/tiff' is not supported") {
		t.Fatalf("unsupported format message %q", msg)
	}
}

func TestRasterizeSVGDataURL(t *testing.T) {
	env := &Env{Home: t.TempDir(), Config: config.Defaults(), Values: &sync.Map{}}
	prev := rasterizeSVGToPNG
	t.Cleanup(func() { rasterizeSVGToPNG = prev })
	var seen []string
	rasterizeSVGToPNG = func(svgPath, outPath string) bool {
		b, _ := os.ReadFile(svgPath)
		seen = append(seen, string(b))
		return os.WriteFile(outPath, isrcPNG, 0o644) == nil
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"/>`
	got, ok := RasterizeSVGDataURL(env, "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(svg)))
	if !ok || got != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(isrcPNG) {
		t.Fatalf("base64 svg: %q %v", got, ok)
	}
	if _, ok := RasterizeSVGDataURL(env, "data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%2F%3E"); !ok {
		t.Fatal("percent encoded svg")
	}
	if len(seen) != 2 || seen[0] != svg || seen[1] != `<svg xmlns="http://www.w3.org/2000/svg"/>` {
		t.Fatalf("svg payloads %q", seen)
	}
	entries, _ := os.ReadDir(filepath.Join(env.Home, "cache", "vision"))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %d", len(entries))
	}
	rasterizeSVGToPNG = func(string, string) bool { return false }
	if _, ok := RasterizeSVGDataURL(env, "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(svg))); ok {
		t.Fatal("no rasterizer must report false")
	}
}
