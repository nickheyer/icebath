package config

import (
	"context"
	"os"
	"runtime"
	"strings"

	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
)

// Dotenv lines decoded as utf-8-sig with replacement, each normalized and ending in one newline
func ReadEnvLines(path string) ([]string, error) { return readSanitizedEnvLines(path) }

// Replaces a dotenv file with lines verbatim, keeping its mode when asked or securing it under the process home
func WriteEnvLines(ctx context.Context, path string, lines []string, preserveMode bool) error {
	return writeEnvLinesMode(ctx, "", path, lines, preserveMode)
}

// Reports whether a dotenv line assigns key in exactly the shapes the env loader parses
func EnvLineDefinesKey(line, key string) bool { return envLineDefines(line, key) }

// Replaces a dotenv file through a staged temp beside its resolved target, keeping the prior mode or securing it
func writeEnvLinesMode(ctx context.Context, h, path string, lines []string, preserveMode bool) error {
	var mode os.FileMode
	keep := false
	if preserveMode {
		if info, err := os.Stat(path); err == nil {
			mode, keep = info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky), true
		}
	}
	tmp, err := fsutil.CreateTempBeside(path, ".env_", ".tmp")
	if err != nil {
		return err
	}
	data := strings.Join(lines, "")
	if runtime.GOOS == "windows" {
		data = strings.ReplaceAll(data, "\n", "\r\n")
	}
	_, err = tmp.WriteString(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_, err = fsutil.Replace(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if keep {
		_ = os.Chmod(path, mode)
		return nil
	}
	SecureFile(ctx, h, path)
	return nil
}
