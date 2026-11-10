package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Config paths an update must never change, a one element path protecting a whole section
var protectedConfigPaths = [][]string{
	{"model", "provider"},
	{"model", "default"},
	{"model", "base_url"},
	{"model", "api_key"},
	{"moa"},
}

// Home a restore entry point works on, the process home when none is given
func restoreHome(ctx context.Context, h string) string {
	if h == "" {
		return home.Dir(ctx)
	}
	return h
}

// Reports whether path is a symlink resolving to the default root auth store of a named profile home
func isTrustedRootAuthAlias(path, h string) bool {
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSymlink == 0 {
		return false
	}
	root, err := pypath.Resolve(home.RootOf(h))
	if err != nil {
		return false
	}
	resolvedHome, err := pypath.Resolve(h)
	if err != nil || resolvedHome == root {
		return false
	}
	trusted, err := pypath.Resolve(pypath.PathJoin(root, "auth.json"))
	if err != nil {
		return false
	}
	if _, under := pypath.PathRelativeTo(trusted, root); !under {
		return false
	}
	resolved, err := pypath.Resolve(path)
	return err == nil && resolved == trusted
}

// Restores a whole quick snapshot offline, false when the profile is owned or any listed file is refused
func RestoreQuickSnapshot(ctx context.Context, snapshotID, h string) (bool, error) {
	if err := config.RequireFileTooling(ctx, "Snapshot restore"); err != nil {
		return false, err
	}
	h = restoreHome(ctx, h)
	inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{h}, "")
	if err != nil {
		if isOwnershipConflict(err) {
			slog.Error(err.Error())
			return false, nil
		}
		return false, err
	}
	defer release()
	ok, err := restoreQuickSnapshotExclusive(inner, snapshotID, h)
	if err != nil && isOwnershipConflict(err) {
		slog.Error(err.Error())
		return false, nil
	}
	return ok, err
}

// Source and destination of one manifest entry, false when either escapes its tree
type entryPathsFunc func(rel string) (src, dst string, ok bool, err error)

