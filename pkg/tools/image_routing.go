package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/image/tiff"

	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Image extensions auto attached from free text, documents and archives excluded
const imageRefExtPattern = "png|jpg|jpeg|gif|webp|bmp|tiff|tif|heic"

var (
	localImagePathRe = mustPyre(`(?i)(?<![/:\w.])(?:~/|/|[A-Za-z]:[\\/])(?:[\w.\-]+[\\/])*[\w.\-]+\.(?:` + imageRefExtPattern + `)\b`)
	imageURLRe       = mustPyre(`(?i)https?://[^\s<>"']+?\.(?:` + imageRefExtPattern + `)(?:\?[^\s<>"']*)?`)
	codeSpanRes      = []*regexp2.Regexp{mustPyre("(?s)```[^\\n]*\\n.*?```"), mustPyre("`[^`\\n]+`")}
)

func mustPyre(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Every match of a regexp2 pattern as byte start, byte end and text
func pyFinditer(re *regexp2.Regexp, text string) [][3]any {
	var out [][3]any
	m, _ := re.FindStringMatch(text)
	for m != nil {
		idx, n := m.ByteRange()
		out = append(out, [3]any{idx, idx + n, m.String()})
		m, _ = re.FindNextMatch(m)
	}
	return out
}

// Match texts whose start lies outside fenced blocks and inline backtick spans
func matchesOutsideCode(re *regexp2.Regexp, text string) []string {
	var spans [][3]any
	for _, p := range codeSpanRes {
		spans = append(spans, pyFinditer(p, text)...)
	}
	var out []string
	for _, m := range pyFinditer(re, text) {
		start := m[0].(int)
		inside := false
		for _, s := range spans {
			if s[0].(int) <= start && start < s[1].(int) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, m[2].(string))
		}
	}
	return out
}

// Normalized path of a regular file after tilde expansion, false when it is not one
func existingFile(candidate string) (string, bool) {
	expanded := pypath.Expanduser(candidate)
	if st, err := os.Stat(expanded); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return pypath.Normpath(expanded), true
}

