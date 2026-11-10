package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
)

const (
	preUpdateBackupsDir = "backups"
	preUpdatePrefix     = "pre-update-"
	preMigrationPrefix  = "pre-migration-"
	incompleteZipSuffix = ".incomplete.zip"

	// Pre update archives kept when the caller names no retention
	PreUpdateDefaultKeep = 5
	// Pre migration archives kept when the caller names no retention
	PreMigrationDefaultKeep = 5
)

// Entries of a directory passing keep, newest by name first, empty when the directory is missing
func newestFirst(root string, keep func(path string) bool) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if p := pypath.PathJoin(root, e.Name()); keep(p) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pypath.PathName(out[i]) > pypath.PathName(out[j]) })
	return out, nil
}

// Index a Python slice from start begins at, a negative start counting back from the end
func sliceFrom(n, start int) int {
	if start < 0 {
		return max(n+start, 0)
	}
	return min(start, n)
}

// Removes every entry past the first keep and returns how many removals succeeded
func pruneOldest(newest []string, keep int, remove func(string) error, what string) int {
	deleted := 0
	for _, p := range newest[sliceFrom(len(newest), keep):] {
		if err := remove(p); err != nil {
			slog.Warn(fmt.Sprintf("Failed to prune %s %s: %s", what, pypath.PathName(p), excText(err)))
			continue
		}
		deleted++
	}
	return deleted
}

// Reports whether a path is a regular file after following links
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Removes the oldest complete prefixed zips beyond keep, leaving other zips and salvage archives alone
func prunePrefixedZips(backupDir, prefix string, keep int, what string) (int, error) {
	backups, err := newestFirst(backupDir, func(p string) bool {
		name := pypath.PathName(p)
		return isFile(p) && strings.HasPrefix(name, prefix) && pystr.Lower(pypath.Suffix(p)) == ".zip" &&
			!strings.HasSuffix(pystr.Lower(name), incompleteZipSuffix)
	})
	if err != nil {
		return 0, err
	}
	return pruneOldest(backups, keep, os.Remove, what), nil
}

// Keeps only the newest prefixed salvage archive
func pruneIncompleteZips(backupDir, prefix, what string) (int, error) {
	salvage, err := newestFirst(backupDir, func(p string) bool {
		name := pypath.PathName(p)
		return isFile(p) && strings.HasPrefix(name, prefix) && strings.HasSuffix(pystr.Lower(name), incompleteZipSuffix)
	})
	if err != nil {
		return 0, err
	}
	return pruneOldest(salvage, 1, os.Remove, "incomplete "+what), nil
}

// Writes backups/<prefix><timestamp>.zip and prunes older ones, empty when nothing was archived or the archive is incomplete
func createPrefixedFullBackup(ctx context.Context, h, prefix string, keep int, what, pruneWhat string) (string, error) {
	root := h
	if root == "" {
		root = home.Root()
	}
	if !isDir(root) {
		return "", nil
	}
	backupDir := pypath.PathJoin(root, preUpdateBackupsDir)
	if err := home.PathMkdir(backupDir, 0o777, true, true); err != nil {
		slog.Warn(fmt.Sprintf("Could not create %s backup dir %s: %s", what, backupDir, excText(err)))
		return "", nil
	}
	outPath := pypath.PathJoin(backupDir, prefix+backupNow().Format("2006-01-02-150405")+".zip")
	written, err := writeFullZipBackup(ctx, outPath, root)
	if err != nil {
		return "", err
	}
	if written == "" {
		_, err := pruneIncompleteZips(backupDir, prefix, pruneWhat)
		return "", err
	}
	if _, err := prunePrefixedZips(backupDir, prefix, keep, pruneWhat); err != nil {
		return "", err
	}
	return outPath, nil
}

// Full zip of a home before an update into backups/pre-update-<timestamp>.zip, empty when skipped or incomplete
func CreatePreUpdateBackup(h string, keep int) (string, error) {
	return createPrefixedFullBackup(context.Background(), h, preUpdatePrefix, max(keep, 1), "pre-update", "backup")
}

