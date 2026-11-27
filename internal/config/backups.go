package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Copies of a config file kept for each reason it is copied, the newest first
const BackupKeep = 5

// Reasons a config file is copied
const (
	// Read and parsed, the copy a broken file falls back to
	Good = "good"
	// Found broken, kept for the person to compare with their fix
	Corrupt = "corrupt"
)

// Folder of the dated copies of a config file, beside it
func BackupsDir(path string) string {
	return filepath.Join(filepath.Dir(path), "backups", "config")
}

// Path as a person reads it, the home folder written ~
func display(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home); ok && (rest == "" || rest[0] == filepath.Separator) {
		return "~" + rest
	}
	return path
}

// Copies of a config file kept, the newest first, of one reason when given
func Backups(path, reason string) []string {
	dir := BackupsDir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix := filepath.Base(path) + "."
	if reason != "" {
		prefix += reason + "."
	}
	var out []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), prefix) {
			out = append(out, filepath.Join(dir, entry.Name()))
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// Copies a config file into its backups for the reason, 0600 as the credentials it may hold call for, keeping the newest keep of that
// reason: the copy, or empty when there was nothing to copy or the newest copy already holds the same bytes. A copy that fails is told and
// skipped, never failing what the file was read or written for
func Backup(path, reason string, keep int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	fail := func(err error) string {
		slog.Warn(fmt.Sprintf("Could not back up %s (%s): %v", path, reason, err))
		return ""
	}
	existing := Backups(path, reason)
	if len(existing) > 0 {
		newest, err := os.ReadFile(existing[0])
		if err != nil {
			return fail(err)
		}
		if bytes.Equal(newest, data) {
			return ""
		}
	}
	dest := filepath.Join(BackupsDir(path), filepath.Base(path)+"."+reason+"."+time.Now().Format("20060102-150405"))
	if fsx.Lexists(dest) {
		return ""
	}
	if err := fsx.Write(dest, data, 0o600); err != nil {
		return fail(err)
	}
	for _, stale := range append([]string{dest}, existing...)[min(max(keep, 1), len(existing)+1):] {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(err)
		}
	}
	return dest
}

// Settings of the newest good copy of a config file that still reads and parses, an unreadable one told and passed over; nil when there is
// none
func newestGood(path string) *viper.Viper {
	for _, backup := range Backups(path, Good) {
		data, err := os.ReadFile(backup)
		var v *viper.Viper
		if err == nil {
			v, err = parse(data)
		}
		if err == nil {
			return v
		}
		slog.Warn(fmt.Sprintf("Last-known-good backup %s is unreadable: %s", backup, details(err)))
	}
	return nil
}
