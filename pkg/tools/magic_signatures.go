package tools

// MagicSignature is one binary file prefix and its human name
type MagicSignature struct {
	Prefix []byte
	Name   string
}

// Ordered binary signatures, first match wins, longer prefixes sharing a first byte first
var fileMagicSignatures = []MagicSignature{
	{[]byte("\x89PNG\r\n\x1a\n"), "PNG image data"},
	{[]byte("\xff\xd8\xff"), "JPEG image data"},
	{[]byte("GIF87a"), "GIF image data"},
	{[]byte("GIF89a"), "GIF image data"},
	{[]byte("RIFF"), "RIFF container (WAV/AVI/WebP family)"},
	{[]byte("%PDF-"), "PDF document"},
	{[]byte("PK\x03\x04"), "ZIP archive (also docx/xlsx/jar/apk)"},
	{[]byte("PK\x05\x06"), "ZIP archive (empty)"},
	{[]byte("\x1f\x8b"), "gzip compressed data"},
	{[]byte("BZh"), "bzip2 compressed data"},
	{[]byte("\xfd7zXZ\x00"), "xz compressed data"},
	{[]byte("7z\xbc\xaf\x27\x1c"), "7-Zip archive"},
	{[]byte("\x7fELF"), "ELF executable"},
	{[]byte("MZ"), "Windows PE executable"},
	{[]byte("\xcf\xfa\xed\xfe"), "Mach-O executable (64-bit)"},
	{[]byte("\xca\xfe\xba\xbe"), "Mach-O universal binary / Java class"},
	{[]byte("SQLite format 3\x00"), "SQLite database"},
	{[]byte("OggS"), "Ogg container"},
	{[]byte("fLaC"), "FLAC audio"},
	{[]byte("ID3"), "MP3 audio (ID3 tag)"},
	{[]byte("\x00\x00\x00"), "ISO media container (MP4/MOV family)"},
	{[]byte("BM"), "BMP image data"},
	{[]byte("II*\x00"), "TIFF image data (little-endian)"},
	{[]byte("MM\x00*"), "TIFF image data (big-endian)"},
}

// FileMagicSignatures is the shared binary signature table of the file and web tools
func FileMagicSignatures() []MagicSignature { return fileMagicSignatures }

// IdentifyBinaryBytes names binary content from its magic bytes, the ISO media entry also needing ftyp at offset 4
func IdentifyBinaryBytes(sample []byte) string {
	for _, sig := range fileMagicSignatures {
		if !hasBytesPrefix(sample, sig.Prefix) {
			continue
		}
		if len(sig.Name) >= 9 && sig.Name[:9] == "ISO media" && (len(sample) < 8 || string(sample[4:8]) != "ftyp") {
			continue
		}
		return sig.Name
	}
	return ""
}

// Reports whether data starts with prefix
func hasBytesPrefix(data, prefix []byte) bool {
	return len(data) >= len(prefix) && string(data[:len(prefix)]) == string(prefix)
}
