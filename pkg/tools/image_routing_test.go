package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"

	"github.com/nickheyer/icebath/pkg/config"
)

var routingPNG = mustB64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABpfZFQAAAAABJRU5ErkJggg==")

func imageRoutingEnv(t *testing.T) *Env {
	return &Env{Home: t.TempDir(), Config: config.Defaults(), Values: &sync.Map{}}
}

func writeRoutingFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func partText(p any) string { s, _ := p.(map[string]any)["text"].(string); return s }

func partURL(p any) string {
	u, _ := p.(map[string]any)["image_url"].(map[string]any)["url"].(string)
	return u
}

func routingParts(t *testing.T, env *Env, text string, paths, urls []string) ([]any, []string) {
	t.Helper()
	parts, skipped, err := BuildNativeContentParts(env, text, paths, urls)
	if err != nil {
		t.Fatal(err)
	}
	return parts, skipped
}

func TestCoerceImageMode(t *testing.T) {
	for raw, want := range map[any]string{"NATIVE": "native", "Auto": "auto", " text\x1f": "text", "nonsense": "auto", "": "auto", nil: "auto", 42: "auto"} {
		if got := coerceImageMode(raw); got != want {
			t.Errorf("coerceImageMode(%#v) = %q want %q", raw, got, want)
		}
	}
}

func TestBuildNativeContentPartsLocal(t *testing.T) {
	env, dir := imageRoutingEnv(t), t.TempDir()
	img := writeRoutingFile(t, filepath.Join(dir, "cat.png"), routingPNG)
	parts, skipped := routingParts(t, env, "hello", []string{img}, nil)
	if len(skipped) != 0 || len(parts) != 2 {
		t.Fatalf("parts %v skipped %v", parts, skipped)
	}
	if tp := parts[0].(map[string]any); tp["type"] != "text" || tp["text"] != "hello\n\n[Image attached at: "+img+"]" {
		t.Fatalf("text part %+v", tp)
	}
	if ip := partURL(parts[1]); parts[1].(map[string]any)["type"] != "image_url" || !strings.HasPrefix(ip, "data:image/png;base64,") {
		t.Fatalf("image part %+v", ip)
	}
	a := writeRoutingFile(t, filepath.Join(dir, "a.png"), routingPNG)
	b := writeRoutingFile(t, filepath.Join(dir, "b.png"), routingPNG)
	parts, skipped = routingParts(t, env, "compare these", []string{a, b}, nil)
	text := partText(parts[0])
	if len(parts) != 3 || len(skipped) != 0 || strings.Count(text, "[Image attached at:") != 2 || !strings.Contains(text, a) || !strings.Contains(text, b) || !strings.HasPrefix(text, "compare these") {
		t.Fatalf("multiple images %v", parts)
	}
	parts, skipped = routingParts(t, env, "  ", []string{img, filepath.Join(dir, "gone.png"), dir}, nil)
	if partText(parts[0]) != "What do you see in this image?\n\n[Image attached at: "+img+"]" || !reflect.DeepEqual(skipped, []string{filepath.Join(dir, "gone.png"), dir}) {
		t.Fatalf("default caption %v skipped %v", parts, skipped)
	}
	if parts, _ = routingParts(t, env, " only text ", nil, []string{"  ", ""}); !reflect.DeepEqual(parts, []any{map[string]any{"type": "text", "text": "only text"}}) {
		t.Fatalf("text only %v", parts)
	}
	if parts, _ = routingParts(t, env, "", nil, nil); len(parts) != 0 {
		t.Fatalf("empty turn %v", parts)
	}
}

func TestBuildNativeContentPartsURLs(t *testing.T) {
	env := imageRoutingEnv(t)
	parts, skipped := routingParts(t, env, "what is this?", nil, []string{"https://example.com/diagram.png"})
	if len(skipped) != 0 || len(parts) != 2 {
		t.Fatalf("parts %v", parts)
	}
	text := partText(parts[0])
	if !strings.HasPrefix(text, "what is this?") || !strings.Contains(text, "[Image attached: https://example.com/diagram.png]") {
		t.Fatalf("text %q", text)
	}
	if got := parts[1]; !reflect.DeepEqual(got, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/diagram.png"}}) {
		t.Fatalf("url part %s", got)
	}
	img := writeRoutingFile(t, filepath.Join(t.TempDir(), "local.png"), routingPNG)
	parts, skipped = routingParts(t, env, "compare these", []string{img}, []string{" https://example.com/remote.jpg "})
	if len(skipped) != 0 || len(parts) != 3 || !strings.HasPrefix(partURL(parts[1]), "data:image/png;base64,") || partURL(parts[2]) != "https://example.com/remote.jpg" {
		t.Fatalf("mixed %v", parts)
	}
	if text = partText(parts[0]); !strings.Contains(text, "[Image attached at:") || !strings.Contains(text, "[Image attached: https://example.com/remote.jpg]") {
		t.Fatalf("mixed text %q", text)
	}
}

