package tools

import "bytes"

// Kind of binary content by the bytes it starts with
type Signature struct {
	Prefix string
	Name   string
}

// Binary kinds the file and web tools tell apart, first match winning, longer prefixes sharing a first byte first
var Signatures = []Signature{
	{"\x89PNG\r\n\x1a\n", "PNG image data"},
	{"\xff\xd8\xff", "JPEG image data"},
	{"GIF87a", "GIF image data"},
	{"GIF89a", "GIF image data"},
	{"RIFF", "RIFF container (WAV/AVI/WebP family)"},
	{"%PDF-", "PDF document"},
	{"PK\x03\x04", "ZIP archive (also docx/xlsx/jar/apk)"},
	{"PK\x05\x06", "ZIP archive (empty)"},
	{"\x1f\x8b", "gzip compressed data"},
	{"BZh", "bzip2 compressed data"},
	{"\xfd7zXZ\x00", "xz compressed data"},
	{"7z\xbc\xaf\x27\x1c", "7-Zip archive"},
	{"\x7fELF", "ELF executable"},
	{"MZ", "Windows PE executable"},
	{"\xcf\xfa\xed\xfe", "Mach-O executable (64-bit)"},
	{"\xca\xfe\xba\xbe", "Mach-O universal binary / Java class"},
	{"SQLite format 3\x00", "SQLite database"},
	{"OggS", "Ogg container"},
	{"fLaC", "FLAC audio"},
	{"ID3", "MP3 audio (ID3 tag)"},
	{"\x00\x00\x00", "ISO media container (MP4/MOV family)"},
	{"BM", "BMP image data"},
	{"II*\x00", "TIFF image data (little-endian)"},
	{"MM\x00*", "TIFF image data (big-endian)"},
}

// Name of the binary kind the bytes start as, empty when none; ISO media also needs ftyp at offset 4
func Identify(sample []byte) string {
	for _, signature := range Signatures {
		if !bytes.HasPrefix(sample, []byte(signature.Prefix)) {
			continue
		}
		if signature.Prefix == "\x00\x00\x00" && (len(sample) < 8 || string(sample[4:8]) != "ftyp") {
			continue
		}
		return signature.Name
	}
	return ""
}
