package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Critical state files and directories of a home a quick snapshot copies, missing ones skipped
var quickStateFiles = []string{
	"state.db", "config.yaml", ".env", "auth.json", "cron/jobs.json", "cron/executions.db",
	"gateway_state.json", "channel_directory.json", "channel_aliases.json", "processes.json",
	"gateway/discord_message_recovery.db",
	"projects.db",
	"response_store.db",
	"memory_store.db",
	"verification_evidence.db",
	"kanban.db",
	"kanban/boards",
	"pairing",
	"platforms/pairing",
	"feishu_comment_pairing.json",
}

// Cron jobs file inside a home, kept in step with the quick state files
const cronJobsRel = "cron/jobs.json"

// Quick snapshot of a home under the backup slot, the id or empty when nothing was captured
func CreateQuickSnapshot(label *string, h string, keep *int, maxFileSize *int64) (string, error) {
	return createQuickSnapshot(context.Background(), stdout, label, h, keep, maxFileSize)
}

// Quick snapshot of a home under the backup slot, printing its warnings to w
func createQuickSnapshot(ctx context.Context, w io.Writer, label *string, h string, keep *int, maxFileSize *int64) (string, error) {
	if h == "" {
		h = home.Dir(ctx)
	}
	var snapID string
	err := backupOperationLock(h, 0.25, func() error {
		var err error
		snapID, err = createQuickSnapshotLocked(ctx, w, label, h, keep, maxFileSize)
		return err
	})
	return snapID, err
}

// Python truthiness of an optional label
func labelText(label *string) string {
	if label == nil {
		return ""
	}
	return *label
}

