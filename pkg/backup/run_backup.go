package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Archive subtree of memory provider state stored relative to the user home
const externalPrefix = "_external/"

const (
	runBackupPrefix = "icebath-backup-"
	backupLockName  = ".backup.lock"
	zipCompressSix  = 6
)

// Writer behind the print calls of the snapshot helpers that take no writer, swapped by tests
var stdout io.Writer = os.Stdout

// Routes the print calls of the writerless helpers into w until the returned restore runs
func SwapOutput(w io.Writer) (restore func()) {
	old := stdout
	stdout = w
	return func() { stdout = old }
}

// Wall clock of archive names and snapshot ids, a seam tests skew forward instead of sleeping
var backupNow = time.Now

// Directory a SQLite snapshot is staged in before it enters an archive, a seam tests spy on
var stageSQLiteSnapshot = func(dir string) (string, error) {
	f, err := os.CreateTemp(dir, "tmp*.db")
	if err != nil {
		return "", err
	}
	return f.Name(), f.Close()
}

// ZipFile.write of one member, a seam tests replace to fail a member part way
var zipWrite = func(zf *pyzip.ZipFile, path, arcname string) error { return zf.Write(path, arcname) }

// WAL safe SQLite copy of the backup and snapshot paths, a seam tests replace or spy on
var copySQLite = func(ctx context.Context, src, dst string) (bool, error) { return safeCopyDB(ctx, src, dst, 10.0) }

// os.replace of a finished archive or snapshot onto its published name, a seam tests observe
var osReplace = os.Rename

// Memory provider paths outside the home, a seam the transfer fixtures empty
var memoryProviderExternalPaths = collectMemoryProviderExternalPaths

// Replaces the memory provider path collector until the returned restore runs
func SwapMemoryProviderExternalPaths(f func(ctx context.Context, h string) []string) (restore func()) {
	old := memoryProviderExternalPaths
	memoryProviderExternalPaths = f
	return func() { memoryProviderExternalPaths = old }
}

// Replaces the gateway service check and start of the import revive until the returned restore runs
func SwapGatewayService(running func(context.Context) (bool, error),
	ensure func(context.Context, io.Writer, gateway.ServiceSetupOptions) (bool, error)) (restore func()) {
	oldRunning, oldEnsure := isServiceRunning, ensureGatewayService
	isServiceRunning, ensureGatewayService = running, ensure
	return func() { isServiceRunning, ensureGatewayService = oldRunning, oldEnsure }
}

// Process exit a backup command requests, the status the CLI returns
type SystemExit struct{ Code int }

func (e *SystemExit) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Another process already owns the backup slot
type BackupInProgressError struct{ Msg string }

func (e *BackupInProgressError) Error() string      { return e.Msg }
func (e *BackupInProgressError) PyTypeName() string { return "BackupInProgressError" }

// SQLite snapshot failure that aborts an automatic archive
type sqliteSnapshotError struct{ rel string }

func (e *sqliteSnapshotError) Error() string { return e.rel }

// Holder scan of the retired updater path, always unknown so an old updater never restores over live holders
func retiredForeignDBHolderPIDs(string) []int { return nil }

// How to restore a state snapshot through the slash command
func snapshotRecoveryHint() string {
	return "To restore a newer snapshot, start `icebath` in a terminal and run `/snapshot list`, then " +
		"`/snapshot restore <id>` (CLI only)."
}

