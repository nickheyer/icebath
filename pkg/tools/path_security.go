package tools

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Control characters and Unicode line separators that corrupt line delimited protocols and forge log lines
var unsafePathCharsRe = regexp.MustCompile("[\x00-\x1f\x7f\u0085  ]")

// Reports whether a path holds control characters or line separators
func hasUnsafePathChars(path string) bool {
	return unsafePathCharsRe.MatchString(path)
}

// HasUnsafePathChars reports control characters or line separators in a path
func HasUnsafePathChars(path string) bool { return hasUnsafePathChars(path) }

// Error text when a path does not resolve inside root with symlinks and dot dot followed, empty otherwise
func ValidateWithinDir(path, root string) string {
	resolved := realPath(path)
	rootResolved := realPath(root)
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return "Path escapes allowed directory: " + err.Error()
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "Path escapes allowed directory: '" + resolved + "' is not in the subpath of '" + rootResolved + "'"
	}
	return ""
}