// ExtractImageRefs returns ordered unique existing local image paths and image URLs found outside code spans
func ExtractImageRefs(text string) ([]string, []string) {
	paths, urls := []string{}, []string{}
	if text == "" {
		return paths, urls
	}
	seen := map[string]bool{}
	for _, m := range matchesOutsideCode(localImagePathRe, text) {
		if p, ok := existingFile(m); ok && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	seen = map[string]bool{}
	for _, m := range matchesOutsideCode(imageURLRe, text) {
		u := pystr.RstripChars(m, ".,;:!?)]>")
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return paths, urls
}

// Magic byte conditions in priority order, each offset and signature must match
var imageRoutingMagic = func() []struct {
	conds [][2]any
	mime  string
} {
	type entry = struct {
		conds [][2]any
		mime  string
	}
	one := func(sig, mime string) entry { return entry{[][2]any{{0, sig}}, mime} }
	out := []entry{
		one("\x89PNG\r\n\x1a\n", "image/png"),
		one("\xff\xd8\xff", "image/jpeg"),
		one("GIF87a", "image/gif"), one("GIF89a", "image/gif"),
		{[][2]any{{0, "RIFF"}, {8, "WEBP"}}, "image/webp"},
		one("BM", "image/bmp"),
	}
	for _, b := range []string{"avif", "avis"} {
		out = append(out, entry{[][2]any{{4, "ftyp"}, {8, b}}, "image/avif"})
	}
	for _, b := range []string{"heic", "heix", "hevc", "hevx", "mif1", "msf1", "heim", "heis"} {
		out = append(out, entry{[][2]any{{4, "ftyp"}, {8, b}}, "image/heic"})
	}
	return append(out, one("II*\x00", "image/tiff"), one("MM\x00*", "image/tiff"), one("\x00\x00\x01\x00", "image/x-icon"))
}()

// Image MIME from magic bytes or an svg tag near the start, empty when unrecognised
func sniffImageRoutingMime(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	for _, m := range imageRoutingMagic {
		ok := true
		for _, c := range m.conds {
			off, sig := c[0].(int), c[1].(string)
			if off > len(raw) || !bytes.Equal(raw[off:min(len(raw), off+len(sig))], []byte(sig)) {
				ok = false
				break
			}
		}
		if ok {
			return m.mime
		}
	}
	head := asciiLowerBytes(bytes.TrimLeft(raw[:min(len(raw), 512)], " \t\n\r\v\f"))
	if (bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<svg"))) && bytes.Contains(head, []byte("<svg")) {
		return "image/svg+xml"
	}
	return ""
}

// Image MIME from magic bytes or an svg tag, empty when unrecognised
func SniffImageMime(raw []byte) string { return sniffImageRoutingMime(raw) }

func asciiLowerBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// PNG bytes of the first frame with exotic modes normalised to RGBA, error when undecodable
func transcodeToPNG(raw []byte) ([]byte, error) {
	r, err := openTranscodeRaster(raw)
	if err != nil {
		return nil, err
	}
	switch r.mode {
	case "RGB", "RGBA", "L", "LA", "P":
	default:
		r = convertRaster(r, "RGBA")
	}
	return encodePNG(r.img)
}

// Decodes TIFF and ICO beside every format the vision raster decoder opens
func openTranscodeRaster(raw []byte) (*raster, error) {
	switch {
	case bytes.HasPrefix(raw, []byte("II*\x00")), bytes.HasPrefix(raw, []byte("MM\x00*")):
		cfg, err := tiff.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if err := pilBombCheck(cfg.Width, cfg.Height); err != nil {
			return nil, err
		}
		img, err := tiff.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		return &raster{img: img, mode: modeOfImage(img)}, nil
	case bytes.HasPrefix(raw, []byte("\x00\x00\x01\x00")):
		return decodeICO(raw)
	}
	return openRaster(raw)
}

// Largest ICO entry, lowest colour depth among equal areas, as a PNG frame or a masked DIB
func decodeICO(raw []byte) (*raster, error) {
	if len(raw) < 6 {
		return nil, errors.New("not an ICO file")
	}
	type icoEntry struct{ w, h, depth, bpp, size, offset int }
	n := int(binary.LittleEndian.Uint16(raw[4:]))
	var entries []icoEntry
	for i := range n {
		s := raw[min(len(raw), 6+16*i):min(len(raw), 6+16*(i+1))]
		if len(s) < 16 {
			return nil, errors.New("truncated ICO directory")
		}
		e := icoEntry{w: int(s[0]), h: int(s[1]), bpp: int(binary.LittleEndian.Uint16(s[6:])),
			size: int(binary.LittleEndian.Uint32(s[8:])), offset: int(binary.LittleEndian.Uint32(s[12:]))}
		if e.w == 0 {
			e.w = 256
		}
		if e.h == 0 {
			e.h = 256
		}
		switch nb := int(s[2]); {
		case e.bpp != 0:
			e.depth = e.bpp
		case nb > 1:
			e.depth = ceilLog2(nb)
		default:
			e.depth = 256
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, errors.New("ICO file has no images")
	}
	best := entries[0]
	for _, e := range entries[1:] {
		if e.w*e.h > best.w*best.h || (e.w*e.h == best.w*best.h && e.depth < best.depth) {
			best = e
		}
	}
	if best.offset >= len(raw) {
		return nil, errors.New("ICO image offset out of range")
	}
	data := raw[best.offset:]
	if bytes.HasPrefix(data, pngSignature) {
		return openRaster(data)
	}
	return decodeICODIB(data, raw, best.offset+best.size, best.bpp)
}

// Bits needed for a colour count the way ceil(log(n, 2)) rounds
func ceilLog2(n int) int {
	bits := 0
	for v := 1; v < n; v <<= 1 {
		bits++
	}
	return bits
}

// Bottom up DIB of doubled height with its alpha taken from 32 bit pixels or the trailing AND mask
func decodeICODIB(data, file []byte, entryEnd, entryBpp int) (*raster, error) {
	if len(data) < 40 {
		return nil, errors.New("truncated DIB header")
	}
	hdr := int(binary.LittleEndian.Uint32(data))
	w := int(int32(binary.LittleEndian.Uint32(data[4:])))
	h := int(int32(binary.LittleEndian.Uint32(data[8:]))) / 2
	bpp := int(binary.LittleEndian.Uint16(data[14:]))
	if comp := binary.LittleEndian.Uint32(data[16:]); comp != 0 {
		return nil, fmt.Errorf("Unsupported BMP compression (%d)", comp)
	}
	if w <= 0 || h <= 0 {
		return nil, errors.New("Unsupported BMP header type")
	}
	if err := pilBombCheck(w, h); err != nil {
		return nil, err
	}
	var palette []color.NRGBA
	if bpp <= 8 {
		count := int(binary.LittleEndian.Uint32(data[32:]))
		if count == 0 {
			count = 1 << bpp
		}
		for i := range count {
			o := hdr + 4*i
			if o+4 > len(data) {
				return nil, errors.New("truncated DIB palette")
			}
			palette = append(palette, color.NRGBA{data[o+2], data[o+1], data[o], 255})
		}
	}
	pix := hdr + 4*len(palette)
	stride := (w*bpp + 31) / 32 * 4
	if pix+stride*h > len(data) {
		return nil, errors.New("image file is truncated")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		row := data[pix+(h-1-y)*stride:]
		for x := range w {
			var c color.NRGBA
			switch bpp {
			case 1, 4, 8:
				bit := x * bpp
				idx := int(row[bit/8]>>(8-bpp-bit%8)) & (1<<bpp - 1)
				if idx < len(palette) {
					c = palette[idx]
				} else {
					c = color.NRGBA{A: 255}
				}
			case 16:
				v := binary.LittleEndian.Uint16(row[2*x:])
				c = color.NRGBA{uint8((v >> 10 & 31) * 255 / 31), uint8((v >> 5 & 31) * 255 / 31), uint8((v & 31) * 255 / 31), 255}
			case 24:
				c = color.NRGBA{row[3*x+2], row[3*x+1], row[3*x], 255}
			case 32:
				c = color.NRGBA{row[4*x+2], row[4*x+1], row[4*x], 255}
			default:
				return nil, fmt.Errorf("Unsupported BMP pixel depth (%d)", bpp)
			}
			img.SetNRGBA(x, y, c)
		}
	}
	if entryBpp == 32 {
		if pix+w*h*4 > len(data) {
			return nil, errors.New("not enough image data")
		}
		for y := range h {
			for x := range w {
				img.Pix[img.PixOffset(x, y)+3] = data[pix+((h-1-y)*w+x)*4+3]
			}
		}
	} else {
		maskStride := (w + 31) / 32 * 4
		start := entryEnd - maskStride*h
		if start < 0 || entryEnd > len(file) {
			return nil, errors.New("not enough image data")
		}
		mask := file[start:entryEnd]
		for y := range h {
			row := mask[(h-1-y)*maskStride:]
			for x := range w {
				a := uint8(255)
				if row[x/8]>>(7-x%8)&1 == 1 {
					a = 0
				}
				img.Pix[img.PixOffset(x, y)+3] = a
			}
		}
	}
	return &raster{img: img, mode: "RGBA"}, nil
}

// Suffix MIME fallback when mimetypes mis-maps or knows nothing
var imageRoutingSuffixMimes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
}

// Image MIME from magic bytes, then mimetypes, then the suffix table, then JPEG
func guessImageMime(path string, raw []byte, haveRaw bool) (string, error) {
	mime := ""
	if haveRaw {
		mime = sniffImageRoutingMime(raw)
	}
	if mime == "" {
		guessed, _, err := pymimetypes.GuessType(path, true)
		if err != nil {
			return "", err
		}
		mime = guessed
	}
	if strings.HasPrefix(mime, "image/") {
		return mime, nil
	}
	if m, ok := imageRoutingSuffixMimes[pystr.Lower(pySuffix(path))]; ok {
		return m, nil
	}
	return "image/jpeg", nil
}

// Base64 data URL of a local image at native size, PNG transcoded outside the accepted set, false when skipped
func fileToDataURL(env *Env, path string) (string, bool, error) {
	if blocked := readBlockError(env, path); blocked != "" {
		envLog(env).Warn(fmt.Sprintf("image_routing: blocked local image attachment %s -- %s", path, blocked))
		return "", false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		envLog(env).Warn(fmt.Sprintf("image_routing: failed to read %s — %s", path, err))
		return "", false, nil
	}
	mime, err := guessImageMime(path, raw, true)
	if err != nil {
		return "", false, err
	}
	if !supportedMediaTypes(env)[mime] {
		transcoded, terr := transcodeToPNG(raw)
		if terr != nil {
			envLog(env).Info("image_routing: Pillow could not transcode image to PNG -- " + terr.Error())
			envLog(env).Warn(fmt.Sprintf("image_routing: %s is %s which is not accepted by the active provider "+
				"and could not be transcoded to PNG; skipping this attachment.", path, mime))
			return "", false, nil
		}
		envLog(env).Info(fmt.Sprintf("image_routing: transcoded %s (%s) -> image/png for provider compatibility", filepath.Base(path), mime))
		raw, mime = transcoded, "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), true, nil
}

// FileToDataURL is the native size base64 data URL of a local image, false when the attachment is skipped
func FileToDataURL(env *Env, path string) (string, bool, error) { return fileToDataURL(env, path) }

// BuildNativeContentParts returns the user turn content parts and the unreadable local paths
func BuildNativeContentParts(env *Env, userText string, imagePaths, imageURLs []string) ([]any, []string, error) {
	skipped := []string{}
	var urls, hints []string
	for _, rawPath := range imagePaths {
		dataURL, ok := "", false
		if st, err := os.Stat(rawPath); err == nil && st.Mode().IsRegular() {
			var err error
			if dataURL, ok, err = fileToDataURL(env, rawPath); err != nil {
				return nil, nil, err
			}
		}
		if ok {
			urls, hints = append(urls, dataURL), append(hints, "[Image attached at: "+rawPath+"]")
		} else {
			skipped = append(skipped, rawPath)
		}
	}
	for _, u := range imageURLs {
		if u = pystr.Strip(u); u != "" {
			urls, hints = append(urls, u), append(hints, "[Image attached: "+u+"]")
		}
	}
	text := pystr.Strip(userText)
	if len(urls) == 0 {
		if text == "" {
			return []any{}, skipped, nil
		}
		return []any{map[string]any{"type": "text", "text": text}}, skipped, nil
	}
	if text == "" {
		text = "What do you see in this image?"
	}
	parts := []any{map[string]any{"type": "text", "text": text + "\n\n" + strings.Join(hints, "\n")}}
	for _, u := range urls {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	return parts, skipped, nil
}
