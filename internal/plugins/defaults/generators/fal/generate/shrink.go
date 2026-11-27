package generate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"

	"github.com/nickheyer/icebath/internal/generation"
)

const (
	// Pixels past which a picture is taken for a decompression bomb and sent as it is
	bombPixels = 2 * (1024 * 1024 * 1024 / 4 / 3)
	// Shortest long side a shrink goes down to
	floor = 256
)

// Data URL of a picture re-encoded to fit the budget, its long side shrunk by a quarter at a time down to 256 pixels: a JPEG, a PNG when it
// has transparency. The URL as it was when the picture can't be read
func shrink(u string, budget int) string {
	data, _, err := generation.Inline(u)
	if err != nil {
		return u
	}
	img, err := decode(data)
	if err != nil {
		slog.Debug("FAL source image could not be read to shrink it", "error", err)
		return u
	}
	img = upright(img, orientation(data))
	transparent := !opaque(img)
	bounds := img.Bounds()
	side := max(bounds.Dx(), bounds.Dy())
	for {
		frame := fit(img, side)
		var out bytes.Buffer
		kind := "image/jpeg"
		if transparent {
			kind, err = "image/png", png.Encode(&out, frame)
		} else {
			err = jpeg.Encode(&out, frame, &jpeg.Options{Quality: 90})
		}
		if err != nil {
			return u
		}
		shrunk := generation.DataURL(kind, out.Bytes())
		if len(shrunk) <= budget || side <= floor {
			slog.Info(fmt.Sprintf("Downscaled FAL source image to (%d, %d) (%d bytes inline)", frame.Bounds().Dx(), frame.Bounds().Dy(), len(shrunk)))
			return shrunk
		}
		side = side * 3 / 4
	}
}

// Picture of any format FAL sources come in, refusing one so large it is a decompression bomb
func decode(data []byte) (image.Image, error) {
	var configOf func() (image.Config, error)
	var decoded func() (image.Image, error)
	reader := func() *bytes.Reader { return bytes.NewReader(data) }
	switch generation.Sniff(data) {
	case "image/png":
		configOf, decoded = func() (image.Config, error) { return png.DecodeConfig(reader()) }, func() (image.Image, error) { return png.Decode(reader()) }
	case "image/jpeg":
		configOf, decoded = func() (image.Config, error) { return jpeg.DecodeConfig(reader()) }, func() (image.Image, error) { return jpeg.Decode(reader()) }
	case "image/gif":
		configOf, decoded = func() (image.Config, error) { return gif.DecodeConfig(reader()) }, func() (image.Image, error) { return gif.Decode(reader()) }
	case "image/bmp":
		configOf, decoded = func() (image.Config, error) { return bmp.DecodeConfig(reader()) }, func() (image.Image, error) { return bmp.Decode(reader()) }
	case "image/webp":
		configOf, decoded = func() (image.Config, error) { return webp.DecodeConfig(reader()) }, func() (image.Image, error) { return webp.Decode(reader()) }
	case "image/tiff":
		configOf, decoded = func() (image.Config, error) { return tiff.DecodeConfig(reader()) }, func() (image.Image, error) { return tiff.Decode(reader()) }
	case "image/avif":
		configOf, decoded = func() (image.Config, error) { return avif.DecodeConfig(reader()) }, func() (image.Image, error) { return avif.Decode(reader()) }
	case "image/heic":
		configOf, decoded = func() (image.Config, error) { return heic.DecodeConfig(reader()) }, func() (image.Image, error) { return heic.Decode(reader()) }
	default:
		return nil, fmt.Errorf("cannot identify image file")
	}
	config, err := configOf()
	if err != nil {
		return nil, err
	}
	if config.Width*config.Height > bombPixels {
		return nil, fmt.Errorf("Image size (%d pixels) exceeds limit of %d pixels, could be decompression bomb DOS attack.", config.Width*config.Height, bombPixels)
	}
	return decoded()
}

// Whether no pixel of the picture lets anything through
func opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

// Copy of the picture fitted into a square of the side, its aspect kept; as large as it is when it fits already
func fit(img image.Image, side int) *image.NRGBA {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w > side || h > side {
		if w >= h {
			w, h = side, max(h*side/w, 1)
		} else {
			w, h = max(w*side/h, 1), side
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(out, out.Bounds(), img, bounds, draw.Src, nil)
	return out
}

// Picture turned so its EXIF orientation reads as the first
func upright(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
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
			out.Set(x, y, color.NRGBAModel.Convert(img.At(bounds.Min.X+sx, bounds.Min.Y+sy)))
		}
	}
	return out
}

// EXIF orientation of a JPEG, PNG, WebP or TIFF, one when it says none
func orientation(data []byte) int {
	switch generation.Sniff(data) {
	case "image/jpeg":
		for off := 2; off+4 <= len(data) && data[off] == 0xff; {
			marker := data[off+1]
			if marker == 0xda || marker == 0xd9 {
				break
			}
			if marker == 0xd8 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
				off += 2
				continue
			}
			n := int(binary.BigEndian.Uint16(data[off+2:]))
			if n < 2 || off+2+n > len(data) {
				break
			}
			if segment := data[off+4 : off+2+n]; marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
				return exif(segment[6:])
			}
			off += 2 + n
		}
	case "image/png":
		for off := 8; off+12 <= len(data); {
			n := int(binary.BigEndian.Uint32(data[off:]))
			if n < 0 || off+12+n > len(data) {
				break
			}
			if string(data[off+4:off+8]) == "eXIf" {
				return exif(data[off+8 : off+8+n])
			}
			off += 12 + n
		}
	case "image/webp":
		for off := 12; off+8 <= len(data); {
			n := int(binary.LittleEndian.Uint32(data[off+4:]))
			if n < 0 || off+8+n > len(data) {
				break
			}
			if string(data[off:off+4]) == "EXIF" {
				return exif(bytes.TrimPrefix(data[off+8:off+8+n], []byte("Exif\x00\x00")))
			}
			off += 8 + n + n%2
		}
	case "image/tiff":
		return exif(data)
	}
	return 1
}

// Orientation tag of the first directory of a TIFF stream, one when it has none
func exif(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	off := int(order.Uint32(t[4:8]))
	if off < 0 || off+2 > len(t) {
		return 1
	}
	for i := range int(order.Uint16(t[off:])) {
		entry := off + 2 + 12*i
		if entry+12 > len(t) {
			break
		}
		if order.Uint16(t[entry:]) != 0x0112 {
			continue
		}
		switch order.Uint16(t[entry+2:]) {
		case 3:
			return int(order.Uint16(t[entry+8:]))
		case 4:
			return int(order.Uint32(t[entry+8:]))
		}
		return 1
	}
	return 1
}
