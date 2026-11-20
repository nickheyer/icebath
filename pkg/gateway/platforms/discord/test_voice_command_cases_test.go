package discord

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVoiceReceiverFfmpegResolverFindsWingetInstallWhenNotOnPath(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := filepath.Join(tmp, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe", "ffmpeg-7.1-full_build", "bin", "ffmpeg.exe")
	if err := os.MkdirAll(filepath.Dir(ffmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffmpeg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_PATH", "")
	t.Setenv("LOCALAPPDATA", tmp)
	prev := sharedFindFFmpeg
	sharedFindFFmpeg = func() string { return "" }
	t.Cleanup(func() { sharedFindFFmpeg = prev })
	if got := resolveFFmpegExecutable(); got != ffmpeg {
		t.Fatalf("resolved %q", got)
	}
}
