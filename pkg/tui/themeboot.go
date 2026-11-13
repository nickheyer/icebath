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

// On disk boot theme, version 1
type bootThemeFile struct {
	Background string `json:"background,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Theme      *theme `json:"theme,omitempty"`
	Version    int    `json:"version"`
}

// Boot theme read at startup and whether it seeded the theme pin
type bootState struct {
	theme     *theme
	seededPin bool
}

var (
	bootOnce         sync.Once
	boot             bootState
	seededBackground string
	bootWriteMu      sync.Mutex
	bootWriteTimer   *time.Timer
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
		Version    any             `json:"version"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Version != float64(1) || !looksLikeTheme(raw.Theme) {
		return bootThemeFile{}, false
	}
	var t theme
	if json.Unmarshal(raw.Theme, &t) != nil {
		return bootThemeFile{}, false
	}
	out := bootThemeFile{Theme: &t, Version: 1}
	out.Background, _ = raw.Background.(string)
	if mode, _ := raw.Mode.(string); mode == "light" || mode == "dark" {
		out.Mode = mode
	}
	return out, true
}

// Seeds the theme pin and background hint from the boot file unless the environment already pins a mode
func seedBootEnvironment(b bootThemeFile, found bool) bool {
	seededBackground = ""
	if !found || os.Getenv("ICEBATH_TUI_THEME") != "" || os.Getenv("ICEBATH_TUI_LIGHT") != "" {
		return false
	}
	seededPin := false
	if b.Mode != "" {
		_ = os.Setenv("ICEBATH_TUI_THEME", b.Mode)
		seededPin = true
	}
	if b.Background != "" && strings.ToLower(b.Background) != "#000000" && os.Getenv("ICEBATH_TUI_BACKGROUND") == "" {
		_ = os.Setenv("ICEBATH_TUI_BACKGROUND", b.Background)
		seededBackground = b.Background
	}
	return seededPin
}

// Boot theme and seeded pin, read and seeded once when the TUI starts
func loadBootTheme() bootState {
	bootOnce.Do(func() {
		b, found := readBootTheme()
		boot = bootState{theme: b.Theme, seededPin: seedBootEnvironment(b, found)}
	})
	return boot
}

// Drops a boot seeded background the terminal contradicted, true when it was the seeded one
func invalidateBootBackground() bool {
	if seededBackground == "" || os.Getenv("ICEBATH_TUI_BACKGROUND") != seededBackground {
		return false
	}
	_ = os.Unsetenv("ICEBATH_TUI_BACKGROUND")
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
		if enc.Encode(bootThemeFile{Background: background, Mode: mode, Theme: &t, Version: 1}) != nil {
			return
		}
		path := bootFilePath()
		tmp := path + ".tmp"
		if os.WriteFile(tmp, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), 0o644) == nil {
			_ = os.Rename(tmp, path)
		}
	})
}
