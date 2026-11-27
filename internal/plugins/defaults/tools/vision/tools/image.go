package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	// Most pixels an image may decode to, past which it is taken for a decompression bomb
	maxPixels = 2 * 89478485
	// Most frames and decoded pixels of an animation checked before it goes to the model
	maxFrames      = 100
	maxFramePixels = 100_000_000
	// Smallest side a downscale goes to
	floor = 64
)

// Formats every vision API reads as they are
var readable = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// Refusal of an image the model is not to see, worded for it
type prepError struct{ text string }

func (e *prepError) Error() string { return e.text }

// Type of an image from its first bytes, empty when it is none the tools read. A PNG must be whole to count
func sniff(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		if _, err := pngChunks(data); err != nil {
			return ""
		}
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		brands := string(data[8:min(len(data), 64)])
		for _, brand := range []string{"avif", "avis", "av01"} {
			if strings.Contains(brands, brand) {
				return "image/avif"
			}
		}
		for _, brand := range []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1"} {
			if strings.Contains(brands, brand) {
				return "image/heic"
			}
		}
	}
	return ""
}

// Whether the bytes are an SVG document
func svg(data []byte) bool {
	return bytes.Contains(bytes.ToLower(data[:min(len(data), 4096)]), []byte("<svg"))
}

type chunk struct {
	kind string
	data []byte
}

// Chunks of a PNG, each with its checksum checked, through IEND
func pngChunks(data []byte) ([]chunk, error) {
	var chunks []chunk
	for at := 8; ; {
		if at+12 > len(data) {
			return nil, errors.New("truncated PNG")
		}
		size := int(binary.BigEndian.Uint32(data[at:]))
		if size < 0 || at+12+size > len(data) {
			return nil, errors.New("truncated PNG chunk")
		}
		kind, body := string(data[at+4:at+8]), data[at+8:at+8+size]
		if crc32.ChecksumIEEE(data[at+4:at+8+size]) != binary.BigEndian.Uint32(data[at+8+size:]) {
			return nil, fmt.Errorf("broken PNG file (bad checksum in %q)", kind)
		}
		chunks = append(chunks, chunk{kind: kind, data: body})
		if kind == "IEND" {
			return chunks, nil
		}
		at += 12 + size
	}
}

// Chunks of a WebP container
func webpChunks(data []byte) []chunk {
	var chunks []chunk
	for at := 12; at+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[at+4:]))
		end := at + 8 + size
		if size < 0 || end > len(data) {
			break
		}
		chunks = append(chunks, chunk{kind: string(data[at : at+4]), data: data[at+8 : end]})
		at = end + size%2
	}
	return chunks
}

func uint24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}

// Refusal of an image whose size marks it as a decompression bomb
func bomb(width, height int) error {
	if pixels := width * height; pixels > maxPixels {
		return &prepError{fmt.Sprintf("Image size (%d pixels) exceeds limit of %d pixels, could be decompression bomb DOS attack.", pixels, maxPixels)}
	}
	return nil
}

// First frame of the image, drawn on its whole canvas
func decode(data []byte, mime string) (image.Image, error) {
	switch mime {
	case "image/png":
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return nil, err
		}
		return png.Decode(bytes.NewReader(data))
	case "image/jpeg":
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return nil, err
		}
		return jpeg.Decode(bytes.NewReader(data))
	case "image/gif":
		config, err := gif.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return nil, err
		}
		all, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, all.Config.Width, all.Config.Height))
		if len(all.Image) > 0 {
			draw.Draw(canvas, all.Image[0].Bounds(), all.Image[0], all.Image[0].Bounds().Min, draw.Over)
		}
		return canvas, nil
	case "image/webp":
		return decodeWebP(data)
	case "image/bmp":
		config, err := bmp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return nil, err
		}
		return bmp.Decode(bytes.NewReader(data))
	case "image/heic":
		return heic.Decode(bytes.NewReader(data))
	case "image/avif":
		return avif.Decode(bytes.NewReader(data))
	}
	return nil, fmt.Errorf("cannot identify image file (%s)", mime)
}

