package logging

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/linktier"
)

// Known log files by the name icebath logs takes
var LogFiles = map[string]string{
	"agent": "agent.log", "errors": "errors.log", "gateway": "gateway.log", "update": "update.log", "mcp": "mcp-stderr.log",
}

// Logs the root home holds whatever profile is active, in name order
var RootHomeLogs = []string{"update"}

// Tier whose code writes a log, update.log being the self updater's, absent for core logs
var LogTiers = map[string]string{"update": linktier.Extras}

// Where a named log lives without checking it exists, false for an unknown name
func LogFilePath(ctx context.Context, name string) (string, bool) {
	filename, ok := LogFiles[name]
	if !ok {
		return "", false
	}
	h := home.Dir(ctx)
	for _, root := range RootHomeLogs {
		if root == name {
			h = home.Root()
		}
	}
	return filepath.Join(h, "logs", filename), true
}