// Runs body while holding the cross process backup slot of a home, refusing after the timeout
func backupOperationLock(h string, timeoutSeconds float64, body func() error) error {
	lockPath := pypath.PathJoin(h, backupLockName)
	if err := home.PathMkdir(pypath.PathParent(lockPath), 0o777, true, true); err != nil {
		return err
	}
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	acquired := false
	defer func() {
		if acquired {
			pyfs.UnlockFile(handle)
		}
		handle.Close()
	}()
	deadline := time.Now().Add(time.Duration(max(0, timeoutSeconds) * float64(time.Second)))
	if err := prepareLockFile(lockPath, handle); err != nil {
		return err
	}
	for !acquired {
		if ok, _ := pyfs.TryLockFile(handle); ok {
			acquired = true
			break
		}
		if !time.Now().Before(deadline) {
			return &BackupInProgressError{"another Icebath backup is already running"}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return body()
}

// Hidden sibling of the final path that body fills, published only after body succeeds
func atomicOutputPath(finalPath string, publishPath func() (string, bool), body func(partial string) error) (err error) {
	partial := pypath.PathJoin(pypath.PathParent(finalPath),
		fmt.Sprintf(".%s.%d-%d.partial", pypath.PathName(finalPath), os.Getpid(), pythread.GoroutineID()))
	if err := unlinkMissingOK(partial); err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			unlinkMissingOK(partial)
			panic(r)
		}
		if err != nil {
			unlinkMissingOK(partial)
		}
	}()
	if err := body(partial); err != nil {
		return err
	}
	destination, publish := finalPath, true
	if publishPath != nil {
		destination, publish = publishPath()
	}
	if !publish {
		return unlinkMissingOK(partial)
	}
	return osReplace(partial, destination)
}

// Existing paths the active memory provider declares for backup, empty on any provider failure
func collectMemoryProviderExternalPaths(ctx context.Context, h string) []string {
	env := &tools.Env{Home: h, Log: slog.Default()}
	active := memory.ActiveMemoryProvider(ctx, env)
	if active == "" {
		return nil
	}
	provider := memory.LoadMemoryProvider(ctx, env, active)
	if provider == nil {
		return nil
	}
	declared, err := provider.BackupPaths()
	if err != nil {
		slog.Warn(fmt.Sprintf("backup_paths() failed for memory provider %s: %s", pyrepr.StrRepr(active), excText(err)))
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range declared {
		p, err := pypath.PathExpanduser(pypath.PathStr(raw))
		if err != nil {
			continue
		}
		p = pypath.PathStr(p)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		resolved, err := pypath.Resolve(p)
		if err != nil {
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, p)
		}
	}
	return out
}

// Regular files under a base file or directory, skipping links, excluded directories and runtime names
func iterExternalFiles(base string) []string {
	if st, err := os.Lstat(base); err == nil && st.Mode().IsRegular() {
		return []string{base}
	}
	if !isDir(base) {
		return nil
	}
	var files []string
	pyfs.Walk(base, false, func(dirpath string, dirnames, filenames []string) []string {
		var keep []string
		for _, d := range dirnames {
			if !excludedDirs[d] {
				keep = append(keep, d)
			}
		}
		for _, f := range filenames {
			fp := pypath.PathJoin(dirpath, f)
			name := pypath.PathName(fp)
			if !(isNonRegularPath(fp) || excludedNames[name] || hasAnySuffix(name, excludedSuffixes)) {
				files = append(files, fp)
			}
		}
		return keep
	})
	return files
}

// Reports whether an archive loop error is one the loop records and continues past
func recordedArchiveError(err error) bool {
	var ve *pypath.ValueError
	return pypath.IsOSError(err) || pyzip.IsValueError(err) || errors.As(err, &ve)
}

// Drops the members a failed write created from the central directory and cuts their bytes off the file
func discardFailedZipMembers(zf *pyzip.ZipFile, filelistLen int) error {
	if len(zf.Filelist) <= filelistLen {
		return nil
	}
	offset := zf.Filelist[filelistLen].HeaderOffset
	if err := zf.TruncateAt(offset); err != nil {
		return err
	}
	zf.StartDir = offset
	zf.Filelist = zf.Filelist[:filelistLen]
	clear(zf.NameToInfo)
	for _, info := range zf.Filelist {
		zf.NameToInfo[info.Filename] = info
	}
	return nil
}

// Writes one member, keeping a failed partial write out of the archive
func writeZipFile(zf *pyzip.ZipFile, path, arcname string) error {
	filelistLen := len(zf.Filelist)
	if err := zipWrite(zf, path, arcname); err != nil {
		if derr := discardFailedZipMembers(zf, filelistLen); derr != nil {
			return derr
		}
		return err
	}
	return nil
}

