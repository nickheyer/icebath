package tuiserver

import (
	"bytes"
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
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi/kitty"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

const (
	// Nominal pixels of a terminal cell, the size a kitty image is fitted to its cell box by
	inlineCellW, inlineCellH = 8, 16
	// Pixels transmitted per nominal cell pixel, sharp on dense displays without shipping a whole photo through the tty
	inlinePixelsPerCellPixel = 2
	// Working memory a preview decode may take, that of a 40 megapixel image at four bytes a pixel
	inlineMaxDecodeBytes = 4 * 40_000_000
	// Image ids of inline pictures, above the ids pets take and below the ids the UI kit hands its surfaces
	inlineImageIDBase, inlineImageIDEnd = 0x8000, 0x2c0000
	// Largest cell grid a preview is laid out on, whoever asks
	inlineMaxCols, inlineMaxRows = 200, 60
)

// Serialises preview decodes so their working memory never stacks
var inlineDecodeMu sync.Mutex

// Image laid out on a grid of cells, kitty placeholders with their transmit escape or half block cells
type inlineImage struct {
	mode        string
	cols, rows  int
	color       string
	transmit    string
	placeholder []string
	cells       [][][8]int64
}

// Effective kitty, unicode or off of display.inline_images for a client that paints cells, detected its own pick for auto
func resolveInlineMode(configured any, graphics bool, detected string) string {
	if on, isBool := configured.(bool); isBool && !on {
		return "off"
	}
	mode := "auto"
	if _, isBool := configured.(bool); configured != nil && !isBool {
		mode = strings.ToLower(strings.TrimSpace(anyutil.String(configured)))
	}
	switch mode {
	case "off":
		return "off"
	case "unicode":
		return "unicode"
	case "kitty":
		if graphics {
			return "kitty"
		}
		return "unicode"
	}
	if graphics && detected == "kitty" {
		return "kitty"
	}
	return "unicode"
}

// Cell grid of a width by height image, never wider than its natural size and never past the caps
func inlineGrid(width, height, maxCols, maxRows int) (cols, rows int) {
	cols = max(1, min(maxCols, (width+inlineCellW-1)/inlineCellW))
	rows = max(1, int(math.RoundToEven(float64(cols)*float64(height)/float64(width)*inlineCellW/inlineCellH)))
	if rows > maxRows {
		rows = maxRows
		cols = max(1, min(cols, int(math.RoundToEven(float64(rows)*float64(width)/float64(height)*inlineCellH/inlineCellW))))
	}
	return cols, rows
}

// Container of image bytes by their first bytes, empty when unrecognised
func sniffInlineFormat(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "webp"
	case bytes.HasPrefix(data, []byte("BM")):
		return "bmp"
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return "tiff"
	case bytes.HasPrefix(data, []byte{0, 0, 1, 0}):
		return "ico"
	}
	return ""
}

