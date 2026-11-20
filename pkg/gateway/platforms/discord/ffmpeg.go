package discord

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
)

// Repo wide ffmpeg discovery, empty when not found
var sharedFindFFmpeg = media.FindFFmpegBinary

// Ffmpeg command honoring FFMPEG_PATH, the shared discovery and Windows winget installs
func resolveFFmpegExecutable() string {
	if explicit := pystr.Strip(os.Getenv("FFMPEG_PATH")); explicit != "" {
		return pypath.Expandvars(pypath.Expanduser(explicit))
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