// Adds a WAL safe snapshot staged beside the output zip, its size and false when the copy failed
func zipSQLiteSnapshot(ctx context.Context, zf *pyzip.ZipFile, absPath, relPath, outPath string) (size int64, ok bool, err error) {
	tmpDB, err := stageSQLiteSnapshot(pypath.PathStr(pypath.PathParent(outPath)))
	if err != nil {
		return 0, false, err
	}
	defer func() {
		if uerr := unlinkMissingOK(tmpDB); uerr != nil {
			size, ok, err = 0, false, uerr
		}
	}()
	copied, err := copySQLite(ctx, absPath, tmpDB)
	if err != nil || !copied {
		return 0, false, err
	}
	if err := writeZipFile(zf, tmpDB, relPath); err != nil {
		return 0, false, err
	}
	st, err := os.Stat(tmpDB)
	if err != nil {
		return 0, false, err
	}
	return st.Size(), true, nil
}

// Callbacks of one archive pass over the home files
type zipEntryHooks struct {
	onDBFailure func(rel string) error
	onError     func(rel string, err error)
	onProgress  func(i int)
	trackBytes  bool
}

// Adds every home file, databases through the WAL safe snapshot, and returns the bytes archived
func writeZipEntries(ctx context.Context, zf *pyzip.ZipFile, files []backupFile, outPath string, hooks zipEntryHooks) (int64, error) {
	var total int64
	for i, f := range files {
		skipped := false
		err := func() error {
			if pypath.Suffix(f.abs) == ".db" {
				size, ok, err := zipSQLiteSnapshot(ctx, zf, f.abs, f.rel, outPath)
				if err != nil {
					return err
				}
				if !ok {
					skipped = true
					return hooks.onDBFailure(f.rel)
				}
				total += size
				return nil
			}
			if err := writeZipFile(zf, f.abs, f.rel); err != nil {
				return err
			}
			if hooks.trackBytes {
				st, err := os.Stat(f.abs)
				if err != nil {
					return err
				}
				total += st.Size()
			}
			return nil
		}()
		if err != nil {
			if recordedArchiveError(err) {
				hooks.onError(f.rel, err)
				continue
			}
			return total, err
		}
		if skipped {
			continue
		}
		if (i+1)%500 == 0 {
			hooks.onProgress(i + 1)
		}
	}
	return total, nil
}

