package file

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

type magicSignature struct {
	prefix string
	name   string
}

var magicSignatures = [...]magicSignature{
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

func IdentifyBinary(sample []byte) string {
	for _, sig := range magicSignatures {
		if !bytes.HasPrefix(sample, []byte(sig.prefix)) {
			continue
		}
		if strings.HasPrefix(sig.name, "ISO media") && !(len(sample) >= 8 && string(sample[4:8]) == "ftyp") {
			continue
		}
		return sig.name
	}
	return "unknown binary"
}

func DescribeBinary(sample []byte, size int64) string {
	kind := IdentifyBinary(sample)
	var human string
	switch {
	case size >= 1024*1024:
		human = fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	case size >= 1024:
		human = fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		human = fmt.Sprintf("%d bytes", size)
	}
	return fmt.Sprintf("Binary file (%s, %s) — cannot display as text.", kind, human)
}

func isBinaryExtension(suffix string) bool {
	switch suffix {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".webp", ".tiff", ".tif",
		".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv", ".flv", ".m4v", ".mpeg", ".mpg",
		".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a", ".wma", ".aiff", ".opus",
		".zip", ".tar", ".gz", ".bz2", ".7z", ".rar", ".xz", ".z", ".tgz", ".iso",
		".exe", ".dll", ".so", ".dylib", ".bin", ".o", ".a", ".obj", ".lib", ".app", ".msi", ".deb", ".rpm",
		".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".odt", ".ods", ".odp",
		".ttf", ".otf", ".woff", ".woff2", ".eot",
		".pyc", ".pyo", ".class", ".jar", ".war", ".ear", ".node", ".wasm", ".rlib",
		".sqlite", ".sqlite3", ".db", ".mdb", ".idx",
		".psd", ".ai", ".eps", ".sketch", ".fig", ".xd", ".blend", ".3ds", ".max",
		".swf", ".fla", ".lockb", ".dat", ".data":
		return true
	}
	return false
}

func isOpaqueDocumentExtension(suffix string) bool {
	switch suffix {
	case ".doc", ".docx", ".docm", ".xls", ".xlsx", ".xlsm", ".xlsb",
		".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
		".odt", ".ods", ".odp", ".rtf", ".epub":
		return true
	}
	return false
}

func isImageExtension(suffix string) bool {
	switch suffix {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico":
		return true
	}
	return false
}

func lowerSuffix(path string) string {
	dot := strings.LastIndex(path, ".")
	if dot == -1 {
		return ""
	}
	return strings.ToLower(path[dot:])
}

func sidecarBase(suffix string) (string, bool) {
	for _, marker := range [...]string{"-wal", "-shm", "-journal"} {
		if !strings.HasSuffix(suffix, marker) {
			continue
		}
		switch base := suffix[:len(suffix)-len(marker)]; base {
		case ".db", ".sqlite", ".sqlite3":
			return base, true
		}
	}
	return "", false
}

func effectiveSuffix(path string) string {
	suffix := lowerSuffix(path)
	if base, ok := sidecarBase(suffix); ok {
		return base
	}
	return suffix
}

func IsSQLiteSidecar(path string) bool {
	_, ok := sidecarBase(lowerSuffix(path))
	return ok
}

func HasBinaryExtension(path string) bool {
	return isBinaryExtension(effectiveSuffix(path))
}

func HasOpaqueDocumentExtension(path string) bool {
	return isOpaqueDocumentExtension(effectiveSuffix(path))
}

func IsPDFPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".pdf")
}

var prunedDirs = [...]string{
	".git", ".hg", ".svn",
	"node_modules", "venv", ".venv", "site-packages", "dist-packages",
	"vendor", "third_party",
	"build", "dist", "target", "out", "coverage",
	".next", ".turbo", ".parcel-cache", ".nuxt", ".svelte-kit",
	".archive",
	"__pycache__", ".cache", ".Trash", ".tox", ".nox", ".mypy_cache",
	".pytest_cache", ".ruff_cache", ".npm", ".yarn", ".pnpm-store",
	".gradle", ".m2", ".nuget",
	"backups", "backup", ".backups",
}

func IsPrunedDir(name string) bool {
	for _, dir := range prunedDirs {
		if dir == name {
			return true
		}
	}
	return false
}

func PrunedDirNames() []string {
	names := append([]string(nil), prunedDirs[:]...)
	sort.Strings(names)
	return names
}