// WebP image, the first frame of an animation on its canvas
func decodeWebP(data []byte) (image.Image, error) {
	chunks := webpChunks(data)
	var width, height int
	animated := false
	for _, c := range chunks {
		if c.kind == "VP8X" && len(c.data) >= 10 {
			animated = c.data[0]&0x02 != 0
			width, height = uint24(c.data[4:])+1, uint24(c.data[7:])+1
		}
	}
	if !animated {
		config, err := webp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return nil, err
		}
		return webp.Decode(bytes.NewReader(data))
	}
	if err := bomb(width, height); err != nil {
		return nil, err
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	for _, c := range chunks {
		if c.kind != "ANMF" || len(c.data) < 16 {
			continue
		}
		x, y := uint24(c.data[0:])*2, uint24(c.data[3:])*2
		frame, err := webp.Decode(bytes.NewReader(still(c.data[16:], uint24(c.data[6:])+1, uint24(c.data[9:])+1)))
		if err != nil {
			return nil, err
		}
		draw.Draw(canvas, frame.Bounds().Add(image.Pt(x, y)), frame, frame.Bounds().Min, draw.Over)
		break
	}
	return canvas, nil
}

// Still WebP file made of the bitstream chunks of one animation frame
func still(frame []byte, width, height int) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	if bytes.HasPrefix(frame, []byte("ALPH")) {
		header := make([]byte, 10)
		header[0] = 0x10
		put24(header[4:], width-1)
		put24(header[7:], height-1)
		writeChunk(&body, "VP8X", header)
	}
	body.Write(frame)
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(body.Len()))
	return append(out, body.Bytes()...)
}

func put24(b []byte, v int) {
	b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16)
}

func writeChunk(b *bytes.Buffer, kind string, data []byte) {
	b.WriteString(kind)
	binary.Write(b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	if len(data)%2 == 1 {
		b.WriteByte(0)
	}
}

// Image in a format the vision API reads: a readable one as it is, an SVG drawn by a rasterizer installed here, any other its first frame
// as a PNG
func normalize(ctx context.Context, data []byte, mime string) ([]byte, string, error) {
	if readable[mime] {
		return data, mime, nil
	}
	if mime == "image/svg+xml" {
		drawn, err := rasterize(ctx, data)
		return drawn, "image/png", err
	}
	img, err := decode(data, mime)
	if err != nil {
		var refused *prepError
		switch {
		case errors.As(err, &refused):
			return nil, "", err
		case mime == "image/heic":
			return nil, "", &prepError{fmt.Sprintf("This is a HEIC/HEIF image (common for iPhone photos), which vision models cannot read directly, and its HEVC payload could not be decoded (%v). Convert the image to PNG/JPEG first and re-run vision_analyze.", err)}
		case mime == "image/avif":
			return nil, "", &prepError{fmt.Sprintf("This is an AVIF image, which vision models cannot read directly, and its AV1 payload could not be decoded (%v). Convert the image to PNG/JPEG first and re-run vision_analyze.", err)}
		}
		return nil, "", &prepError{fmt.Sprintf("Image format %q is not supported by the vision API and could not be converted to PNG. Convert it to PNG or JPEG and try again.", mime)}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, "", &prepError{fmt.Sprintf("Image format %q is not supported by the vision API and could not be converted to PNG. Convert it to PNG or JPEG and try again.", mime)}
	}
	return out.Bytes(), "image/png", nil
}

// PNG of an SVG, drawn by rsvg-convert or Inkscape
func rasterize(ctx context.Context, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "icebath-svg-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "image.svg"), filepath.Join(dir, "image.png")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"rsvg-convert", "-o", out, in}, {"inkscape", in, "--export-type=png", "--export-filename=" + out}} {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		run, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := exec.CommandContext(run, args[0], args[1:]...).Run()
		cancel()
		if drawn, readErr := os.ReadFile(out); err == nil && readErr == nil && len(drawn) > 0 {
			return drawn, nil
		}
	}
	return nil, &prepError{"This is an SVG, which vision models cannot read directly, and no SVG rasterizer is installed (tried rsvg-convert, inkscape). " +
		"Convert the SVG to PNG first — e.g. open it in a browser and screenshot it — then re-run vision_analyze on the PNG."}
}