// Prints a header then at most ten indented lines and a count of the rest
func printCapped(w io.Writer, header string, lines []string, indent string) {
	fmt.Fprintln(w, header)
	for _, line := range lines[:min(10, len(lines))] {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
	if len(lines) > 10 {
		fmt.Fprintf(w, "%s... and %d more\n", indent, len(lines)-10)
	}
}

// Zip path for the output option whose parent exists, an unwritable path printing one error line and exiting 1
func resolveBackupOutputPath(w io.Writer, output string) (string, error) {
	outPath := ""
	defaultName := runBackupPrefix + backupNow().Format("2006-01-02-150405") + ".zip"
	err := func() error {
		if output != "" {
			p, err := pypath.PathExpanduser(output)
			if err != nil {
				return err
			}
			if outPath, err = pypath.Resolve(p); err != nil {
				return err
			}
			if isDir(outPath) {
				outPath = pypath.PathJoin(outPath, defaultName)
			}
		} else {
			userHome, err := home.UserHome()
			if err != nil {
				return err
			}
			outPath = pypath.PathJoin(userHome, defaultName)
		}
		if suffix := pypath.Suffix(outPath); pystr.Lower(suffix) != ".zip" {
			withZip, err := pypath.WithSuffix(outPath, suffix+".zip")
			if err != nil {
				return err
			}
			outPath = pypath.PathStr(withZip)
		}
		return home.PathMkdir(pypath.PathParent(outPath), 0o777, true, true)
	}()
	if err != nil {
		if !pypath.IsOSError(err) {
			return "", err
		}
		shown := output
		if shown == "" {
			shown = outPath
		}
		fmt.Fprintf(w, "Error: cannot write backup to %s: %s\n", shown, excText(err))
		return "", &SystemExit{1}
	}
	return outPath, nil
}

// Memory provider file archived under the external prefix
type externalEntry struct{ abs, arcname string }

// External memory provider files arc named under the user home and the declared paths skipped outside it
func collectExternalEntries(ctx context.Context, h string) ([]externalEntry, []string, error) {
	userHome, err := home.UserHome()
	if err != nil {
		return nil, nil, err
	}
	homeDir, err := pypath.Resolve(userHome)
	if err != nil {
		return nil, nil, err
	}
	var entries []externalEntry
	var skipped []string
	for _, base := range memoryProviderExternalPaths(ctx, h) {
		resolved, err := pypath.Resolve(base)
		if err != nil {
			skipped = append(skipped, base)
			continue
		}
		if _, inside := pypath.PathRelativeTo(resolved, homeDir); !inside {
			skipped = append(skipped, base)
			continue
		}
		for _, fpath := range iterExternalFiles(base) {
			resolved, err := pypath.Resolve(fpath)
			if err != nil {
				continue
			}
			if rel, inside := pypath.PathRelativeTo(resolved, homeDir); inside {
				entries = append(entries, externalEntry{fpath, externalPrefix + pypath.PathAsPosix(rel)})
			}
		}
	}
	return entries, skipped, nil
}

// Option text of a namespace entry, empty when absent or None
func nsOutput(ns map[string]any) string {
	s, _ := ns["output"].(string)
	return s
}

// Retention count of a namespace, zero when absent or None
func nsKeep(ns map[string]any) int {
	switch v := ns["keep"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// Zip backup of the home tree, false when the archive was written but is incomplete
func RunBackup(ctx context.Context, h string, ns map[string]any, w io.Writer) (bool, error) {
	if err := config.RequireFileTooling(ctx, "`icebath backup`"); err != nil {
		return false, err
	}
	root := home.Root()
	if !isDir(root) {
		fmt.Fprintf(w, "Error: Icebath home directory not found at %s\n", root)
		return false, &SystemExit{1}
	}
	complete := false
	err := backupOperationLock(root, 0.25, func() error {
		var err error
		complete, err = runBackupLocked(ctx, h, ns, w, root)
		return err
	})
	var busy *BackupInProgressError
	if errors.As(err, &busy) {
		fmt.Fprintf(w, "Error: %s\n", busy.Msg)
		return false, &SystemExit{2}
	}
	return complete, err
}

// Milliseconds since a start for the phase logs
func sinceMS(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }

// Writes the full backup while the backup slot is held
func runBackupLocked(ctx context.Context, h string, ns map[string]any, w io.Writer, root string) (bool, error) {
	outPath, err := resolveBackupOutputPath(w, nsOutput(ns))
	if err != nil {
		return false, err
	}
	scanStarted := time.Now()
	slog.Info("backup phase=scan status=started")
	fmt.Fprintf(w, "Scanning %s ...\n", home.Display(h))
	skippedDirs := map[string]bool{}
	files := iterBackupFiles(root, outPath, skippedDirs)
	external, skippedExternal, err := collectExternalEntries(ctx, h)
	if err != nil {
		return false, err
	}
	if len(files) == 0 && len(external) == 0 {
		slog.Info(fmt.Sprintf("backup phase=scan status=empty duration_ms=%.1f", sinceMS(scanStarted)))
		fmt.Fprintln(w, "No files to back up.")
		return true, nil
	}
	fileCount := len(files) + len(external)
	slog.Info(fmt.Sprintf("backup phase=scan status=complete duration_ms=%.1f files=%d", sinceMS(scanStarted), fileCount))
	slog.Info(fmt.Sprintf("backup phase=archive status=started files=%d", fileCount))
	fmt.Fprintf(w, "Backing up %d files ...\n", fileCount)
	var errs []string
	t0 := time.Now()
	var totalBytes int64
	err = atomicOutputPath(outPath, nil, func(archivePath string) error {
		zf, err := pyzip.Create(archivePath, pyzip.Deflated, ptr(zipCompressSix))
		if err != nil {
			return err
		}
		bodyErr := func() error {
			total, err := writeZipEntries(ctx, zf, files, outPath, zipEntryHooks{
				onProgress: func(i int) {
					fmt.Fprintf(w, "  %d/%d files ...\n", i, fileCount)
					slog.Info(fmt.Sprintf("backup phase=archive status=progress completed=%d total=%d", i, fileCount))
				},
				trackBytes:  true,
				onDBFailure: func(rel string) error { errs = append(errs, rel+": SQLite safe copy failed"); return nil },
				onError:     func(rel string, err error) { errs = append(errs, rel+": "+excText(err)) },
			})
			totalBytes = total
			if err != nil {
				return err
			}
			for _, e := range external {
				err := writeZipFile(zf, e.abs, e.arcname)
				if err == nil {
					var st os.FileInfo
					if st, err = os.Stat(e.abs); err == nil {
						totalBytes += st.Size()
					}
				}
				if err != nil {
					if !recordedArchiveError(err) {
						return err
					}
					errs = append(errs, e.arcname+": "+excText(err))
				}
			}
			return nil
		}()
		if err := zf.Close(); err != nil {
			return err
		}
		return bodyErr
	})
	if err != nil {
		return false, err
	}
	elapsed := time.Since(t0).Seconds()
	st, err := os.Stat(outPath)
	if err != nil {
		return false, err
	}
	zipSize := st.Size()
	slog.Info(fmt.Sprintf("backup phase=archive status=complete duration_ms=%.1f files=%d errors=%d bytes=%d",
		elapsed*1000, fileCount, len(errs), zipSize))
	state := "complete"
	if len(errs) > 0 {
		state = "incomplete"
	}
	fmt.Fprintf(w, "\nBackup %s: %s\n  Files:       %d\n  Original:    %s\n  Compressed:  %s\n  Time:        %.1fs\n",
		state, outPath, fileCount, tools.SizefmtBytes(totalBytes), tools.SizefmtBytes(zipSize), elapsed)
	if len(external) > 0 {
		fmt.Fprintf(w, "\n  Included %d memory-provider file(s) stored outside %s.\n", len(external), home.Display(h))
	}
	if len(skippedExternal) > 0 {
		sorted := append([]string{}, skippedExternal...)
		sort.Strings(sorted)
		lines := make([]string, 0, 10)
		for _, p := range sorted[:min(10, len(sorted))] {
			lines = append(lines, "    "+p)
		}
		fmt.Fprintf(w, "\n  Skipped %d memory-provider path(s) outside your home directory (not portable):\n%s\n",
			len(skippedExternal), strings.Join(lines, "\n"))
	}
	if len(skippedDirs) > 0 {
		dirs := make([]string, 0, len(skippedDirs))
		for d := range skippedDirs {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for i, d := range dirs {
			dirs[i] = "    " + d + "/"
		}
		fmt.Fprintf(w, "\n  Excluded directories:\n%s\n", strings.Join(dirs, "\n"))
	}
	if len(errs) > 0 {
		printCapped(w, fmt.Sprintf("\n  Archive kept, but %d file(s) could not be added:", len(errs)), errs, "  ")
	} else {
		fmt.Fprintf(w, "\nRestore with: icebath import %s\n", pypath.PathName(outPath))
	}
	if keep := nsKeep(ns); keep != 0 && len(errs) == 0 && strings.HasPrefix(pypath.PathName(outPath), runBackupPrefix) {
		pruned, err := prunePrefixedZips(pypath.PathParent(outPath), runBackupPrefix, keep, "backup")
		if err != nil {
			return false, err
		}
		if pruned > 0 {
			fmt.Fprintf(w, "  Pruned %d older %s*.zip (keeping %d).\n", pruned, runBackupPrefix, keep)
		}
	}
	return len(errs) == 0, nil
}

// Address of a value for an optional argument
func ptr[T any](v T) *T { return &v }
