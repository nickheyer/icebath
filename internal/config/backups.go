package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Copies of one reason kept, older ones removed after each new copy
const defaultBackupKeep = 5

// Reports a copy earlier releases wrote beside the config named base, base.bak.<digit>..., base.corrupt.*.bak or base.bak-pre-migrate-*
func legacyBackupName(base, name string) bool {
	if rest, ok := strings.CutPrefix(name, base+".bak."); ok {
		return rest != "" && rest[0] >= '0' && rest[0] <= '9'
	}
	if rest, ok := strings.CutPrefix(name, base+".corrupt."); ok {
		return strings.HasSuffix(rest, ".bak")
	}
	return strings.HasPrefix(name, base+".bak-pre-migrate-")
}

// Directory the point in time copies of the config at path are kept in
func configBackupsDir(path string) string {
	return home.ConfigBackups.In(filepath.Dir(path))
}

// Existing copies of the config at path, newest first, one reason only when given
func listConfigBackups(path, reason string) []string {
	root := configBackupsDir(path)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	prefix := filepath.Base(path) + "."
	if reason != "" {
		prefix += reason + "."
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// Copies the config into the backups directory unless missing, empty or unchanged since the newest copy for reason
func (s *Store) backupConfig(ctx context.Context, path, reason string) (string, bool) {
	b, err := s.backend(ctx)
	if err != nil || !b.SupportsFileTooling() {
		return "", false
	}
	dest, err := s.copyConfig(ctx, path, reason)
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not back up %s (%s): %v", path, reason, err))
		return "", false
	}
	return dest, dest != ""
}

// Makes one copy and prunes the oldest beyond the kept count, empty when nothing was copied
func (s *Store) copyConfig(ctx context.Context, path, reason string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return "", nil
	}
	root := configBackupsDir(path)
	if err := s.homes.Mkdir(ctx, root); err != nil {
		return "", err
	}
	sweepLegacyBackups(path, root)
	existing := listConfigBackups(path, reason)
	if len(existing) > 0 {
		current, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		newest, err := os.ReadFile(existing[0])
		if err != nil {
			return "", err
		}
		if bytes.Equal(current, newest) {
			return "", nil
		}
	}
	dest := filepath.Join(root, fmt.Sprintf("%s.%s.%s", filepath.Base(path), reason, time.Now().Format("20060102-150405")))
	if _, err := os.Lstat(dest); err == nil {
		return "", nil
	}
	if err := fileutil.CopyFile(path, dest); err != nil {
		return "", err
	}
	for _, stale := range append([]string{dest}, existing...)[min(defaultBackupKeep, len(existing)+1):] {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return dest, nil
}

// Moves copies earlier releases left beside the config into the backups directory, hand named copies untouched
func sweepLegacyBackups(path, root string) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !legacyBackupName(filepath.Base(path), e.Name()) {
			continue
		}
		old := filepath.Join(filepath.Dir(path), e.Name())
		fi, err := os.Lstat(old)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Rename(old, filepath.Join(root, e.Name())); err != nil {
			slog.Debug(fmt.Sprintf("Could not move legacy backup %s: %v", old, err))
		}
	}
}

// Raw mapping of the newest good copy, the file as last loaded successfully, false without a usable copy
func (s *Store) newestGoodBackup(ctx context.Context, path string) (*yaml.Node, bool) {
	b, err := s.backend(ctx)
	if err != nil || !b.SupportsFileTooling() {
		return nil, false
	}
	newest := listConfigBackups(path, "good")
	if len(newest) == 0 {
		return nil, false
	}
	doc, err := readYAMLFile(newest[0])
	if err != nil {
		slog.Warn(fmt.Sprintf("Last-known-good backup %s is unreadable: %v", newest[0], err))
		return nil, false
	}
	if !isMapping(doc.root) {
		return nil, false
	}
	return doc.root, true
}
