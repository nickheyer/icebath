package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"

	"golang.org/x/text/cases"
)

var windowsServiceExecutable = os.Executable

// Directories an Icebath owned SCM service binary lives under, the install tree, the binary dir and the launcher dir
func IcebathServiceRoots(ctx context.Context) []string {
	exeDir := ""
	if exe, err := windowsServiceExecutable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	return []string{pm.RepoRoot(), exeDir, filepath.Join(home.Dir(ctx), "gateway-service")}
}

// True for an SCM service named icebath or whose binary path starts under one of the roots
func IcebathOwnsWindowsService(name, binpath string, roots []string) bool {
	var normalized strings.Builder
	for _, char := range cases.Fold().String(name) {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			normalized.WriteRune(char)
		}
	}
	if strings.HasPrefix(normalized.String(), "icebath") {
		return true
	}
	candidate := normalizeWindowsPath(binpath)
	for _, root := range roots {
		if root != "" && strings.HasPrefix(candidate, normalizeWindowsPath(root)+"/") {
			return true
		}
	}
	return false
}
