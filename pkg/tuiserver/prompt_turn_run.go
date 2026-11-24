package tuiserver

import (
	"path/filepath"
)

// Crash log of the turn dispatcher under the launch home
func crashLogPath() string { return filepath.Join(launchHome(), "logs", "tui_gateway_crash.log") }
