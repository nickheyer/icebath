package imaging

import (
	"bytes"
	"encoding/binary"
)

// Leading bytes of every PNG file
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// ISO-BMFF brands that mark an AV1 coded image or sequence
var avifBrands = []string{"avif", "avis", "av01"}

// ISO-BMFF brands that mark an HEVC coded or generic HEIF image or sequence
var heifBrands = []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1"}

// Format the media resolvers accept by magic bytes, PNG only when every checksum holds and HEIF by any brand
func Sniff(data []byte) Format {
	switch f := sniffContainer(data); f {
	case PNG:
		if verifyPNG(data) != nil {
			return UnknownFormat
		}
		return PNG
	case JPEG, GIF, BMP, WebP, HEIC, AVIF:
		return f
	}
	return UnknownFormat
}

// Container a decode routes by, the HEIF family judged by its major and compatible brands
func sniffContainer(data []byte) Format {
	header := data[:min(len(data), 64)]
	if f := sniffPrefix(header); f != UnknownFormat {
		return f
	}
	if len(header) >= 12 && string(header[4:8]) == "ftyp" {
		size := int(binary.BigEndian.Uint32(header[:4]))
		limit := 16
		if size >= 16 {
			limit = min(size, len(header))
		}
		brands := []string{string(header[8:12])}
		for i := 16; i < limit-3; i += 4 {
			brands = append(brands, string(header[i:i+4]))
		}
		if anyBrand(brands, avifBrands) {
			return AVIF
		}
		if anyBrand(brands, heifBrands) {
			return HEIC
		}
	}
	return sniffTail(header)
}

// Container format from magic bytes with the HEIF family judged by the major brand alone, then an SVG document start
func SniffMajorBrand(data []byte) Format {
	if len(data) == 0 {
		return UnknownFormat
	}
	if f := sniffPrefix(data); f != UnknownFormat {
		return f
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch string(data[8:12]) {
		case "avif", "avis":
			return AVIF
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1", "heim", "heis":
			return HEIC
		}
	}
	if f := sniffTail(data); f != UnknownFormat {
		return f
	}
	head := asciiLower(bytes.TrimLeft(data[:min(len(data), 512)], " \t\n\r\v\f"))
	if (bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<svg"))) && bytes.Contains(head, []byte("<svg")) {
		return SVG
	}
	return UnknownFormat
}

// Reports an svg tag within the first 4096 bytes, case ignored
func HasSVGTag(data []byte) bool {
	return bytes.Contains(asciiLower(data[:min(len(data), 4096)]), []byte("<svg"))
}

// Copy with ASCII capitals lowered and every other byte kept
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// Formats identified by a fixed prefix ahead of the ISO-BMFF check
func sniffPrefix(b []byte) Format {
	switch {
	case bytes.HasPrefix(b, pngSignature):
		return PNG
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return JPEG
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return GIF
	case bytes.HasPrefix(b, []byte("BM")):
		return BMP
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return WebP
	}
	return UnknownFormat
}

// Formats identified by a fixed prefix after the ISO-BMFF check
func sniffTail(b []byte) Format {
	switch {
	case bytes.HasPrefix(b, []byte("II*\x00")), bytes.HasPrefix(b, []byte("MM\x00*")):
		return TIFF
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0}):
		return ICO
	}
	return UnknownFormat
}

// Reports whether any brand of one list appears in the other
func anyBrand(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}
