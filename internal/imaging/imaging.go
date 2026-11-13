// Image format sniffing, decoding, encoding, resampling, palette quantization and QR codes
package imaging

import (
	"errors"
	"fmt"
	"mime"
	"strings"
)

// Image container kind
type Format uint8

// Containers the package sniffs, decodes or encodes
const (
	UnknownFormat Format = iota
	PNG
	JPEG
	GIF
	WebP
	BMP
	TIFF
	ICO
	HEIC
	AVIF
	SVG
)

// Lower case container name
func (f Format) String() string {
	switch f {
	case PNG:
		return "png"
	case JPEG:
		return "jpeg"
	case GIF:
		return "gif"
	case WebP:
		return "webp"
	case BMP:
		return "bmp"
	case TIFF:
		return "tiff"
	case ICO:
		return "ico"
	case HEIC:
		return "heic"
	case AVIF:
		return "avif"
	case SVG:
		return "svg"
	}
	return "unknown"
}

// Media type a sniff reports for the container, empty for an unknown format
func (f Format) MediaType() string {
	switch f {
	case ICO:
		return "image/x-icon"
	case SVG:
		return "image/svg+xml"
	case UnknownFormat:
		return ""
	}
	return "image/" + f.String()
}

// File extension with its dot that files of the container are written under
func (f Format) Extension() string {
	switch f {
	case JPEG:
		return ".jpg"
	case UnknownFormat:
		return ""
	}
	return "." + f.String()
}

// Format a media type names, parameters and case ignored, unknown for anything else
func FormatFromMediaType(mediaType string) Format {
	base, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	}
	switch base {
	case "image/png", "image/apng", "image/x-png":
		return PNG
	case "image/jpeg", "image/jpg", "image/pjpeg":
		return JPEG
	case "image/gif":
		return GIF
	case "image/webp":
		return WebP
	case "image/bmp", "image/x-bmp", "image/x-ms-bmp":
		return BMP
	case "image/tiff", "image/tiff-fx":
		return TIFF
	case "image/x-icon", "image/vnd.microsoft.icon", "image/ico":
		return ICO
	case "image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence":
		return HEIC
	case "image/avif", "image/avif-sequence":
		return AVIF
	case "image/svg+xml":
		return SVG
	}
	return UnknownFormat
}

// Format a file extension names, the dot optional and case ignored, unknown for anything else
func FormatFromExtension(ext string) Format {
	switch strings.TrimPrefix(strings.ToLower(ext), ".") {
	case "png", "apng":
		return PNG
	case "jpg", "jpeg", "jpe", "jfif":
		return JPEG
	case "gif":
		return GIF
	case "webp":
		return WebP
	case "bmp", "dib":
		return BMP
	case "tif", "tiff":
		return TIFF
	case "ico":
		return ICO
	case "heic", "heif", "hif":
		return HEIC
	case "avif", "avifs":
		return AVIF
	case "svg":
		return SVG
	}
	return UnknownFormat
}

// Bytes that match no image signature the package knows
var ErrUnknownFormat = errors.New("imaging: unknown image format")

// Recognized container whose content or requested operation the package cannot handle
var ErrUnsupported = errors.New("imaging: unsupported image")

// Corrupt or truncated image data, wrapping the decoder failure
type CorruptError struct {
	Format Format
	Err    error
}

// Format name and the decoder failure
func (e *CorruptError) Error() string {
	return fmt.Sprintf("imaging: corrupt %s image: %v", e.Format, e.Err)
}

// Decoder failure behind the corruption
func (e *CorruptError) Unwrap() error { return e.Err }

// Canvas whose pixel count marks a likely decompression bomb
type BombError struct {
	Pixels int
	Limit  int
}

// Pixel count and the limit it crossed
func (e *BombError) Error() string {
	return fmt.Sprintf("imaging: image size (%d pixels) exceeds limit of %d pixels, could be decompression bomb DOS attack", e.Pixels, e.Limit)
}

// Pixels above which opening an image is refused
const bombPixels = 2 * (1024 * 1024 * 1024 / 4 / 3)

// Refuses a canvas over the bomb limit, zero sides counting as one
func checkBomb(w, h int) error {
	pixels := max(1, w) * max(1, h)
	if pixels > bombPixels {
		return &BombError{Pixels: pixels, Limit: bombPixels}
	}
	return nil
}

// Wraps a decoder failure as corruption unless it already carries a package error
func corrupt(f Format, err error) error {
	if err == nil {
		return nil
	}
	var ce *CorruptError
	var be *BombError
	if errors.As(err, &ce) || errors.As(err, &be) || errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnknownFormat) {
		return err
	}
	return &CorruptError{Format: f, Err: err}
}
