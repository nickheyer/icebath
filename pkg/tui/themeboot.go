package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const bootWriteDelay = 400 * time.Millisecond

// File of the last session's resolved theme that paints the first frame
func bootFilePath() string { return filepath.Join(home.Process(), "tui-theme-boot.json") }

// On disk boot theme
type bootThemeFile struct {
	Background string `json:"background,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Theme      *theme `json:"theme,omitempty"`
}

// Boot theme read at startup and whether it seeded the theme pin
type bootState struct {
	theme     *theme
	seededPin bool
}

var (
	bootOnce sync.Once
	boot     bootState
	// Mode and background hint the boot file seeded under the terminal's own environment, each dropped once something contradicts it
	seededMode, seededBackground string
	seedMu                       sync.Mutex
	bootWriteMu                  sync.Mutex
	bootWriteTimer               *time.Timer
)

// Whether a decoded theme carries the color and brand fields the TUI needs
func looksLikeTheme(raw json.RawMessage) bool {
	var probe struct {
		Color map[string]any `json:"color"`
		Brand map[string]any `json:"brand"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Color == nil || probe.Brand == nil {
		return false
	}
	_, textOK := probe.Color["text"].(string)
	_, primaryOK := probe.Color["primary"].(string)
	_, nameOK := probe.Brand["name"].(string)
	return textOK && primaryOK && nameOK
}

// Boot theme of the last session, false outside a real run or when the file is missing or malformed
func readBootTheme() (bootThemeFile, bool) {
	if testing.Testing() {
		return bootThemeFile{}, false
	}
	data, err := os.ReadFile(bootFilePath())
	if err != nil {
		return bootThemeFile{}, false
	}
	var raw struct {
		Background any             `json:"background"`
		Mode       any             `json:"mode"`
		Theme      json.RawMessage `json:"theme"`
	}
	if json.Unmarshal(data, &raw) != nil || !looksLikeTheme(raw.Theme) {
		return bootThemeFile{}, false
	}
	var t theme
	if json.Unmarshal(raw.Theme, &t) != nil {
		return bootThemeFile{}, false
	}
	out := bootThemeFile{Theme: &t}
	out.Background, _ = raw.Background.(string)
	if mode, _ := raw.Mode.(string); mode == "light" || mode == "dark" {
		out.Mode = mode
	}
	return out, true
}

// Seeds the theme pin: the light or dark theme the launch resolved, else the boot file's mode and background hint
func seedBootEnvironment(b bootThemeFile, found bool) bool {
	seedMu.Lock()
	defer seedMu.Unlock()
	seededMode, seededBackground = "", ""
	if pin := prefs.Theme; pin == "light" || pin == "dark" {
		seededMode = pin
		return true
	}
	if !found {
		return false
	}
	seededMode = b.Mode
	if b.Background != "" && strings.ToLower(b.Background) != "#000000" && os.Getenv("ICEBATH_TUI_BACKGROUND") == "" {
		seededBackground = b.Background
	}
	return seededMode != ""
}

// Value of a theme detection input before the window worked anything out itself: the mode pin seeded at boot, the terminal's background
// hint from its environment or the boot file, and the terminal's other variables
func bootThemeEnv(name string) string {
	loadBootTheme()
	switch name {
	case "ICEBATH_TUI_THEME":
		seedMu.Lock()
		defer seedMu.Unlock()
		return seededMode
	case "ICEBATH_TUI_BACKGROUND":
		if v := os.Getenv(name); v != "" {
			return v
		}
		seedMu.Lock()
		defer seedMu.Unlock()
		return seededBackground
	}
	return os.Getenv(name)
}

// Drops the mode seeded at boot, once the configured theme no longer pins one
func dropBootMode() {
	seedMu.Lock()
	seededMode = ""
	seedMu.Unlock()
}

// Boot theme and seeded pin, read and seeded once when the TUI starts
func loadBootTheme() bootState {
	bootOnce.Do(func() {
		b, found := readBootTheme()
		boot = bootState{theme: b.Theme, seededPin: seedBootEnvironment(b, found)}
	})
	return boot
}

// Drops a boot seeded background the terminal contradicted, true when current, the background the detection reads, was the seeded one
func invalidateBootBackground(current string) bool {
	seedMu.Lock()
	defer seedMu.Unlock()
	if seededBackground == "" || current != seededBackground {
		return false
	}
	seededBackground = ""
	return true
}

// Writes the resolved theme for the next launch's first frame, debounced and atomic
func writeBootTheme(t theme, background, mode string) {
	if testing.Testing() {
		return
	}
	bootWriteMu.Lock()
	defer bootWriteMu.Unlock()
	if bootWriteTimer != nil {
		bootWriteTimer.Stop()
	}
	bootWriteTimer = time.AfterFunc(bootWriteDelay, func() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(bootThemeFile{Background: background, Mode: mode, Theme: &t}) != nil {
			return
		}
		path := bootFilePath()
		tmp := path + ".tmp"
		if os.WriteFile(tmp, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), 0o644) == nil {
			_ = os.Rename(tmp, path)
		}
	})
}
