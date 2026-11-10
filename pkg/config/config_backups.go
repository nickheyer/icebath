package config

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// Backups a config writer keeps per reason
const DefaultBackupKeep = 5

var legacySiblingGlobs = []string{"config.yaml.bak.[0-9]*", "config.yaml.corrupt.*.bak", "config.yaml.bak-pre-migrate-*"}

// Local time stamp a backup name ends with, the time.strftime call hermes tests replace
var backupTimestamp = func() string { return time.Now().Format("20060102-150405") }

// Directory holding the dated config backups beside a config file
func BackupsDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "backups", "config")
}

// Existing backups newest first, filtered to one reason when given
func ListConfigBackups(configPath, reason string) []string {
	root := BackupsDir(configPath)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	prefix := filepath.Base(configPath) + "."
	if reason != "" {
		prefix += reason + "."
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, p)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// Copies config into the backups dir, empty when skipped or failed, errors when no backend serves config
func BackupConfig(ctx context.Context, configPath, reason string, keep int) (string, error) {
	files, err := SupportsFileTooling(ctx)
	if err != nil {
		return "", err
	}
	if !files {
		return "", nil
	}
	st, err := os.Stat(configPath)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", nil
	}
	root := BackupsDir(configPath)
	fail := func(err error) string {
		slog.Warn(fmt.Sprintf("Could not back up %s (%s): %s", configPath, reason, pyOSErrorString(err)))
		return ""
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return fail(err), nil
	}
	sweepLegacySiblings(configPath, root)
	existing := ListConfigBackups(configPath, reason)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fail(err), nil
	}
	if len(existing) > 0 {
		newest, err := os.ReadFile(existing[0])
		if err != nil {
			return fail(err), nil
		}
		if bytes.Equal(data, newest) {
			return "", nil
		}
	}
	dest := filepath.Join(root, filepath.Base(configPath)+"."+reason+"."+backupTimestamp())
	if _, err := os.Lstat(dest); err == nil {
		return "", nil
	}
	if err := pyshutil.Copy2(configPath, dest); err != nil {
		return fail(err), nil
	}
	all := append([]string{dest}, existing...)
	start := keep
	if start < 0 {
		start = max(len(all)+start, 0)
	}
	for _, stale := range all[min(start, len(all)):] {
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			return fail(err), nil
		}
	}
	return dest, nil
}

// Raw mapping of the newest good backup, nil when absent, unreadable, not a mapping or the backend has no files
func LoadNewestGoodBackup(ctx context.Context, configPath string) (map[string]any, error) {
	files, err := SupportsFileTooling(ctx)
	if err != nil || !files {
		return nil, err
	}
	newest := ListConfigBackups(configPath, "good")
	if len(newest) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(newest[0])
	var doc any
	if err == nil {
		doc, err = FastSafeLoad(data)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Last-known-good backup %s is unreadable: %s", newest[0], pyOSErrorString(err)))
		return nil, nil
	}
	if m, ok := doc.(map[string]any); ok {
		return m, nil
	}
	return nil, nil
}

// Moves legacy backup siblings of config.yaml into the backups dir
func sweepLegacySiblings(configPath, root string) {
	for _, pattern := range legacySiblingGlobs {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(configPath), pattern))
		for _, old := range matches {
			st, err := os.Lstat(old)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			if err := os.Rename(old, filepath.Join(root, filepath.Base(old))); err != nil {
				slog.Debug(fmt.Sprintf("Could not move legacy backup %s: %s", old, pyOSErrorString(err)))
			}
		}
	}
}
