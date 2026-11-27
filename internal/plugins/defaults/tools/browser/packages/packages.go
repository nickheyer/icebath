// The Chromium the browser tools and the Bot Desktop run: Chrome for Testing, or Playwright's mirror where it builds none, laid out the way
// Playwright finds its browsers
package packages

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/browser"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/pkg/fsx"
)

//go:embed lock.json
var lock []byte

// Where the builds come from: Chrome for Testing by its platform name, Playwright's own builds where it has none
const cdn = "https://cdn.playwright.dev"

var (
	testing = map[string]string{"linux-x64": "linux64", "darwin-x64": "mac-x64", "darwin-arm64": "mac-arm64", "win32-x64": "win64", "win32-arm64": "win64"}
	mirror  = map[string]string{"linux-arm64": "linux-arm64"}
)

// File Playwright writes into a browser it finished installing
const complete = "INSTALLATION_COMPLETE"

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Tools: []packages.Package{chromium(packages.Pins(lock)[browser.Chromium])}})
}

// Version is the Playwright revision, a plus, and the Chrome for Testing version
func chromium(pin packages.Pin) packages.Package {
	gaps := map[string]string{packages.Bionic: "no Chromium build for Android/Termux"}
	for target := range packages.Musl {
		gaps[target] = "Playwright/Chrome-for-Testing publishes no musl build"
	}
	return packages.Package{
		Name: browser.Chromium, Optional: true, Default: true, OffPath: true, Gaps: gaps, Pin: pin,
		// Windows on Arm runs the x64 build
		Emulated: []string{"win32-arm64"},
		// The archive's own folder is what Playwright looks for under the entry
		Nested: true,
		Entry: func(version, _ string) string {
			revision, _, _ := strings.Cut(version, "+")
			return browser.Chromium + "-" + revision
		},
		URLs: func(version, target string) ([]string, error) {
			revision, chrome, _ := strings.Cut(version, "+")
			if platform := testing[target]; platform != "" && chrome != "" {
				return []string{fmt.Sprintf("%s/builds/cft/%s/%s/chrome-%s.zip", cdn, chrome, platform, platform)}, nil
			}
			if platform, ok := mirror[target]; ok {
				return []string{fmt.Sprintf("%s/dbazure/download/playwright/builds/chromium/%s/chromium-%s.zip", cdn, revision, platform)}, nil
			}
			return nil, fmt.Errorf("no download for target %s", target)
		},
		Locate: executable,
		Stage: func(b *packages.Build) error {
			return os.WriteFile(filepath.Join(b.Dir, complete), nil, 0o644)
		},
		Verify: func(entry, target string, standard func() string) string {
			if !fsx.IsFile(filepath.Join(entry, complete)) {
				return fmt.Sprintf("%s missing under %s", complete, entry)
			}
			if executable(entry, target) == "" {
				return "Chromium executable missing under " + entry
			}
			return standard()
		},
		// Playwright finds the browser by the store folder that holds the entry
		Env: func(entry, _ string) packages.Env {
			return packages.Env{Vars: map[string]string{"PLAYWRIGHT_BROWSERS_PATH": filepath.Dir(entry)}}
		},
	}
}

// First browser program under the entry in path order, whichever folder the archive put it in, never entering a folder that is a link
func executable(entry, target string) string {
	names := []string{"chrome", "chromium", "Google Chrome for Testing", "Chromium"}
	if strings.HasPrefix(target, "win32") {
		names = []string{"chrome.exe"}
	}
	var found string
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if e.Type().IsRegular() && slices.Contains(names, e.Name()) {
				found = path
				return true
			}
			if e.IsDir() && walk(path) {
				return true
			}
		}
		return false
	}
	walk(entry)
	return found
}