// Refusal of an image that would not decode whole, or an animation too long or too large to check
func validate(data []byte, mime string) error {
	failed := func(err error) error {
		var refused *prepError
		if errors.As(err, &refused) {
			return err
		}
		return &prepError{"Image could not be fully decoded: " + err.Error()}
	}
	frames := func(count, pixels int) error {
		if count > maxFrames {
			return &prepError{fmt.Sprintf("Image validation rejected animation: frame %d exceeds the maximum %d validated frames.", count, maxFrames)}
		}
		if pixels > maxFramePixels {
			return &prepError{fmt.Sprintf("Image validation rejected animation: aggregate decoded pixel count would reach %d at frame %d, exceeding the maximum %d.", pixels, count, maxFramePixels)}
		}
		return nil
	}
	switch mime {
	case "image/gif":
		config, err := gif.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return failed(err)
		}
		if err := bomb(config.Width, config.Height); err != nil {
			return err
		}
		all, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return failed(err)
		}
		pixels := 0
		for i, frame := range all.Image {
			pixels += frame.Bounds().Dx() * frame.Bounds().Dy()
			if err := frames(i+1, pixels); err != nil {
				return err
			}
		}
		return nil
	case "image/png":
		chunks, err := pngChunks(data)
		if err != nil {
			return failed(err)
		}
		pixels, count := 0, 0
		for _, c := range chunks {
			if c.kind == "fcTL" && len(c.data) >= 12 {
				count++
				pixels += int(binary.BigEndian.Uint32(c.data[4:])) * int(binary.BigEndian.Uint32(c.data[8:]))
				if err := frames(count, pixels); err != nil {
					return err
				}
			}
		}
	case "image/webp":
		pixels, count := 0, 0
		for _, c := range webpChunks(data) {
			if c.kind == "ANMF" && len(c.data) >= 12 {
				count++
				pixels += (uint24(c.data[6:]) + 1) * (uint24(c.data[9:]) + 1)
				if err := frames(count, pixels); err != nil {
					return err
				}
			}
		}
	}
	if _, err := decode(data, mime); err != nil {
		return failed(err)
	}
	return nil
}