// Copies the critical state files into a private staged directory, publishes it with a manifest and prunes old snapshots
func createQuickSnapshotLocked(ctx context.Context, w io.Writer, label *string, h string, keep *int, maxFileSize *int64) (string, error) {
	if h == "" {
		h = home.Dir(ctx)
	}
	root := QuickSnapshotRoot(h)
	tooLarge := func(path, relName string) bool {
		if maxFileSize == nil {
			return false
		}
		st, err := os.Stat(path)
		if err != nil {
			return false
		}
		size := st.Size()
		if size <= *maxFileSize {
			return false
		}
		fmt.Fprintf(w, "  ⚠ Snapshot: skipping %s (%s exceeds %s limit)\n", relName, tools.SizefmtBytes(size), tools.SizefmtBytes(*maxFileSize))
		pkgLog().Warn(fmt.Sprintf("Quick snapshot skipped %s: %d bytes exceeds %d byte limit", relName, size, *maxFileSize))
		return true
	}
	ts := backupNow().UTC().Format("20060102-150405")
	baseSnapID := ts
	if l := labelText(label); l != "" {
		baseSnapID = ts + "-" + l
	}
	snapID := baseSnapID
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(pathutil.Join(root, snapID)); err != nil {
			break
		}
		snapID = fmt.Sprintf("%s-%d", baseSnapID, suffix)
	}
	snapDir := pathutil.Join(root, snapID)
	stagingDir := pathutil.Join(root, fmt.Sprintf(".%s.%d.partial", snapID, os.Getpid()))
	_ = os.RemoveAll(stagingDir)
	if err := home.PathMkdir(root, 0o700, true, true); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0o700); err != nil {
			return "", err
		}
	}
	if err := home.PathMkdir(stagingDir, 0o700, false, false); err != nil {
		return "", err
	}
	pkgLog().Info("quick snapshot phase=copy status=started id=" + snapID)

	manifest := jsonutil.NewObject()
	var totalSize int64
	var failedDBs, oversizedSkipped []string
	copyOne := func(src, rel, zeroedTail string) error {
		dst := pathutil.Join(stagingDir, rel)
		if err := home.PathMkdir(filepath.Dir(filepath.Clean(dst)), 0o777, true, true); err != nil {
			return err
		}
		err := func() error {
			if pathutil.Suffix(src) == ".db" {
				copied, err := copySQLite(ctx, src, dst)
				if err != nil {
					return err
				}
				if !copied {
					failedDBs = append(failedDBs, rel)
					fmt.Fprintf(w, "  ⚠ Snapshot: SQLite safe copy FAILED for %s — file may be locked or corrupted\n", rel)
					if state.IsZeroedStateDB(src, 100, false) {
						st, err := os.Stat(src)
						if err != nil {
							return err
						}
						fmt.Fprintf(w, "  ⚠ Snapshot: %s looks ZEROED (no SQLite header; %d bytes%s)\n", rel, st.Size(), zeroedTail)
					}
					return nil
				}
			} else if err := fsutil.CopyFile(src, dst); err != nil {
				return err
			}
			st, err := os.Stat(dst)
			if err != nil {
				return err
			}
			manifest.Set(rel, st.Size())
			totalSize += st.Size()
			return nil
		}()
		if err != nil {
			var stop *Interrupted
			if errors.As(err, &stop) || !errorutil.IsSystem(err) {
				return err
			}
			pkgLog().Warn(fmt.Sprintf("Could not snapshot %s: %s", rel, excText(err)))
		}
		return nil
	}

	for _, rel := range quickStateFiles {
		src := pathutil.Join(h, rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if isDir(src) {
			subs, err := pathutil.Glob(src, "**/*")
			if err != nil {
				return "", err
			}
			for _, sub := range subs {
				if !isFile(sub) {
					continue
				}
				subRel, _ := pathutil.RelativeTo(sub, h)
				subRel = filepath.ToSlash(filepath.Clean(subRel))
				if wrapped := "/" + subRel + "/"; strings.Contains(wrapped, "/workspaces/") || strings.Contains(wrapped, "/attachments/") {
					continue
				}
				if tooLarge(sub, subRel) {
					if pathutil.Suffix(sub) == ".db" {
						oversizedSkipped = append(oversizedSkipped, subRel)
					}
					continue
				}
				if err := copyOne(sub, subRel, " of NULs?"); err != nil {
					return "", err
				}
			}
			continue
		}
		if !isFile(src) {
			continue
		}
		if tooLarge(src, rel) {
			if pathutil.Suffix(src) == ".db" {
				oversizedSkipped = append(oversizedSkipped, rel)
			}
			continue
		}
		if err := copyOne(src, rel, ""); err != nil {
			return "", err
		}
	}

	if len(failedDBs) > 0 {
		fmt.Fprintln(w, "  ⚠ CRITICAL: could not snapshot DB file(s): "+strings.Join(failedDBs, ", "))
		fmt.Fprintf(w, "  ⚠ If sessions disappear after the update, check %s. %s\n", root, snapshotRecoveryHint())
		pkgLog().Error("Quick snapshot failed to capture DB file(s): " + strings.Join(failedDBs, ", "))
	}
	if manifest.Len() == 0 {
		_ = os.RemoveAll(stagingDir)
		if len(failedDBs) > 0 {
			fmt.Fprintf(w, "  ⚠ Snapshot aborted: no files captured (failed DBs: %s)\n", strings.Join(failedDBs, ", "))
		}
		return "", nil
	}

	meta := jsonutil.NewObject()
	meta.Set("id", snapID)
	meta.Set("timestamp", ts)
	if label != nil {
		meta.Set("label", *label)
	} else {
		meta.Set("label", nil)
	}
	meta.Set("file_count", int64(manifest.Len()))
	meta.Set("total_size", totalSize)
	meta.Set("files", manifest)
	meta.Set("failed_dbs", stringsAny(failedDBs))
	meta.Set("oversized_skipped", stringsAny(oversizedSkipped))
	text, err := jsonutil.Encode(meta, jsonutil.Indent(2), jsonutil.ASCII())
	if err != nil {
		return "", err
	}
	if err := writeTextFile(pathutil.Join(stagingDir, "manifest.json"), text); err != nil {
		return "", err
	}

	if runtime.GOOS != "windows" {
		for _, dir := range []string{root, stagingDir} {
			if err := os.Chmod(dir, 0o700); err != nil {
				return "", err
			}
		}
		paths, err := pathutil.Glob(stagingDir, "**/*")
		if err != nil {
			return "", err
		}
		for _, p := range paths {
			mode := os.FileMode(0)
			if isDir(p) {
				mode = 0o700
			} else if isFile(p) {
				mode = 0o600
			}
			if mode != 0 {
				if err := os.Chmod(p, mode); err != nil {
					return "", err
				}
			}
		}
	}

	if err := osReplace(stagingDir, snapDir); err != nil {
		return "", err
	}

	if len(failedDBs) == 0 && len(oversizedSkipped) == 0 {
		retain := quickDefaultKeep
		if keep != nil {
			retain = *keep
		}
		if _, err := pruneQuickSnapshotsIn(root, retain); err != nil {
			return "", err
		}
	} else {
		if len(oversizedSkipped) > 0 {
			fmt.Fprintln(w, "  ⚠ Skipping snapshot prune: DB file(s) skipped for size: "+strings.Join(oversizedSkipped, ", "))
			pkgLog().Warn("Quick snapshot skipped oversized DB file(s): " + strings.Join(oversizedSkipped, ", "))
		}
		pkgLog().Warn(fmt.Sprintf("Skipping snapshot prune because %d DB(s) failed to capture and/or %d were oversized — "+
			"preserving older snapshots as recovery source", len(failedDBs), len(oversizedSkipped)))
	}
	pkgLog().Info(fmt.Sprintf("quick snapshot phase=copy status=complete id=%s files=%d bytes=%d", snapID, manifest.Len(), totalSize))
	return snapID, nil
}

