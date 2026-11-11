package dashboard

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Path prefixes that are files rather than client routes, a miss answering 404 not the SPA shell
var staticAssetPrefixes = []string{"assets/", "ds-assets/", "fonts/", "fonts-terminal/"}

// Root page icons that answer without sign-in
var staticAssetFiles = map[string]bool{"apple-touch-icon.png": true, "favicon.ico": true}

var publicFontDirs = map[string]bool{"fonts": true, "fonts-terminal": true}

// Web public directory the dashboard build copies fonts from
func webPublicDir() string { return filepath.Join(pm.RepoRoot(), "web", "public") }

// Whether a SPA path names a static file whose miss is a 404
func IsStaticAssetPath(fullPath string) bool {
	if staticAssetFiles[fullPath] {
		return true
	}
	for _, p := range staticAssetPrefixes {
		if strings.HasPrefix(fullPath, p) {
			return true
		}
	}
	return false
}

// Public font from the served dist, the packaged dashboard dist, or web public, empty when none has it
func PublicFontFile(dist, dashboardDist, fullPath string) string {
	folder, name, _ := strings.Cut(fullPath, "/")
	if !publicFontDirs[folder] || name == "" {
		return ""
	}
	for _, root := range []string{filepath.Join(dist, folder), filepath.Join(dashboardDist, folder), filepath.Join(webPublicDir(), folder)} {
		candidate := filepath.Join(root, name)
		if st, err := os.Stat(candidate); err != nil || !st.Mode().IsRegular() {
			continue
		}
		resolved, err1 := pypath.Resolve(candidate)
		rootResolved, err2 := pypath.Resolve(root)
		if err1 != nil || err2 != nil {
			continue
		}
		if rel, err := filepath.Rel(rootResolved, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return candidate
		}
	}
	return ""
}
