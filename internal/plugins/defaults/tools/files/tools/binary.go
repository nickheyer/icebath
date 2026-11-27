package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Images, video, audio, archives, executables, documents, fonts, bytecode, databases and design files
var binaryExtensions = strs.Set(
	".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".webp", ".tiff", ".tif",
	".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv", ".flv", ".m4v", ".mpeg", ".mpg",
	".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a", ".wma", ".aiff", ".opus",
	".zip", ".tar", ".gz", ".bz2", ".7z", ".rar", ".xz", ".z", ".tgz", ".iso",
	".exe", ".dll", ".so", ".dylib", ".bin", ".o", ".a", ".obj", ".lib", ".app", ".msi", ".deb", ".rpm",
	".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".odt", ".ods", ".odp",
	".ttf", ".otf", ".woff", ".woff2", ".eot",
	".pyc", ".pyo", ".class", ".jar", ".war", ".ear", ".node", ".wasm", ".rlib",
	".sqlite", ".sqlite3", ".db", ".mdb", ".idx",
	".psd", ".ai", ".eps", ".sketch", ".fig", ".xd", ".blend", ".3ds", ".max",
	".swf", ".fla", ".lockb", ".dat", ".data",
)

// Container documents a plain text write can never make validly
var opaqueDocuments = strs.Set(".doc", ".docx", ".docm", ".xls", ".xlsx", ".xlsm", ".xlsb", ".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
	".odt", ".ods", ".odp", ".rtf", ".epub")

// Images read_file sends to vision_analyze
var imageExtensions = strs.Set(".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico")

var sqliteExtensions = strs.Set(".db", ".sqlite", ".sqlite3")

// Lower case suffix from the last dot of the path, empty with no dot
func lastSuffix(path string) string {
	if dot := strings.LastIndex(path, "."); dot >= 0 {
		return strings.ToLower(path[dot:])
	}
	return ""
}

// Database suffix a SQLite wal, shm or journal suffix hangs off, empty for any other
func sidecarBase(suffix string) string {
	for _, marker := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(suffix, marker); ok && sqliteExtensions[base] {
			return base
		}
	}
	return ""
}

// Whether the path's suffix is in the set, a SQLite sidecar counting as its database
func suffixIn(path string, extensions map[string]bool) bool {
	suffix := lastSuffix(path)
	if base := sidecarBase(suffix); base != "" {
		suffix = base
	}
	return extensions[suffix]
}

func binaryExtension(path string) bool { return suffixIn(path, binaryExtensions) }

func opaqueDocument(path string) bool { return suffixIn(path, opaqueDocuments) }

func sqliteSidecar(path string) bool { return sidecarBase(lastSuffix(path)) != "" }

func pdf(path string) bool { return strings.HasSuffix(strings.ToLower(path), ".pdf") }

func image(path string) bool { return imageExtensions[ext(path)] }

// Whether bytes are binary: a NUL, or invalid UTF-8 anywhere but a sequence cut at the very end
func binaryBytes(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	for _, c := range sample {
		if c == 0 {
			return true
		}
	}
	for i := 0; i < len(sample); {
		r, size := utf8.DecodeRune(sample[i:])
		if r == utf8.RuneError && size == 1 {
			return i < len(sample)-3
		}
		i += size
	}
	return false
}

// Whether text a command printed looks binary: by the extension, a replacement character, or controls in more than 30 percent of it
func binaryText(path, sample string) bool {
	if binaryExtension(path) {
		return true
	}
	if sample == "" {
		return false
	}
	head := []rune(sample)
	total := len(head)
	head = head[:min(len(head), 1000)]
	controls := 0
	for _, c := range head {
		if c == utf8.RuneError {
			return true
		}
		if c < 32 && c != '\n' && c != '\r' && c != '\t' {
			controls++
		}
	}
	return float64(controls)/float64(min(total, 1000)) > 0.30
}

// One line refusal of a binary file naming its kind and size
func describeBinary(sample []byte, size int64) string {
	kind := tools.Identify(sample)
	if kind == "" {
		kind = "unknown binary"
	}
	var shown string
	switch {
	case size >= 1024*1024:
		shown = fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	case size >= 1024:
		shown = fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		shown = fmt.Sprintf("%d bytes", size)
	}
	return "Binary file (" + kind + ", " + shown + ") — cannot display as text."
}

// UTF-16 a sample is by its byte order mark or where its zero bytes sit, empty when it is not
func utf16Of(sample []byte) string {
	switch {
	case len(sample) >= 2 && sample[0] == 0xfe && sample[1] == 0xff:
		return "utf-16-be"
	case len(sample) >= 2 && sample[0] == 0xff && sample[1] == 0xfe:
		return "utf-16-le"
	}
	odd, even := 0, 0
	for i, c := range sample {
		if c != 0 {
			continue
		}
		if i%2 == 1 {
			odd++
		} else {
			even++
		}
	}
	switch {
	case even == 0 && odd >= 2:
		return "utf-16-le"
	case odd == 0 && even >= 2:
		return "utf-16-be"
	}
	return ""
}

// UTF-16 bytes as text, a replacement character for each bad unit and for a cut tail
func decodeUTF16(data []byte, big bool) string {
	unit := func(i int) int {
		if big {
			return int(data[i])<<8 | int(data[i+1])
		}
		return int(data[i+1])<<8 | int(data[i])
	}
	var b strings.Builder
	i := 0
	for i+1 < len(data) {
		u := unit(i)
		switch {
		case u >= 0xd800 && u < 0xdc00:
			if i+3 >= len(data) {
				b.WriteRune(utf8.RuneError)
				return b.String()
			}
			if low := unit(i + 2); low >= 0xdc00 && low < 0xe000 {
				b.WriteRune(rune(0x10000 + (u-0xd800)<<10 + (low - 0xdc00)))
				i += 4
				continue
			}
			b.WriteRune(utf8.RuneError)
		case u >= 0xdc00 && u < 0xe000:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(rune(u))
		}
		i += 2
	}
	if i < len(data) {
		b.WriteRune(utf8.RuneError)
	}
	return b.String()
}

// Page of a UTF-16 file as UTF-8 with its line count and encoding, false when it is not UTF-16
func utf16Page(data []byte, offset, limit int) (string, int, string, bool) {
	encoding := utf16Of(data[:min(len(data), 512)])
	if encoding == "" {
		return "", 0, "", false
	}
	text := strings.TrimPrefix(decodeUTF16(data, encoding == "utf-16-be"), "\ufeff")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lo, hi := min(offset-1, len(lines)), min(offset-1+limit, len(lines))
	return strings.Join(lines[lo:hi], "\n"), len(lines), encoding, true
}
