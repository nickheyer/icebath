package config

import (
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Dotenv lines decoded as utf-8-sig with replacement, each normalized and ending in one newline
func ReadEnvLines(path string) ([]string, error) { return readSanitizedEnvLines(path) }

// Replaces a dotenv file with lines verbatim, keeping its mode when asked or securing it under the process home
func WriteEnvLines(path string, lines []string, preserveMode bool) error {
	return writeEnvLinesMode("", path, lines, preserveMode)
}

// Reports whether a dotenv line assigns key in exactly the shapes the env loader parses
func EnvLineDefinesKey(line, key string) bool { return envLineDefines(line, key) }

// Replaces a dotenv file through a staged temp beside its resolved target, keeping the prior mode or securing it
func writeEnvLinesMode(h, path string, lines []string, preserveMode bool) error {
	var mode os.FileMode
	keep := false
	if preserveMode {
		if info, err := os.Stat(path); err == nil {
			mode, keep = info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky), true
		}
	}
	tmp, err := pyfs.MkstempBeside(path, ".env_", ".tmp")
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
		_, err = pyfs.AtomicReplace(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if keep {
		_ = os.Chmod(path, mode)
		return nil
	}
	SecureFile(h, path)
	return nil
}