func TestFileToDataURLSizesAndMissing(t *testing.T) {
	env, dir := imageRoutingEnv(t), t.TempDir()
	big := writeRoutingFile(t, filepath.Join(dir, "medium.png"), append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("X"), 200_000)...))
	u, ok, err := fileToDataURL(env, big)
	if err != nil || !ok || !strings.HasPrefix(u, "data:image/png;base64,") || len(u) <= 200_000 {
		t.Fatalf("large image %v %v len %d", ok, err, len(u))
	}
	if _, ok, _ := fileToDataURL(env, filepath.Join(dir, "does_not_exist.png")); ok {
		t.Fatal("missing file encoded")
	}
	u, ok, _ = fileToDataURL(env, writeRoutingFile(t, filepath.Join(dir, "ok.png"), routingPNG))
	if !ok || u != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(routingPNG) {
		t.Fatalf("png passthrough %q", u)
	}
	if _, ok, _ := fileToDataURL(env, writeRoutingFile(t, filepath.Join(dir, "junk.svg"), []byte("not an image"))); ok {
		t.Fatal("undecodable svg suffix attached")
	}
	if u, ok, _ := fileToDataURL(env, writeRoutingFile(t, filepath.Join(dir, "junk.weird"), []byte("not an image"))); !ok || !strings.HasPrefix(u, "data:image/jpeg;base64,") {
		t.Fatalf("unknown suffix falls back to jpeg %q", u)
	}
}

func TestImageRoutingReadGuard(t *testing.T) {
	env, dir := imageRoutingEnv(t), t.TempDir()
	secret := writeRoutingFile(t, filepath.Join(dir, ".env"), routingPNG)
	if _, ok, _ := fileToDataURL(env, secret); ok {
		t.Fatal(".env image attached")
	}
	link := filepath.Join(dir, "secret.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink %v", err)
	}
	parts, skipped := routingParts(t, env, "inspect this", []string{link}, nil)
	if !reflect.DeepEqual(skipped, []string{link}) {
		t.Fatalf("skipped %v", skipped)
	}
	for _, p := range parts {
		if p.(map[string]any)["type"] == "image_url" {
			t.Fatal("blocked symlink attached")
		}
	}
}

func TestExtractImageRefs(t *testing.T) {
	if p, u := ExtractImageRefs(""); len(p) != 0 || len(u) != 0 {
		t.Fatal("empty text")
	}
	dir := t.TempDir()
	img := writeRoutingFile(t, filepath.Join(dir, "screenshot.png"), routingPNG)
	p, u := ExtractImageRefs("Look at " + img + " and tell me what's wrong.")
	if !reflect.DeepEqual(p, []string{img}) || len(u) != 0 {
		t.Fatalf("absolute %v %v", p, u)
	}
	t.Setenv("HOME", dir)
	foo := writeRoutingFile(t, filepath.Join(dir, "foo.png"), routingPNG)
	if p, u = ExtractImageRefs("see ~/foo.png please"); !reflect.DeepEqual(p, []string{foo}) || len(u) != 0 {
		t.Fatalf("home relative %v %v", p, u)
	}
	if p, u = ExtractImageRefs("Check out https://example.com/photos/cat.png — cute right?"); len(p) != 0 || !reflect.DeepEqual(u, []string{"https://example.com/photos/cat.png"}) {
		t.Fatalf("url %v %v", p, u)
	}
	text := "see https://x.io/a.PNG. and (https://x.io/b.jpg?s=1) twice https://x.io/a.PNG " + img + " " + img +
		" `" + foo + "` ```sh\n" + foo + " https://x.io/c.png\n``` http://x.io/doc.pdf missing /nope/x.png"
	p, u = ExtractImageRefs(text)
	if !reflect.DeepEqual(p, []string{img}) || !reflect.DeepEqual(u, []string{"https://x.io/a.PNG", "https://x.io/b.jpg?s=1"}) {
		t.Fatalf("code spans and dedupe %v %v", p, u)
	}
	if p, _ = ExtractImageRefs("url:" + img); len(p) != 0 {
		t.Fatalf("lookbehind ignored %v", p)
	}
}

