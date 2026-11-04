package tools

import "strings"

// Images, video, audio, archives, executables, documents, fonts, bytecode, databases, design files
var binaryExtensions = setOf(
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

// Container documents a plain text write can never produce validly
var opaqueDocumentExtensions = setOf(
	".doc", ".docx", ".docm", ".xls", ".xlsx", ".xlsm", ".xlsb",
	".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
	".odt", ".ods", ".odp", ".rtf", ".epub",
)

var sqliteSidecarMarkers = []string{"-wal", "-shm", "-journal"}

var sqliteExtensions = setOf(".db", ".sqlite", ".sqlite3")

// Builds a string set from its arguments
func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// Lower cased final suffix of a path, empty when there is no dot
func lowerSuffix(path string) string {
	dot := strings.LastIndex(path, ".")
	if dot == -1 {
		return ""
	}
	return strings.ToLower(path[dot:])
}

// Database suffix a sidecar suffix hangs off, empty when not a SQLite sidecar
func stripSidecarMarker(suffix string) string {
	for _, marker := range sqliteSidecarMarkers {
		if strings.HasSuffix(suffix, marker) {
			base := suffix[:len(suffix)-len(marker)]
			if sqliteExtensions[base] {
				return base
			}
		}
	}
	return ""
}

// Case insensitive membership of the final suffix, a sidecar counting as its database
func hasExtensionIn(path string, extensions map[string]bool) bool {
	suffix := lowerSuffix(path)
	if base := stripSidecarMarker(suffix); base != "" {
		suffix = base
	}
	return extensions[suffix]
}

// Reports whether a path names a SQLite wal, shm or journal sidecar
func isSqliteSidecar(path string) bool {
	return stripSidecarMarker(lowerSuffix(path)) != ""
}

// Reports whether the final suffix is a known binary extension
func hasBinaryExtension(path string) bool {
	return hasExtensionIn(path, binaryExtensions)
}

// Reports whether the final suffix is an opaque document container
func hasOpaqueDocumentExtension(path string) bool {
	return hasExtensionIn(path, opaqueDocumentExtensions)
}

// Reports whether the path ends in .pdf
func isPDFPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".pdf")
}
