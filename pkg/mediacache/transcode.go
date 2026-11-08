package mediacache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Encodes the file to voip tuned Ogg Opus through ffmpeg, empty when ffmpeg is missing or the encode fails
func TranscodeToOggOpus(ctx context.Context, path, bitrate string, timeout time.Duration, outputPath string) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", nil
	}
	oggPath := outputPath
	if oggPath == "" {
		f, err := os.CreateTemp("", "voice_transcode_*.ogg")
		if err != nil {
			return "", err
		}
		oggPath = f.Name()
		f.Close()
	}
	absIn, _ := filepath.Abs(path)
	absOut, _ := filepath.Abs(oggPath)
	inPlace := absIn == absOut
	workPath := oggPath
	if inPlace {
		workPath = oggPath + ".tmp.ogg"
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, ffmpeg, "-v", "error", "-y", "-i", path, "-acodec", "libopus", "-ac", "1", "-b:a", bitrate,
		"-vbr", "on", "-application", "voip", "-compression_level", "10", "-f", "ogg", workPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if transcodeSucceeded(path, oggPath, workPath, inPlace, runErr, runCtx.Err() != nil, stderr.Bytes()) {
		return oggPath, nil
	}
	os.Remove(workPath)
	return "", nil
}

// Reports a non empty encode moved into place, logging the ffmpeg failure or the raised error otherwise
func transcodeSucceeded(path, oggPath, workPath string, inPlace bool, runErr error, timedOut bool, stderr []byte) bool {
	var exitErr *exec.ExitError
	if runErr != nil && (timedOut || !errors.As(runErr, &exitErr)) {
		slog.Warn(fmt.Sprintf("voice transcode to Ogg/Opus failed for %s", path), "error", runErr)
		return false
	}
	if runErr == nil {
		info, err := os.Stat(workPath)
		if err == nil && info.Size() > 0 && inPlace {
			err = os.Rename(workPath, oggPath)
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("voice transcode to Ogg/Opus failed for %s", path), "error", err)
			return false
		}
		if info.Size() > 0 {
			return true
		}
	}
	code := 0
	if exitErr != nil {
		code = exitErr.ExitCode()
	}
	detail := []rune(strings.ToValidUTF8(string(stderr), "\uFFFD"))
	if len(detail) > 500 {
		detail = detail[:500]
	}
	slog.Warn(fmt.Sprintf("ffmpeg Ogg/Opus transcode of %s failed (returncode=%d): %s", path, code, string(detail)))
	return false
}
