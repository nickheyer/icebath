package logging

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
)

// Known log files by the name icebath logs takes
var LogFiles = map[string]string{
	"agent": "agent.log", "errors": "errors.log", "gateway": "gateway.log", "gui": "gui.log", "desktop": "desktop.log",
	"update": "update.log", "handoff": "desktop-update-handoff.log", "mcp": "mcp-stderr.log",
}

// Logs the root home holds whatever profile is active
var rootHomeLogs = map[string]bool{"update": true, "handoff": true}

// Where a named log lives without checking it exists, false for an unknown name
func LogFilePath(ctx context.Context, name string) (string, bool) {
	filename, ok := LogFiles[name]
	if !ok {
		return "", false
	}
	h := home.Dir(ctx)
	if rootHomeLogs[name] {
		h = home.Root()
	}
	return filepath.Join(h, "logs", filename), true
}
