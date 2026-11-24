package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Regular file check of pathlib, ignored stat errors read as false and others returned
func inboundIsFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if pathutil.IsMissing(err) {
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
		rel, ok := pathutil.RelativeTo(raw, filepath.Join(launch, "cache"))
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
			_, err = fsutil.Move(raw, dest)
		}
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("Could not move inbound attachment %s into the routed profile's cache", raw), "exc_info", err)
			continue
		}
		rewritten[i] = dest
		if event.Text != "" && strings.Contains(event.Text, raw) {
			event.Text = strings.ReplaceAll(event.Text, raw, tools.ToAgentVisibleCachePath(inboundToolsEnv(ctx), dest))
		}
	}
	event.MediaURLs = rewritten
}