func TestSniffImageRoutingMime(t *testing.T) {
	cases := map[string]string{
		`<svg xmlns="http://www.w3.org/2000/svg"/>`: "image/svg+xml",
		`<?xml version="1.0"?><svg/>`:               "image/svg+xml",
		"  \n<SVG/>":                                "image/svg+xml",
		`<?xml version="1.0"?><html/>`:              "",
		"\xef\xbb\xbf<svg/>":                        "",
		"\x89PNG\r\n\x1a\nrest":                     "image/png",
		"\xff\xd8\xffxx":                            "image/jpeg",
		"GIF89a":                                    "image/gif",
		"RIFF\x00\x00\x00\x00WEBPVP8 ":              "image/webp",
		"BMxx":                                      "image/bmp",
		"\x00\x00\x00\x1cftypavif":                  "image/avif",
		"\x00\x00\x00\x1cftypmif1":                  "image/heic",
		"\x00\x00\x00\x1cftypav01":                  "",
		"II*\x00":                                   "image/tiff",
		"MM\x00*":                                   "image/tiff",
		"\x00\x00\x01\x00\x01\x00":                  "image/x-icon",
		"":                                          "",
	}
	for raw, want := range cases {
		if got := sniffImageRoutingMime([]byte(raw)); got != want {
			t.Errorf("sniff(%q) = %q want %q", raw, got, want)
		}
	}
}

// Exported sniffer agrees with the routing sniffer on png, svg and unknown bytes
func TestSniffImageMimeExport(t *testing.T) {
	for _, raw := range []string{"\x89PNG\r\n\x1a\nrest", "<svg xmlns='x'></svg>", "plain"} {
		if got, want := SniffImageMime([]byte(raw)), sniffImageRoutingMime([]byte(raw)); got != want {
			t.Errorf("SniffImageMime(%q) = %q want %q", raw, got, want)
		}
	}
	if SniffImageMime([]byte("\x89PNG\r\n\x1a\n")) != "image/png" {
		t.Fatal("png magic not recognised")
	}
}

func decodeDataURLImage(t *testing.T, u string) image.Image {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestFormatsTranscodedToPNG(t *testing.T) {
	env, dir := imageRoutingEnv(t), t.TempDir()
	red := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(red.Pix); i += 4 {
		copy(red.Pix[i:], []byte{255, 0, 0, 255})
	}
	var bmpBuf, tiffBuf bytes.Buffer
	bmp.Encode(&bmpBuf, red)
	tiff.Encode(&tiffBuf, red, nil)
	for name, data := range map[string][]byte{"scan.bmp": bmpBuf.Bytes(), "scan.tiff": tiffBuf.Bytes()} {
		u, ok, err := fileToDataURL(env, writeRoutingFile(t, filepath.Join(dir, name), data))
		if err != nil || !ok || !strings.HasPrefix(u, "data:image/png;base64,") {
			t.Fatalf("%s not transcoded %q", name, u[:min(len(u), 60)])
		}
		if r, g, b, _ := decodeDataURLImage(t, u).At(1, 1).RGBA(); r>>8 != 255 || g != 0 || b != 0 {
			t.Fatalf("%s pixel %d %d %d", name, r, g, b)
		}
	}
	webp := writeRoutingFile(t, filepath.Join(dir, "a.webp"), webpAnimLossless)
	if u, _, _ := fileToDataURL(env, webp); !strings.HasPrefix(u, "data:image/webp;base64,") {
		t.Fatalf("webp accepted by cloud providers %q", u[:30])
	}
	env.Values.Store("runtime_main", map[string]any{"provider": "llamacpp", "model": "local"})
	if u, _, _ := fileToDataURL(env, webp); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatalf("managed runtime must transcode webp %q", u[:30])
	}
}

