package config

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Backups a config writer keeps per reason
const DefaultBackupKeep = 5

var legacySiblingGlobs = []string{"config.yaml.bak.[0-9]*", "config.yaml.corrupt.*.bak", "config.yaml.bak-pre-migrate-*"}

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
func BackupConfig(configPath, reason string, keep int) (string, error) {
	files, err := SupportsFileTooling(context.Background())
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
		slog.Warn("Could not back up config", "path", configPath, "reason", reason, "error", err)
		return ""
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
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
	dest := filepath.Join(root, filepath.Base(configPath)+"."+reason+"."+time.Now().Format("20060102-150405"))
	if _, err := os.Lstat(dest); err == nil {
		return "", nil
	}
	if err := copyPreservingTimes(configPath, dest, st); err != nil {
		return fail(err), nil
	}
	all := append([]string{dest}, existing...)
	if keep < len(all) {
		for _, stale := range all[max(keep, 0):] {
			if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
				return fail(err), nil
			}
		}
	}
	return dest, nil
}

// Copies a file with its mode and modification time as shutil copy2 does
func copyPreservingTimes(src, dest string, st os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dest, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dest, st.ModTime(), st.ModTime())
}

// Raw mapping of the newest good backup, nil when absent, unreadable, not a mapping or the backend has no files
func LoadNewestGoodBackup(configPath string) (map[string]any, error) {
	files, err := SupportsFileTooling(context.Background())
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
		err = yaml.Unmarshal(data, &doc)
	}
	if err != nil {
		slog.Warn("Last-known-good backup is unreadable", "path", newest[0], "error", err)
		return nil, nil
	}
	if m := AsMap(doc); m != nil {
		return normalizeMaps(m).(map[string]any), nil
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
				slog.Debug("Could not move legacy backup", "path", old, "error", err)
			}
		}
	}
}
