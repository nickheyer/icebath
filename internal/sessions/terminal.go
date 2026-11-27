package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// How long a terminal's breadcrumb names the session to continue there
const breadcrumbAge = 30 * 24 * time.Hour

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Variables a terminal multiplexer or emulator names its pane or window by, first set first
var paneVariables = []string{"ZELLIJ_PANE_ID", "TMUX_PANE", "KITTY_WINDOW_ID", "WEZTERM_PANE", "TERM_SESSION_ID", "WT_SESSION"}

// Name of the terminal this process runs in: the device of its input or output, else the pane its multiplexer or emulator names;
// empty when it runs in none
func TerminalID() string {
	for _, fd := range []string{"/proc/self/fd/0", "/proc/self/fd/1"} {
		if target, err := os.Readlink(fd); err == nil && (strings.HasPrefix(target, "/dev/pts/") || strings.HasPrefix(target, "/dev/tty")) {
			return clean("tty-" + strings.Trim(target, "/"))
		}
	}
	for _, name := range paneVariables {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return clean(strings.ToLower(name) + "-" + value)
		}
	}
	return ""
}

func clean(name string) string {
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), "/")
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// Breadcrumb a terminal keeps of the session it showed last
type Breadcrumb struct {
	SessionID string  `json:"session_id"`
	Directory string  `json:"cwd"`
	At        float64 `json:"ts"`
}

func breadcrumbs(dir string) string {
	return filepath.Join(dir, "terminal-sessions")
}

// Records the session this terminal shows, so continuing here picks it up, and drops breadcrumbs gone stale
func Remember(dir, id, directory string) error {
	terminal := TerminalID()
	if terminal == "" || id == "" {
		return nil
	}
	folder := breadcrumbs(dir)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(Breadcrumb{SessionID: id, Directory: directory, At: float64(time.Now().UnixNano()) / 1e9})
	if err != nil {
		return err
	}
	path := filepath.Join(folder, terminal)
	if err := fsx.Write(path, data, 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > breadcrumbAge {
			os.Remove(filepath.Join(folder, entry.Name()))
		}
	}
	return nil
}

// Session this terminal showed last, false when it has no breadcrumb younger than a month
func Remembered(dir string) (Breadcrumb, bool) {
	terminal := TerminalID()
	if terminal == "" {
		return Breadcrumb{}, false
	}
	data, err := os.ReadFile(filepath.Join(breadcrumbs(dir), terminal))
	if err != nil {
		return Breadcrumb{}, false
	}
	var crumb Breadcrumb
	if json.Unmarshal(data, &crumb) != nil || crumb.SessionID == "" {
		return Breadcrumb{}, false
	}
	at := time.Unix(0, int64(crumb.At*1e9))
	return crumb, time.Since(at) < breadcrumbAge
}
