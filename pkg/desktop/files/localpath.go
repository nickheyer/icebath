package files

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	localFilesystemPathRE = regexp.MustCompile(`^(?:/[^/]|/$|///|~/|[a-zA-Z]:[\\/]|\\\\)`)
	protocolRelativeRE    = regexp.MustCompile(`^//[^/\s]`)
)

// True for POSIX, tilde, Windows drive and UNC paths, never a protocol relative URL
func LooksLikeLocalFilesystemPath(value string) bool {
	return localFilesystemPathRE.MatchString(value)
}

// A protocol relative URL opened as https, everything else unchanged
func AbsolutizeProtocolRelativeURL(value string) string {
	if protocolRelativeRE.MatchString(value) {
		return "https:" + value
	}
	return value
}

// WSL bridge every file read boundary routes its raw path through
var localReadPath = func(p string) string { return ResolveLocalReadPath(p, "", nil) }

// Media stream path, already decoded by the protocol handler, only bridged
func ResolveMediaStreamFile(filePath any) string { return localReadPath(jsStringOrEmpty(filePath)) }

// File read IPC path, bridged
func ResolveIpcFileReadPath(filePath any) string { return localReadPath(jsStringOrEmpty(filePath)) }

// Preview target, file URLs untouched, plain targets tilde expanded, then bridged
func ResolvePreviewTargetPath(rawTarget string, expandUserPath func(string) string) string {
	if len(rawTarget) >= 5 && strings.EqualFold(rawTarget[:5], "file:") {
		return localReadPath(rawTarget)
	}
	return localReadPath(expandUserPath(rawTarget))
}

// Tilde expansion against the user home, the main process expandUserPath
func ExpandUserPath(filePath string) string {
	value := strings.TrimSpace(filePath)
	if value == "~" {
		return userHome()
	}
	if strings.HasPrefix(value, "~"+string(os.PathSeparator)) || strings.HasPrefix(value, "~/") {
		return filepath.Join(userHome(), value[2:])
	}
	return value
}