// Full zip of a home before an agent import into backups/pre-migration-<timestamp>.zip, empty when skipped or incomplete
func CreatePreMigrationBackup(h string, keep int) (string, error) {
	return createPrefixedFullBackup(context.Background(), h, preMigrationPrefix, max(keep, 0), "pre-migration", "pre-migration backup")
}

// Automatic full zip under the backup slot, empty when another backup holds it
func writeFullZipBackup(ctx context.Context, outPath, root string) (string, error) {
	var written string
	err := backupOperationLock(root, 0.25, func() error {
		var err error
		written, err = writeFullZipBackupLocked(ctx, outPath, root)
		return err
	})
	var busy *BackupInProgressError
	if errors.As(err, &busy) {
		slog.Warn("Full-zip backup skipped: " + busy.Msg)
		return "", nil
	}
	return written, err
}

// Writes the automatic archive, publishing an incomplete one as salvage and an all failed one nowhere
func writeFullZipBackupLocked(ctx context.Context, outPath, root string) (string, error) {
	scanStarted := time.Now()
	slog.Info("automatic backup phase=scan status=started")
	files := iterBackupFiles(root, outPath, nil)
	if len(files) == 0 {
		return "", nil
	}
	slog.Info(fmt.Sprintf("automatic backup phase=scan status=complete duration_ms=%.1f files=%d", sinceMS(scanStarted), len(files)))
	var errs []string
	cappedErrors := func() string {
		shown := strings.Join(errs[:min(10, len(errs))], "; ")
		if len(errs) > 10 {
			return fmt.Sprintf("%s (+%d more)", shown, len(errs)-10)
		}
		return shown
	}
	name := pypath.PathName(outPath)
	salvagePath := pypath.PathJoin(pypath.PathParent(outPath), name[:len(name)-len(pypath.Suffix(outPath))]+incompleteZipSuffix)
	published := ""
	publish := func() (string, bool) {
		if len(errs) == 0 {
			published = outPath
		} else if len(errs) < len(files) {
			published = salvagePath
		}
		return published, published != ""
	}
	archiveStarted := time.Now()
	err := atomicOutputPath(outPath, publish, func(archivePath string) error {
		zf, err := pyzip.Create(archivePath, pyzip.Deflated, ptr(zipCompressSix))
		if err != nil {
			return err
		}
		_, bodyErr := writeZipEntries(ctx, zf, files, outPath, zipEntryHooks{
			onDBFailure: func(rel string) error {
				slog.Warn("Full-zip backup aborted: SQLite snapshot failed for " + rel)
				return &sqliteSnapshotError{rel}
			},
			trackBytes: false,
			onError:    func(rel string, err error) { errs = append(errs, rel+": "+excText(err)) },
			onProgress: func(i int) {
				slog.Info(fmt.Sprintf("automatic backup phase=archive status=progress completed=%d total=%d", i, len(files)))
			},
		})
		if err := zf.Close(); err != nil {
			return err
		}
		return bodyErr
	})
	if err != nil {
		var snap *sqliteSnapshotError
		if pypath.IsOSError(err) || errors.As(err, &snap) {
			slog.Warn("Full-zip backup: zip write failed: " + excText(err))
			return "", nil
		}
		return "", err
	}
	if published == "" {
		slog.Warn("Full-zip backup: every entry failed, nothing salvaged: " + cappedErrors())
		return "", nil
	}
	st, err := os.Stat(published)
	if err != nil {
		return "", err
	}
	if published != outPath {
		slog.Warn(fmt.Sprintf("automatic backup phase=archive status=incomplete duration_ms=%.1f files=%d errors=%d "+
			"bytes=%d salvage=%s skipped=%s", sinceMS(archiveStarted), len(files), len(errs), st.Size(), salvagePath, cappedErrors()))
		return "", nil
	}
	slog.Info(fmt.Sprintf("automatic backup phase=archive status=complete duration_ms=%.1f files=%d bytes=%d",
		sinceMS(archiveStarted), len(files), st.Size()))
	return outPath, nil
}