// False with the error logged when a listed state.db destination cannot prove its runtime epoch
func snapshotEpochsRestorable(ctx context.Context, files []string, entryPaths entryPathsFunc) (bool, error) {
	for _, rel := range files {
		src, dst, ok, err := entryPaths(rel)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		isState := pypath.PathName(dst) == "state.db"
		if !isState {
			resolved, err := pypath.Resolve(dst)
			if err != nil {
				return false, err
			}
			isState = pypath.PathName(resolved) == "state.db"
		}
		if !isState {
			continue
		}
		if _, err := restoreDestinationEpoch(ctx, dst); err != nil {
			if isOSError(err) {
				slog.Error(excText(err))
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// Publishes one snapshot file over dst, false with the reason logged when it is refused or fails
func restoreSnapshotEntry(ctx context.Context, rel, src, dst string) (bool, error) {
	err := func() error {
		switch {
		case pypath.PathSuffix(dst) == ".db":
			ok, err := safeRestoreDB(ctx, src, dst)
			if err != nil {
				return err
			}
			if !ok {
				slog.Error(fmt.Sprintf("Failed to restore %s: refused or source integrity check failed (see previous log)", rel))
				return errEntryRefused
			}
		case rel == "auth.json":
			if !restoreAuthJSON(src, dst) {
				slog.Error(fmt.Sprintf("Failed to restore %s safely", rel))
				return errEntryRefused
			}
		default:
			return pyfs.Copy2(src, dst)
		}
		return nil
	}()
	if errors.Is(err, errEntryRefused) {
		return false, nil
	}
	if err != nil {
		if isOSError(err) {
			slog.Error(fmt.Sprintf("Failed to restore %s: %s", rel, excText(err)))
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Refusal of one snapshot entry already logged by its restore step
var errEntryRefused = errors.New("snapshot entry refused")

// Manifest file names in listing order, the keys of a mapping or the items of a list
func manifestFiles(meta any) ([]string, error) {
	obj, ok := meta.(*pyjson.Object)
	if !ok {
		return nil, &pyValueError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(meta))}
	}
	files, ok := obj.Get("files")
	if !ok {
		return nil, nil
	}
	switch t := files.(type) {
	case *pyjson.Object:
		return append([]string(nil), t.Keys...), nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, &pyValueError{"TypeError", fmt.Sprintf("unsupported operand type(s) for /: 'PosixPath' and '%s'", pyrepr.TypeName(item))}
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		out := make([]string, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &pyValueError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(files))}
}

// Restores a quick snapshot while the home is reserved, every state.db epoch proven before any file changes
func restoreQuickSnapshotExclusive(ctx context.Context, snapshotID, h string) (bool, error) {
	snapDir, err := QuickSnapshotDir(snapshotID, h)
	if err != nil || snapDir == "" {
		return false, err
	}
	raw, err := os.ReadFile(pypath.PathJoin(snapDir, "manifest.json"))
	if err != nil {
		return false, err
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return false, err
	}
	meta, err := pyjson.Loads(text)
	if err != nil {
		return false, err
	}
	files, err := manifestFiles(meta)
	if err != nil {
		return false, err
	}
	snapRes, err := pypath.Resolve(snapDir)
	if err != nil {
		return false, err
	}
	homeRes, err := pypath.Resolve(h)
	if err != nil {
		return false, err
	}
	entryPaths := func(rel string) (string, string, bool, error) {
		src, dst := pypath.PathJoin(snapDir, rel), pypath.PathJoin(h, rel)
		resolved, err := pypath.Resolve(src)
		if err != nil {
			return "", "", false, err
		}
		if _, under := pypath.PathRelativeTo(resolved, snapRes); !under {
			return "", "", false, nil
		}
		if resolved, err = pypath.Resolve(dst); err != nil {
			return "", "", false, err
		}
		if _, under := pypath.PathRelativeTo(resolved, homeRes); !under {
			if rel != "auth.json" || !isTrustedRootAuthAlias(dst, h) {
				return "", "", false, nil
			}
		}
		return src, dst, true, nil
	}
	seen := map[string]bool{}
	homes := addHome(nil, seen, h)
	for _, rel := range files {
		_, dst, ok, err := entryPaths(rel)
		if err != nil {
			return false, err
		}
		if ok && pypath.PathSuffix(dst) == ".db" {
			abs, err := pathAbsolute(dst)
			if err != nil {
				return false, err
			}
			resolved, err := pypath.Resolve(dst)
			if err != nil {
				return false, err
			}
			homes = addHome(homes, seen, pypath.PathParent(abs))
			homes = addHome(homes, seen, pypath.PathParent(resolved))
		}
	}
	inner, release, err := gateway.ExclusiveMaintenance(ctx, homes, "")
	if err != nil {
		return false, err
	}
	restored, authFailed, epochsOK, err := func() (int, bool, bool, error) {
		defer release()
		ok, err := snapshotEpochsRestorable(inner, files, entryPaths)
		if err != nil || !ok {
			return 0, false, false, err
		}
		restored, authFailed := 0, false
		for _, rel := range files {
			src, dst, ok, err := entryPaths(rel)
			if err != nil {
				return 0, false, false, err
			}
			if !ok {
				slog.Error("Manifest path traversal blocked: " + rel)
				if rel == "auth.json" {
					authFailed = true
				}
				continue
			}
			if _, err := os.Stat(src); err != nil {
				if rel == "auth.json" {
					slog.Error("Snapshot auth.json listed in manifest is missing: " + pypath.PathStr(src))
					authFailed = true
				}
				continue
			}
			if err := home.PathMkdir(pypath.PathParent(dst), 0o777, true, true); err != nil {
				return 0, false, false, err
			}
			done, err := restoreSnapshotEntry(inner, rel, src, dst)
			if err != nil {
				return 0, false, false, err
			}
			if done {
				restored++
			} else if rel == "auth.json" {
				authFailed = true
			}
		}
		return restored, authFailed, true, nil
	}()
	if err != nil || !epochsOK {
		return false, err
	}
	slog.Info(fmt.Sprintf("Restored %d files from snapshot %s", restored, snapshotID))
	return restored > 0 && !authFailed, nil
}

// Restores cron/jobs.json from the pre-update snapshot when the live file lost jobs, nil when nothing was done
func RestoreCronJobsIfEmptied(snapshotID, h string) (*pyjson.Object, error) {
	if snapshotID == "" {
		return nil, nil
	}
	h = restoreHome(context.Background(), h)
	livePath := pypath.PathJoin(h, cronJobsRel)
	liveCount, err := countCronJobs(livePath)
	if err != nil || liveCount == nil {
		return nil, err
	}
	snapPath := pypath.PathJoin(QuickSnapshotRoot(h), snapshotID, cronJobsRel)
	snapCount, err := countCronJobs(snapPath)
	if err != nil || snapCount == nil || *snapCount == 0 {
		return nil, err
	}
	if *liveCount >= *snapCount {
		return nil, nil
	}
	err = home.PathMkdir(pypath.PathParent(livePath), 0o777, true, true)
	if err == nil {
		err = pyfs.Copy2(snapPath, livePath)
	}
	if err != nil {
		if isOSError(err) {
			slog.Error("Cron jobs were emptied during update but auto-restore failed: " + excText(err))
			return nil, nil
		}
		return nil, err
	}
	slog.Warn(fmt.Sprintf("Restored %d cron job(s) from pre-update snapshot %s "+
		"(live file had %d job(s), snapshot had %d — jobs were lost during migration)",
		*snapCount, snapshotID, *liveCount, *snapCount))
	result := pyjson.NewObject()
	result.Set("restored", true)
	result.Set("job_count", int64(*snapCount))
	result.Set("snapshot_id", snapshotID)
	return result, nil
}

// YAML mapping of a file, nil when it is missing, unreadable or not a mapping
func readRawYAMLDict(path string) map[string]any {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil
	}
	data, err := config.FastSafeLoad([]byte(text))
	if err != nil {
		return nil
	}
	m, _ := data.(map[string]any)
	return m
}

// Reports whether a snapshot value is one the user never set, None, empty text or an empty container
func unsetConfigValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// File tooling check of the config backend, a seam tests replace with a remote backend answer
var supportsFileTooling = config.SupportsFileTooling

// Restores the protected model and MoA keys an update rewrote, nil when nothing was done
func RestoreConfigModelSettingsIfRewritten(ctx context.Context, snapshotID, h string) (*pyjson.Object, error) {
	if snapshotID == "" {
		return nil, nil
	}
	supported, err := supportsFileTooling(ctx)
	if err != nil || !supported {
		return nil, err
	}
	h = restoreHome(ctx, h)
	livePath := pypath.PathJoin(h, "config.yaml")
	snap := readRawYAMLDict(pypath.PathJoin(QuickSnapshotRoot(h), snapshotID, "config.yaml"))
	if len(snap) == 0 {
		return nil, nil
	}
	live := readRawYAMLDict(livePath)
	if live == nil {
		return nil, nil
	}
	var restoredKeys []string
	for _, dotted := range protectedConfigPaths {
		snapVal := getConfigPathValue(snap, dotted)
		if unsetConfigValue(snapVal) {
			continue
		}
		if pyrepr.Equal(getConfigPathValue(live, dotted), snapVal) {
			continue
		}
		setConfigPathValue(live, dotted, snapVal)
		restoredKeys = append(restoredKeys, strings.Join(dotted, "."))
	}
	if len(restoredKeys) == 0 {
		return nil, nil
	}
	if err := config.AtomicConfigWrite(ctx, h, livePath, live, ""); err != nil {
		if isOSError(err) {
			slog.Error("config.yaml model settings were rewritten during update but auto-restore failed: " + excText(err))
			return nil, nil
		}
		return nil, err
	}
	slog.Warn(fmt.Sprintf("Restored user config value(s) %s from pre-update snapshot %s — the update flow rewrote them (#64160)",
		strings.Join(restoredKeys, ", "), snapshotID))
	keys := make([]any, len(restoredKeys))
	for i, k := range restoredKeys {
		keys[i] = k
	}
	result := pyjson.NewObject()
	result.Set("restored", true)
	result.Set("keys", keys)
	result.Set("snapshot_id", snapshotID)
	return result, nil
}

// Runs a per profile safety net over every sibling profile against its own snapshot, failures logged and skipped
func restoreAllProfiles(profileSnapshots *pyjson.Object, invokingHome, what string, restore func(snapID, h string) (*pyjson.Object, error)) []*pyjson.Object {
	restored := []*pyjson.Object{}
	if profileSnapshots == nil || profileSnapshots.Len() == 0 {
		return restored
	}
	invokingHome = restoreHome(context.Background(), invokingHome)
	byName := map[string]string{}
	for _, pair := range SiblingProfileHomes(invokingHome) {
		byName[pair[0]] = pair[1]
	}
	for _, name := range profileSnapshots.Keys {
		profileHome, ok := byName[name]
		if !ok {
			continue
		}
		raw, _ := profileSnapshots.Get(name)
		snapID, _ := raw.(string)
		result, err := restore(snapID, profileHome)
		if err != nil {
			slog.Debug(fmt.Sprintf("%s for profile %s failed: %s", what, name, excText(err)))
			continue
		}
		if result != nil && result.Len() > 0 {
			result.Set("profile", name)
			restored = append(restored, result)
		}
	}
	return restored
}

// Config model settings safety net of every sibling profile, one result per restored profile
func RestoreConfigModelSettingsAllProfiles(ctx context.Context, profileSnapshots *pyjson.Object, invokingHome string) []*pyjson.Object {
	return restoreAllProfiles(profileSnapshots, invokingHome, "Config model-settings restore check", func(snapID, h string) (*pyjson.Object, error) {
		return RestoreConfigModelSettingsIfRewritten(ctx, snapID, h)
	})
}

// Cron jobs safety net of every sibling profile, one result per restored profile
func RestoreCronJobsAllProfiles(profileSnapshots *pyjson.Object, invokingHome string) []*pyjson.Object {
	return restoreAllProfiles(profileSnapshots, invokingHome, "Cron restore check", RestoreCronJobsIfEmptied)
}