// Largest picture of an icon file, its payload and whether that payload is a PNG rather than a bitmap
func icoLargest(data []byte) (payload []byte, isPNG bool, err error) {
	if len(data) < 6 {
		return nil, false, errors.New("truncated icon file")
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	best := -1
	for i := range count {
		entry := 6 + 16*i
		if entry+16 > len(data) {
			break
		}
		w, h := int(data[entry]), int(data[entry+1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size, off := int(binary.LittleEndian.Uint32(data[entry+8:])), int(binary.LittleEndian.Uint32(data[entry+12:]))
		if off < 0 || size <= 0 || off+size > len(data) || w*h <= best {
			continue
		}
		best, payload = w*h, data[off:off+size]
	}
	if payload == nil {
		return nil, false, errors.New("icon file holds no picture")
	}
	return payload, bytes.HasPrefix(payload, []byte("\x89PNG\r\n\x1a\n")), nil
}

// Picture of an icon bitmap, its colour rows decoded as a bitmap file and the transparency taken from the alpha bytes or the mask rows
func decodeIcoBitmap(dib []byte) (image.Image, error) {
	if len(dib) < 40 {
		return nil, errors.New("truncated icon bitmap")
	}
	le := binary.LittleEndian
	headerLen, width, doubled, depth := int(le.Uint32(dib)), int(int32(le.Uint32(dib[4:]))), int(int32(le.Uint32(dib[8:]))), int(le.Uint16(dib[14:]))
	height := doubled / 2
	if headerLen < 40 || headerLen > len(dib) || width <= 0 || height <= 0 {
		return nil, errors.New("unsupported icon bitmap")
	}
	colors := 0
	if depth <= 8 {
		if colors = int(le.Uint32(dib[32:])); colors == 0 {
			colors = 1 << depth
		}
	}
	pixels := headerLen + 4*colors
	stride := (width*depth + 31) / 32 * 4
	if pixels+stride*height > len(dib) {
		return nil, errors.New("truncated icon bitmap")
	}
	file := make([]byte, 14, 14+pixels+stride*height)
	file[0], file[1] = 'B', 'M'
	le.PutUint32(file[2:], uint32(14+pixels+stride*height))
	le.PutUint32(file[10:], uint32(14+pixels))
	file = append(file, dib[:pixels+stride*height]...)
	le.PutUint32(file[14+8:], uint32(height))
	le.PutUint32(file[14+20:], uint32(stride*height))
	decoded, err := bmp.Decode(bytes.NewReader(file))
	if err != nil {
		return nil, err
	}
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(out, out.Rect, decoded, decoded.Bounds().Min, draw.Src)
	rows := dib[pixels:]
	mask := rows[stride*height:]
	maskStride := (width + 31) / 32 * 4
	for y := range height {
		from := height - 1 - y
		for x := range width {
			alpha := uint8(0xff)
			switch {
			case depth == 32:
				alpha = rows[from*stride+4*x+3]
			case len(mask) >= maskStride*height && mask[from*maskStride+x/8]&(0x80>>(x%8)) != 0:
				alpha = 0
			}
			out.Pix[out.PixOffset(x, y)+3] = alpha
		}
	}
	return out, nil
}

// First frame of an animated WebP as a still file of its own, the bytes unchanged when they hold no animation
func webpFirstFrame(data []byte) []byte {
	if len(data) < 30 || string(data[12:16]) != "VP8X" || data[20]&0x02 == 0 {
		return data
	}
	for off := 12; off+8 <= len(data); {
		kind, size := string(data[off:off+4]), int(binary.LittleEndian.Uint32(data[off+4:]))
		body := off + 8
		if size < 0 || body+size > len(data) {
			break
		}
		if kind == "ANMF" && size > 16 {
			frame := data[body+16 : body+size]
			still := make([]byte, 12, 12+len(frame))
			copy(still, "RIFF")
			binary.LittleEndian.PutUint32(still[4:], uint32(4+len(frame)))
			copy(still[8:], "WEBP")
			return append(still, frame...)
		}
		off = body + size + size%2
	}
	return data
}

// Pixel size of an image and the bytes each pixel takes while it decodes
func inlineConfig(format string, data []byte) (width, height int, cost float64, err error) {
	cost = 4
	var cfg image.Config
	switch format {
	case "png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case "jpeg":
		if cfg, err = jpeg.DecodeConfig(bytes.NewReader(data)); err == nil {
			cost = jpegDecodeCost(data)
		}
	case "gif":
		cfg, err = gif.DecodeConfig(bytes.NewReader(data))
	case "webp":
		cfg, err = webp.DecodeConfig(bytes.NewReader(webpFirstFrame(data)))
	case "bmp":
		cfg, err = bmp.DecodeConfig(bytes.NewReader(data))
	case "tiff":
		cfg, err = tiff.DecodeConfig(bytes.NewReader(data))
	case "ico":
		payload, isPNG, icoErr := icoLargest(data)
		switch {
		case icoErr != nil:
			err = icoErr
		case isPNG:
			cfg, err = png.DecodeConfig(bytes.NewReader(payload))
		case len(payload) < 12:
			err = errors.New("truncated icon bitmap")
		default:
			cfg.Width = int(int32(binary.LittleEndian.Uint32(payload[4:])))
			cfg.Height = int(int32(binary.LittleEndian.Uint32(payload[8:]))) / 2
		}
	default:
		err = errors.New("not an image this preview decodes")
	}
	if err == nil && (cfg.Width <= 0 || cfg.Height <= 0) {
		err = errors.New("image has no pixels")
	}
	if cfg.ColorModel == color.RGBA64Model || cfg.ColorModel == color.NRGBA64Model {
		cost = 8
	}
	return cfg.Width, cfg.Height, cost, err
}

// Bytes a pixel of a JPEG takes while it decodes, its sample planes plus the coefficients a progressive scan keeps
func jpegDecodeCost(data []byte) float64 {
	for off := 2; off+4 <= len(data); {
		if data[off] != 0xff {
			return 4
		}
		marker, size := data[off+1], int(binary.BigEndian.Uint16(data[off+2:]))
		if marker == 0xda || size < 2 || off+2+size > len(data) {
			return 4
		}
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			seg := data[off+4 : off+2+size]
			if len(seg) < 6 || len(seg) < 6+3*int(seg[5]) {
				return 4
			}
			maxH, maxV, samples := 1, 1, 0.0
			for i := range int(seg[5]) {
				maxH, maxV = max(maxH, int(seg[7+3*i]>>4)), max(maxV, int(seg[7+3*i]&0x0f))
			}
			for i := range int(seg[5]) {
				samples += float64(int(seg[7+3*i]>>4)*int(seg[7+3*i]&0x0f)) / float64(maxH*maxV)
			}
			if marker == 0xc2 {
				return samples * 5
			}
			return samples
		}
		off += 2 + size
	}
	return 4
}

// Orientation tag of a TIFF block, one when the block carries none
func tiffOrientation(block []byte) int {
	if len(block) < 8 {
		return 1
	}
	var order binary.ByteOrder = binary.LittleEndian
	if string(block[:2]) == "MM" {
		order = binary.BigEndian
	} else if string(block[:2]) != "II" {
		return 1
	}
	dir := int(order.Uint32(block[4:]))
	if dir < 0 || dir+2 > len(block) {
		return 1
	}
	for i := range int(order.Uint16(block[dir:])) {
		entry := dir + 2 + 12*i
		if entry+12 > len(block) {
			return 1
		}
		if order.Uint16(block[entry:]) == 0x0112 {
			if value := int(order.Uint16(block[entry+8:])); value >= 1 && value <= 8 {
				return value
			}
			return 1
		}
	}
	return 1
}

// Orientation the metadata of an image asks for, one for an upright image or one that says nothing
func inlineOrientation(format string, data []byte) int {
	exif := func(block []byte) int { return tiffOrientation(bytes.TrimPrefix(block, []byte("Exif\x00\x00"))) }
	switch format {
	case "tiff":
		return tiffOrientation(data)
	case "jpeg":
		for off := 2; off+4 <= len(data) && data[off] == 0xff; {
			marker, size := data[off+1], int(binary.BigEndian.Uint16(data[off+2:]))
			if marker == 0xda || size < 2 || off+2+size > len(data) {
				break
			}
			if seg := data[off+4 : off+2+size]; marker == 0xe1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				return exif(seg)
			}
			off += 2 + size
		}
	case "png":
		for off := 8; off+12 <= len(data); {
			size := int(binary.BigEndian.Uint32(data[off:]))
			if size < 0 || off+12+size > len(data) {
				break
			}
			switch string(data[off+4 : off+8]) {
			case "eXIf":
				return exif(data[off+8 : off+8+size])
			case "IDAT", "IEND":
				return 1
			}
			off += 12 + size
		}
	case "webp":
		for off := 12; off+8 <= len(data); {
			size := int(binary.LittleEndian.Uint32(data[off+4:]))
			if size < 0 || off+8+size > len(data) {
				break
			}
			if string(data[off:off+4]) == "EXIF" {
				return exif(data[off+8 : off+8+size])
			}
			off += 8 + size + size%2
		}
	}
	return 1
}

// Whole picture of image bytes, the first frame of an animation
func decodeInline(format string, data []byte) (image.Image, error) {
	switch format {
	case "png":
		return png.Decode(bytes.NewReader(data))
	case "jpeg":
		return jpeg.Decode(bytes.NewReader(data))
	case "gif":
		return gif.Decode(bytes.NewReader(data))
	case "webp":
		return webp.Decode(bytes.NewReader(webpFirstFrame(data)))
	case "bmp":
		return bmp.Decode(bytes.NewReader(data))
	case "tiff":
		return tiff.Decode(bytes.NewReader(data))
	}
	payload, isPNG, err := icoLargest(data)
	if err != nil {
		return nil, err
	}
	if isPNG {
		return png.Decode(bytes.NewReader(payload))
	}
	return decodeIcoBitmap(payload)
}

// Copy of a picture resampled to w by h pixels with straight alpha
func resampled(src image.Image, w, h int) *image.NRGBA {
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	if b := src.Bounds(); b.Dx() == w && b.Dy() == h {
		draw.Draw(scaled, scaled.Rect, src, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(scaled, scaled.Rect, src, b, draw.Src, nil)
	}
	out := image.NewNRGBA(scaled.Rect)
	draw.Draw(out, out.Rect, scaled, image.Point{}, draw.Src)
	return out
}

// Picture turned upright from the orientation its metadata gave
func upright(img *image.NRGBA, orientation int) *image.NRGBA {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	ow, oh := w, h
	if orientation >= 5 {
		ow, oh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, ow, oh))
	for y := range oh {
		for x := range ow {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			copy(out.Pix[out.PixOffset(x, y):out.PixOffset(x, y)+4], img.Pix[img.PixOffset(sx, sy):img.PixOffset(sx, sy)+4])
		}
	}
	return out
}

// Image of a file decoded no larger than its preview needs, upright, with the cell grid it is laid out on
func decodePreview(path string, maxCols, maxRows int) (*image.NRGBA, int, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if info.Size() > inlineMaxDecodeBytes {
		return nil, 0, 0, fmt.Errorf("%d bytes is past the preview decode cap", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	format := sniffInlineFormat(data)
	width, height, cost, err := inlineConfig(format, data)
	if err != nil {
		return nil, 0, 0, err
	}
	if float64(width)*float64(height)*cost > inlineMaxDecodeBytes {
		return nil, 0, 0, fmt.Errorf("%dx%d is past the preview decode cap", width, height)
	}
	orientation := inlineOrientation(format, data)
	swapped := orientation >= 5
	if swapped {
		width, height = height, width
	}
	cols, rows := inlineGrid(width, height, maxCols, maxRows)
	boxW, boxH := cols*inlineCellW*inlinePixelsPerCellPixel, rows*inlineCellH*inlinePixelsPerCellPixel
	inlineDecodeMu.Lock()
	defer inlineDecodeMu.Unlock()
	decoded, err := decodeInline(format, data)
	if err != nil {
		return nil, 0, 0, err
	}
	w, h := width, height
	if w > boxW || h > boxH {
		f := min(float64(boxW)/float64(w), float64(boxH)/float64(h))
		w, h = max(int(math.Round(float64(w)*f)), 1), max(int(math.Round(float64(h)*f)), 1)
	}
	if swapped {
		w, h = h, w
	}
	return upright(resampled(decoded, w, h), orientation), cols, rows, nil
}

// Kitty image id of one revision of a file on one cell grid, so a repaint reuses the terminal's copy and an edited file or another size gets its own
func inlineImageID(path string, cols, rows int) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return 0, err
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%s:%d:%d:%dx%d", resolved, info.ModTime().UnixNano(), info.Size(), cols, rows)
	return inlineImageIDBase + int(crc32.ChecksumIEEE([]byte(key))%(inlineImageIDEnd-inlineImageIDBase)), nil
}

// Kitty escape transmitting a PNG as a virtual placement over cols by rows cells, in chunks of 4096 characters with replies muted
func kittyVirtualTransmit(img *image.NRGBA, id, cols, rows int) (string, error) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return "", err
	}
	data := base64.StdEncoding.EncodeToString(encoded.Bytes())
	var out strings.Builder
	for first := true; first || data != ""; first = false {
		piece := data[:min(len(data), 4096)]
		data = data[len(piece):]
		out.WriteString("\x1b_G")
		if first {
			fmt.Fprintf(&out, "a=T,U=1,i=%d,c=%d,r=%d,f=100,q=2,", id, cols, rows)
		}
		more := 0
		if data != "" {
			more = 1
		}
		fmt.Fprintf(&out, "m=%d;%s\x1b\\", more, piece)
	}
	return out.String(), nil
}