// Region of the image cut out at full resolution as a PNG, with its offset in the original
func crop(data []byte, mime string, region any) ([]byte, image.Point, error) {
	invalid := &prepError{"Invalid region: expected [x1, y1, x2, y2] as four numbers (pixel coordinates in the original image)."}
	values, ok := region.([]any)
	if !ok || len(values) != 4 {
		return nil, image.Point{}, invalid
	}
	var box [4]int
	for i, value := range values {
		number, ok := value.(float64)
		if !ok {
			return nil, image.Point{}, invalid
		}
		box[i] = int(number)
	}
	img, err := decode(data, mime)
	if err != nil {
		var refused *prepError
		if errors.As(err, &refused) {
			return nil, image.Point{}, err
		}
		return nil, image.Point{}, &prepError{"Failed to crop region: " + err.Error()}
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	clamp := func(v, limit int) int { return min(max(v, 0), limit) }
	x1, y1, x2, y2 := clamp(box[0], w), clamp(box[1], h), clamp(box[2], w), clamp(box[3], h)
	if x2 <= x1 || y2 <= y1 {
		return nil, image.Point{}, &prepError{fmt.Sprintf("Invalid region [%d, %d, %d, %d]: crops to zero area after clamping to the image bounds. The image is %dx%d px — pick x1<x2 and y1<y2 inside [0, 0, %d, %d].",
			box[0], box[1], box[2], box[3], w, h, w, h)}
	}
	part := image.NewNRGBA(image.Rect(0, 0, x2-x1, y2-y1))
	draw.Draw(part, part.Bounds(), img, bounds.Min.Add(image.Pt(x1, y1)), draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, part); err != nil {
		return nil, image.Point{}, &prepError{"Failed to crop region: " + err.Error()}
	}
	return out.Bytes(), image.Pt(x1, y1), nil
}

// Size of the image, false when it can't be read
func dimensions(data []byte, mime string) (int, int, bool) {
	var config image.Config
	var err error
	switch mime {
	case "image/png":
		config, err = png.DecodeConfig(bytes.NewReader(data))
	case "image/jpeg":
		config, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "image/gif":
		config, err = gif.DecodeConfig(bytes.NewReader(data))
	case "image/webp":
		for _, c := range webpChunks(data) {
			if c.kind == "VP8X" && len(c.data) >= 10 {
				return uint24(c.data[4:]) + 1, uint24(c.data[7:]) + 1, true
			}
		}
		config, err = webp.DecodeConfig(bytes.NewReader(data))
	default:
		return 0, 0, false
	}
	return config.Width, config.Height, err == nil
}

// Downscale that was applied, from the original size to the one sent
type scale struct {
	from, to image.Point
}

// Image shrunk while it is over the byte budget or its long side over the limit: halved up to four times down to 64 pixels, a JPEG also
// stepping its quality down. The original comes back when it fits, the smallest try when nothing does
func resize(data []byte, mime string, budget, longest int, forceJPEG bool) ([]byte, string, *scale, error) {
	w, h, known := dimensions(data, mime)
	if size(len(data), mime) <= budget && !(longest > 0 && known && max(w, h) > longest) {
		return data, mime, nil, nil
	}
	img, err := decode(data, mime)
	if err != nil {
		return data, mime, nil, nil
	}
	keepPNG := mime == "image/png" && !forceJPEG
	qualities, out := []int{0}, "image/png"
	if !keepPNG {
		qualities, out = []int{85, 70, 50}, "image/jpeg"
	}
	original := img.Bounds().Size()
	current := original
	var best []byte
	for step := range 5 {
		if step > 0 {
			next := image.Pt(max(current.X/2, floor), max(current.Y/2, floor))
			if next.X == floor && current.X > 0 {
				next.Y = max(current.Y*floor/current.X, floor)
			} else if next.Y == floor && current.Y > 0 {
				next.X = max(current.X*floor/current.Y, floor)
			}
			if next == current {
				break
			}
			smaller := image.NewNRGBA(image.Rectangle{Max: next})
			draw.CatmullRom.Scale(smaller, smaller.Bounds(), img, img.Bounds(), draw.Src, nil)
			img, current = smaller, next
		}
		fits := longest <= 0 || max(current.X, current.Y) <= longest
		for _, quality := range qualities {
			var encoded bytes.Buffer
			if keepPNG {
				err = png.Encode(&encoded, img)
			} else {
				err = jpeg.Encode(&encoded, opaque(img), &jpeg.Options{Quality: quality})
			}
			if err != nil {
				return nil, "", nil, err
			}
			best = encoded.Bytes()
			if size(len(best), out) <= budget && fits {
				return best, out, shrunk(original, current), nil
			}
		}
	}
	return best, out, shrunk(original, current), nil
}

func shrunk(from, to image.Point) *scale {
	if from == to {
		return nil
	}
	return &scale{from: from, to: to}
}

// Image without its alpha, as JPEG keeps it: the stored colour of every pixel
func opaque(img image.Image) image.Image {
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			out.SetRGBA(x, y, color.RGBA{R: c.R, G: c.G, B: c.B, A: 255})
		}
	}
	return out
}

// Length of the data URL the bytes of the type make
func size(n int, mime string) int {
	return len("data:"+mime+";base64,") + base64.StdEncoding.EncodedLen(n)
}

// Note telling the model how its coordinates map back to the original image after a downscale and a crop, empty when neither applied
func note(s *scale, offset image.Point, cropped bool) string {
	var parts []string
	if s != nil {
		fx, fy := float64(s.from.X)/float64(max(s.to.X, 1)), float64(s.from.Y)/float64(max(s.to.Y, 1))
		sx, sy := fmt.Sprintf("%.2f", fx), fmt.Sprintf("%.2f", fy)
		axes := "any coordinates you report by " + sx
		if sx != sy {
			axes = "any x coordinates you report by " + sx + " and any y coordinates by " + sy
		}
		parts = append(parts, fmt.Sprintf("Image downscaled from %dx%d to %dx%d for vision; multiply %s to map back to the original image.", s.from.X, s.from.Y, s.to.X, s.to.Y, axes))
	}
	if cropped {
		parts = append(parts, fmt.Sprintf("Analysis was performed on a cropped region of the original image starting at offset (%d, %d); "+
			"coordinates are relative to that crop origin — add the offset to map back to the full image.", offset.X, offset.Y))
	}
	return strings.Join(parts, " ")
}
