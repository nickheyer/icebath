package tools

import (
	"errors"
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

// Resolves a model supplied path against the session base directory after rejecting unsafe characters
func ResolveSafePath(env *Env, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path must not be empty")
	}
	if hasUnsafePathChars(path) {
		return "", errors.New("path contains control characters or line separators; use a plain filesystem path")
	}
	if msg := ntNamespaceError(path, "Access"); msg != "" {
		return "", errors.New(msg)
	}
	return resolvePathForTask(env, path), nil
}