// Text grid kitty paints the image under, each row led by its row mark and the columns counting up from it
func kittyPlaceholderRows(cols, rows int) []string {
	out := make([]string, rows)
	for r := range out {
		out[r] = string(kitty.Placeholder) + string(kitty.Diacritic(r)) + strings.Repeat(string(kitty.Placeholder), cols-1)
	}
	return out
}

// Half block cells of a picture resampled to cols by rows cells, each the top and bottom pixel as eight channel values
func halfBlockCells(img *image.NRGBA, cols, rows int) [][][8]int64 {
	small := resampled(img, cols, rows*2)
	out := make([][][8]int64, rows)
	for r := range out {
		out[r] = make([][8]int64, cols)
		for c := range cols {
			top, bottom := small.PixOffset(c, 2*r), small.PixOffset(c, 2*r+1)
			for i := range 4 {
				out[r][c][i], out[r][c][4+i] = int64(small.Pix[top+i]), int64(small.Pix[bottom+i])
			}
		}
	}
	return out
}

// Lays out the image of a file for a mode on a grid inside the cell budget, nil when it cannot be shown
func renderInlineImage(path, mode string, maxCols, maxRows int) *inlineImage {
	if (mode != "kitty" && mode != "unicode") || maxCols < 1 || maxRows < 1 {
		return nil
	}
	img, cols, rows, err := decodePreview(path, maxCols, maxRows)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("inline image render failed (%s): %s", path, err))
		return nil
	}
	if mode == "unicode" {
		return &inlineImage{mode: mode, cols: cols, rows: rows, cells: halfBlockCells(img, cols, rows)}
	}
	id, err := inlineImageID(path, cols, rows)
	if err == nil {
		var transmit string
		if transmit, err = kittyVirtualTransmit(img, id, cols, rows); err == nil {
			return &inlineImage{mode: mode, cols: cols, rows: rows, color: fmt.Sprintf("#%06x", id&0xFFFFFF), transmit: transmit,
				placeholder: kittyPlaceholderRows(cols, rows)}
		}
	}
	pkgLog().Debug(fmt.Sprintf("inline image render failed (%s): %s", path, err))
	return nil
}
