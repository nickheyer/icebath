package discord

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/settings"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Repo wide ffmpeg discovery, empty when not found
var sharedFindFFmpeg = media.FindFFmpegBinary

// Ffmpeg command from discord.ffmpeg_path, else the shared discovery and Windows winget installs
func resolveFFmpegExecutable(ctx context.Context) string {
	if explicit := strings.TrimSpace(settings.DiscordFfmpegPath.Get(gateway.SettingsContext(ctx))); explicit != "" {
		return pathutil.ExpandEnv(pathutil.ExpandUser(explicit))
	}
	if discovered := sharedFindFFmpeg(); discovered != "" {
		return discovered
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		candidates, _ := filepath.Glob(filepath.Join(localAppData, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg_*", "*", "bin", "ffmpeg.exe"))
		if len(candidates) > 0 {
			sort.Strings(candidates)
			return candidates[len(candidates)-1]
		}
	}
	return "ffmpeg"
}
