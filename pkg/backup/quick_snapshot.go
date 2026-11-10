package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const quickSnapshotsDir = "state-snapshots"

// Directory holding the quick state snapshots of a home, the home in effect when empty
func QuickSnapshotRoot(h string) string {
	if h == "" {
		h = home.Dir(context.Background())
	}
	return filepath.Join(h, quickSnapshotsDir)
}

// Snapshots a quick snapshot root retains by default
const quickDefaultKeep = 20

// Entries of a directory sorted by name descending, nil when the directory cannot be listed
func entriesNewestFirst(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// Reports whether an entry of the snapshot root is a finished snapshot directory
func isSnapshotEntry(root, name string) bool {
	return isDir(filepath.Join(root, name)) && !strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".partial")
}

// Manifests of the newest quick snapshots up to the limit, a stub entry for an unreadable manifest
func ListQuickSnapshots(limit int, h string) ([]any, error) {
	root := QuickSnapshotRoot(h)
	if _, err := os.Stat(root); err != nil {
		return []any{}, nil
	}
	names, err := entriesNewestFirst(root)
	if err != nil {
		return nil, err
	}
	results := []any{}
	for _, name := range names {
		if !isSnapshotEntry(root, name) {
			continue
		}
		manifestPath := filepath.Join(root, name, "manifest.json")
		if _, err := os.Stat(manifestPath); err == nil {
			manifest, err := readManifest(manifestPath)
			var decodeErr *pyjson.DecodeError
			switch {
			case err == nil:
				results = append(results, manifest)
			case errors.As(err, &decodeErr) || pypath.IsOSError(err):
				stub := pyjson.NewObject()
				stub.Set("id", name)
				stub.Set("file_count", int64(0))
				stub.Set("total_size", int64(0))
				results = append(results, stub)
			default:
				return nil, err
			}
		}
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// json.load of a utf-8-sig manifest file, a bad encoding propagating as the codec error
func readManifest(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Directory of a restorable quick snapshot, empty for an invalid, escaping or missing id, failing where resolve fails
func QuickSnapshotDir(snapshotID, h string) (string, error) {
	root := QuickSnapshotRoot(h)
	if snapshotID == "" || strings.ContainsAny(snapshotID, `/\`) || snapshotID == "." || snapshotID == ".." {
		slog.Error("Invalid snapshot_id: " + snapshotID)
		return "", nil
	}
	snapDir := filepath.Join(root, snapshotID)
	resolved, err := pypath.Resolve(snapDir)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := pypath.Resolve(root)
	if err != nil {
		return "", err
	}
	if _, inside := pypath.PathRelativeTo(resolved, resolvedRoot); !inside {
		slog.Error("Snapshot path traversal blocked for id: " + snapshotID)
		return "", nil
	}
	if !isDir(snapDir) {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err != nil {
		return "", nil
	}
	return snapDir, nil
}

// Removes the oldest finished snapshots beyond keep and returns how many went, failing when the root cannot be listed
func pruneQuickSnapshotsIn(root string, keep int) (int, error) {
	if _, err := os.Stat(root); err != nil {
		return 0, nil
	}
	names, err := entriesNewestFirst(root)
	if err != nil {
		return 0, err
	}
	var dirs []string
	for _, name := range names {
		if isSnapshotEntry(root, name) {
			dirs = append(dirs, name)
		}
	}
	deleted := 0
	for _, name := range dirs[sliceFrom(len(dirs), keep):] {
		if err := pyfs.RmTree(filepath.Join(root, name), nil); err != nil {
			slog.Warn(fmt.Sprintf("Failed to prune snapshot %s: %s", name, excText(err)))
			continue
		}
		deleted++
	}
	return deleted, nil
}

// Prunes the quick snapshots of a home down to keep and returns how many were deleted
func PruneQuickSnapshots(keep int, h string) (int, error) {
	return pruneQuickSnapshotsIn(QuickSnapshotRoot(h), keep)
}