// Strings as a JSON list value
func stringsAny(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// open(path, "w", encoding="utf-8").write of a text file with the platform newline translation
func writeTextFile(path, text string) error {
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Quick snapshot creation the all profile pass calls, a seam tests replace to fail one profile
var quickSnapshotForProfile = createQuickSnapshot

// Pre update quick snapshots of every sibling profile into its own home, mapping name to snapshot id
func CreatePreUpdateSnapshotsAllProfiles(invokingHome string, keep *int, maxFileSize *int64) (*jsonutil.Object, error) {
	return createPreUpdateSnapshotsAllProfiles(context.Background(), stdout, invokingHome, keep, maxFileSize)
}

func createPreUpdateSnapshotsAllProfiles(ctx context.Context, w io.Writer, invokingHome string, keep *int, maxFileSize *int64) (*jsonutil.Object, error) {
	results := jsonutil.NewObject()
	h := invokingHome
	if h == "" {
		h = home.Dir(ctx)
	}
	for _, pair := range SiblingProfileHomes(h) {
		name, profileHome := pair[0], pair[1]
		snapID, err := quickSnapshotForProfile(ctx, w, ptr("pre-update"), profileHome, keep, maxFileSize)
		if err != nil {
			var stop *Interrupted
			if errors.As(err, &stop) {
				return results, err
			}
			pkgLog().Debug(fmt.Sprintf("Pre-update snapshot for profile %s failed: %s", name, excText(err)))
			continue
		}
		if snapID != "" {
			results.Set(name, snapID)
		}
	}
	return results, nil
}

// icebath backup --quick, one quick snapshot of the home in effect and where it is stored
func RunQuickBackup(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	var label *string
	if l, ok := ns["label"].(string); ok {
		label = &l
	}
	snapID, err := createQuickSnapshot(ctx, w, label, h, nil, nil)
	if err != nil {
		return 1, err
	}
	if snapID == "" {
		fmt.Fprintln(w, "No state files found to snapshot.")
		return 0, nil
	}
	fmt.Fprintf(w, "State snapshot created: %s\n", snapID)
	snaps, err := ListQuickSnapshots(20, h)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "  %d snapshot(s) stored in %s/state-snapshots/\n", len(snaps), home.Display(h))
	fmt.Fprintf(w, "  Restore with: /snapshot restore %s\n", snapID)
	return 0, nil
}
