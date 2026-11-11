package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Logger carrying the record name of the hermes runner module
func inboundLog() *slog.Logger { return slog.Default().With("logger", "gateway.run") }

// Regular file check of pathlib, ignored stat errors read as false and others returned
func inboundIsFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if pypath.PathlibIgnoredError(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// Moves adapter cached attachments into the active profile cache and repoints the event and its baked notes
func rehomeInboundMedia(ctx context.Context, event *gateway.MessageEvent) {
	if len(event.MediaURLs) == 0 {
		return
	}
	active, launch := home.Dir(ctx), home.RoutingProcess()
	if home.Key(active) == home.Key(launch) {
		return
	}
	rewritten := append([]string(nil), event.MediaURLs...)
	for i, raw := range event.MediaURLs {
		rel, ok := pypath.PathRelativeTo(raw, filepath.Join(launch, "cache"))
		if !ok {
			continue
		}
		dest := filepath.Join(active, "cache", rel)
		isFile, err := inboundIsFile(raw)
		if err == nil && !isFile {
			continue
		}
		if err == nil {
			err = os.MkdirAll(filepath.Dir(dest), 0o777)
		}
		if err == nil {
			_, err = pyfs.Move(raw, dest)
		}
		if err != nil {
			inboundLog().Warn(fmt.Sprintf("Could not move inbound attachment %s into the routed profile's cache", raw), "exc_info", err)
			continue
		}
		rewritten[i] = dest
		if event.Text != "" && strings.Contains(event.Text, raw) {
			event.Text = strings.ReplaceAll(event.Text, raw, tools.ToAgentVisibleCachePath(inboundToolsEnv(ctx), dest))
		}
	}
	event.MediaURLs = rewritten
}