// ICO with a 2x2 32 bit entry, a larger 4x2 8 bit masked entry, and the larger area chosen
func buildTestICO(t *testing.T) []byte {
	t.Helper()
	dib := func(w, h, bpp int, palette [][4]byte, pixels []byte, mask []byte) []byte {
		var b bytes.Buffer
		hdr := make([]byte, 40)
		binary.LittleEndian.PutUint32(hdr, 40)
		binary.LittleEndian.PutUint32(hdr[4:], uint32(w))
		binary.LittleEndian.PutUint32(hdr[8:], uint32(2*h))
		binary.LittleEndian.PutUint16(hdr[12:], 1)
		binary.LittleEndian.PutUint16(hdr[14:], uint16(bpp))
		binary.LittleEndian.PutUint32(hdr[32:], uint32(len(palette)))
		b.Write(hdr)
		for _, p := range palette {
			b.Write(p[:])
		}
		b.Write(pixels)
		b.Write(mask)
		return b.Bytes()
	}
	small := dib(2, 2, 32, nil, bytes.Repeat([]byte{0, 255, 0, 255}, 4), nil)
	// 8 bit rows are padded to 4 bytes, bottom row first, palette index 1 is blue
	pixels := []byte{1, 1, 1, 1, 0, 0, 0, 0}
	mask := []byte{0b1000_0000, 0, 0, 0, 0, 0, 0, 0}
	large := dib(4, 2, 8, [][4]byte{{0, 0, 255, 0}, {255, 0, 0, 0}}, pixels, mask)
	var out bytes.Buffer
	out.Write([]byte{0, 0, 1, 0, 2, 0})
	entry := func(w, h, bpp, size, off int) {
		e := make([]byte, 16)
		e[0], e[1] = byte(w), byte(h)
		binary.LittleEndian.PutUint16(e[4:], 1)
		binary.LittleEndian.PutUint16(e[6:], uint16(bpp))
		binary.LittleEndian.PutUint32(e[8:], uint32(size))
		binary.LittleEndian.PutUint32(e[12:], uint32(off))
		out.Write(e)
	}
	entry(2, 2, 32, len(small), 38)
	entry(4, 2, 8, len(large), 38+len(small))
	out.Write(small)
	out.Write(large)
	return out.Bytes()
}

func TestDecodeICO(t *testing.T) {
	r, err := decodeICO(buildTestICO(t))
	if err != nil {
		t.Fatal(err)
	}
	if b := r.img.Bounds(); b.Dx() != 4 || b.Dy() != 2 || r.mode != "RGBA" {
		t.Fatalf("chosen entry %v %s", b, r.mode)
	}
	img := r.img.(*image.NRGBA)
	if got := img.NRGBAAt(0, 0); got != (color.NRGBA{255, 0, 0, 255}) {
		t.Fatalf("top row palette colour %v", got)
	}
	if got := img.NRGBAAt(0, 1); got != (color.NRGBA{0, 0, 255, 0}) {
		t.Fatalf("masked bottom left %v", got)
	}
	if got := img.NRGBAAt(1, 1); got != (color.NRGBA{0, 0, 255, 255}) {
		t.Fatalf("unmasked bottom pixel %v", got)
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, image.NewGray(image.Rect(0, 0, 3, 3)))
	ico := append([]byte{0, 0, 1, 0, 1, 0, 3, 3, 0, 0, 1, 0, 32, 0}, make([]byte, 8)...)
	binary.LittleEndian.PutUint32(ico[14:], uint32(pngBuf.Len()))
	binary.LittleEndian.PutUint32(ico[18:], 22)
	r, err = decodeICO(append(ico, pngBuf.Bytes()...))
	if err != nil || r.img.Bounds().Dx() != 3 {
		t.Fatalf("png entry %v %v", r, err)
	}
	if _, err := decodeICO([]byte{0, 0, 1, 0, 0, 0}); err == nil {
		t.Fatal("empty directory decoded")
	}
	u, ok, _ := fileToDataURL(imageRoutingEnv(t), writeRoutingFile(t, filepath.Join(t.TempDir(), "fav.ico"), buildTestICO(t)))
	if !ok || decodeDataURLImage(t, u).Bounds().Dx() != 4 {
		t.Fatal("ico not transcoded")
	}
}
