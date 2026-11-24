package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
	root, err := pathutil.Resolve(home.RootOf(h))
	if err != nil {
		return false
	}
	resolvedHome, err := pathutil.Resolve(h)
	if err != nil || resolvedHome == root {
		return false
	}
	trusted, err := pathutil.Resolve(pathutil.Join(root, "auth.json"))
	if err != nil {
		return false
	}
	if _, under := pathutil.RelativeTo(trusted, root); !under {
		return false
	}
	resolved, err := pathutil.Resolve(path)
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
			pkgLog().Error(err.Error())
			return false, nil
		}
		return false, err
	}
	defer release()
	ok, err := restoreQuickSnapshotExclusive(inner, snapshotID, h)
	if err != nil && isOwnershipConflict(err) {
		pkgLog().Error(err.Error())
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
		isState := pathutil.Name(dst) == "state.db"
		if !isState {
			resolved, err := pathutil.Resolve(dst)
			if err != nil {
				return false, err
			}
			isState = pathutil.Name(resolved) == "state.db"
		}
		if !isState {
			continue
		}
		if _, err := restoreDestinationEpoch(ctx, dst); err != nil {
			if isRestoreFailure(err) {
				pkgLog().Error(excText(err))
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
		case pathutil.Suffix(dst) == ".db":
			ok, err := safeRestoreDB(ctx, src, dst)
			if err != nil {
				return err
			}
			if !ok {
				pkgLog().Error(fmt.Sprintf("Failed to restore %s: refused or source integrity check failed (see previous log)", rel))
				return errEntryRefused
			}
		case rel == "auth.json":
			if !restoreAuthJSON(src, dst) {
				pkgLog().Error(fmt.Sprintf("Failed to restore %s safely", rel))
				return errEntryRefused
			}
		default:
			if err := fsutil.CopyFile(src, dst); err != nil {
				return err
			}
			// The copy carries the mode the snapshot was taken with, and the secrets file is owner only as an import leaves it
			switch pathutil.Name(dst) {
			case "config.yaml":
				config.DropOtherAccess(ctx, dst)
			case ".env":
				return os.Chmod(dst, 0o600)
			}
		}
		return nil
	}()
	if errors.Is(err, errEntryRefused) {
		return false, nil
	}
	if err != nil {
		if isRestoreFailure(err) {
			pkgLog().Error(fmt.Sprintf("Failed to restore %s: %s", rel, excText(err)))
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
	obj, ok := meta.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("snapshot manifest is %s, not an object", anyutil.Kind(meta))
	}
	files, ok := obj.Get("files")
	if !ok {
		return nil, nil
	}
	switch t := files.(type) {
	case *jsonutil.Object:
		return append([]string(nil), t.Keys()...), nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("snapshot manifest lists a file name that is %s, not a string", anyutil.Kind(item))
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
	return nil, fmt.Errorf("snapshot manifest files are %s, not a list, an object or a string", anyutil.Kind(files))
}

// Restores a quick snapshot while the home is reserved, every state.db epoch proven before any file changes
func restoreQuickSnapshotExclusive(ctx context.Context, snapshotID, h string) (bool, error) {
	snapDir, err := QuickSnapshotDir(snapshotID, h)
	if err != nil || snapDir == "" {
		return false, err
	}
	raw, err := os.ReadFile(pathutil.Join(snapDir, "manifest.json"))
	if err != nil {
		return false, err
	}
	text, err := charsetutil.Decode(raw, "utf-8-sig")
	if err != nil {
		return false, err
	}
	meta, err := jsonutil.Decode(text)
	if err != nil {
		return false, err
	}
	files, err := manifestFiles(meta)
	if err != nil {
		return false, err
	}
	snapRes, err := pathutil.Resolve(snapDir)
	if err != nil {
		return false, err
	}
	homeRes, err := pathutil.Resolve(h)
	if err != nil {
		return false, err
	}
	entryPaths := func(rel string) (string, string, bool, error) {
		src, dst := pathutil.Join(snapDir, rel), pathutil.Join(h, rel)
		resolved, err := pathutil.Resolve(src)
		if err != nil {
			return "", "", false, err
		}
		if _, under := pathutil.RelativeTo(resolved, snapRes); !under {
			return "", "", false, nil
		}
		if resolved, err = pathutil.Resolve(dst); err != nil {
			return "", "", false, err
		}
		if _, under := pathutil.RelativeTo(resolved, homeRes); !under {
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
		if ok && pathutil.Suffix(dst) == ".db" {
			abs, err := pathAbsolute(dst)
			if err != nil {
				return false, err
			}
			resolved, err := pathutil.Resolve(dst)
			if err != nil {
				return false, err
			}
			homes = addHome(homes, seen, filepath.Dir(filepath.Clean(abs)))
			homes = addHome(homes, seen, filepath.Dir(filepath.Clean(resolved)))
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
				pkgLog().Error("Manifest path traversal blocked: " + rel)
				if rel == "auth.json" {
					authFailed = true
				}
				continue
			}
			if _, err := os.Stat(src); err != nil {
				if rel == "auth.json" {
					pkgLog().Error("Snapshot auth.json listed in manifest is missing: " + filepath.Clean(src))
					authFailed = true
				}
				continue
			}
			if err := home.PathMkdir(filepath.Dir(filepath.Clean(dst)), 0o777, true, true); err != nil {
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
	pkgLog().Info(fmt.Sprintf("Restored %d files from snapshot %s", restored, snapshotID))
	return restored > 0 && !authFailed, nil
}

// Restores cron/jobs.json from the pre-update snapshot when the live file lost jobs, nil when nothing was done
func RestoreCronJobsIfEmptied(snapshotID, h string) (*jsonutil.Object, error) {
	if snapshotID == "" {
		return nil, nil
	}
	h = restoreHome(context.Background(), h)
	livePath := pathutil.Join(h, cronJobsRel)
	liveCount, err := countCronJobs(livePath)
	if err != nil || liveCount == nil {
		return nil, err
	}
	snapPath := pathutil.Join(QuickSnapshotRoot(h), snapshotID, cronJobsRel)
	snapCount, err := countCronJobs(snapPath)
	if err != nil || snapCount == nil || *snapCount == 0 {
		return nil, err
	}
	if *liveCount >= *snapCount {
		return nil, nil
	}
	err = home.PathMkdir(filepath.Dir(filepath.Clean(livePath)), 0o777, true, true)
	if err == nil {
		err = fsutil.CopyFile(snapPath, livePath)
	}
	if err != nil {
		if isRestoreFailure(err) {
			pkgLog().Error("Cron jobs were emptied during update but auto-restore failed: " + excText(err))
			return nil, nil
		}
		return nil, err
	}
	pkgLog().Warn(fmt.Sprintf("Restored %d cron job(s) from pre-update snapshot %s "+
		"(live file had %d job(s), snapshot had %d — jobs were lost during migration)",
		*snapCount, snapshotID, *liveCount, *snapCount))
	result := jsonutil.NewObject()
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
	text, err := charsetutil.Decode(raw, "utf-8-sig")
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
func RestoreConfigModelSettingsIfRewritten(ctx context.Context, snapshotID, h string) (*jsonutil.Object, error) {
	if snapshotID == "" {
		return nil, nil
	}
	supported, err := supportsFileTooling(ctx)
	if err != nil || !supported {
		return nil, err
	}
	h = restoreHome(ctx, h)
	livePath := pathutil.Join(h, "config.yaml")
	snap := readRawYAMLDict(pathutil.Join(QuickSnapshotRoot(h), snapshotID, "config.yaml"))
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
		if anyutil.Equal(getConfigPathValue(live, dotted), snapVal) {
			continue
		}
		setConfigPathValue(live, dotted, snapVal)
		restoredKeys = append(restoredKeys, strings.Join(dotted, "."))
	}
	if len(restoredKeys) == 0 {
		return nil, nil
	}
	if err := config.AtomicConfigWrite(ctx, h, livePath, live, ""); err != nil {
		if isRestoreFailure(err) {
			pkgLog().Error("config.yaml model settings were rewritten during update but auto-restore failed: " + excText(err))
			return nil, nil
		}
		return nil, err
	}
	pkgLog().Warn(fmt.Sprintf("Restored user config value(s) %s from pre-update snapshot %s — the update flow rewrote them (#64160)",
		strings.Join(restoredKeys, ", "), snapshotID))
	keys := make([]any, len(restoredKeys))
	for i, k := range restoredKeys {
		keys[i] = k
	}
	result := jsonutil.NewObject()
	result.Set("restored", true)
	result.Set("keys", keys)
	result.Set("snapshot_id", snapshotID)
	return result, nil
}

// Runs a per profile safety net over every sibling profile against its own snapshot, failures logged and skipped
func restoreAllProfiles(ctx context.Context, profileSnapshots *jsonutil.Object, invokingHome, what string, restore func(snapID, h string) (*jsonutil.Object, error)) []*jsonutil.Object {
	restored := []*jsonutil.Object{}
	if profileSnapshots == nil || profileSnapshots.Len() == 0 {
		return restored
	}
	invokingHome = restoreHome(ctx, invokingHome)
	byName := map[string]string{}
	for _, pair := range SiblingProfileHomes(invokingHome) {
		byName[pair[0]] = pair[1]
	}
	for _, name := range profileSnapshots.Keys() {
		profileHome, ok := byName[name]
		if !ok {
			continue
		}
		raw, _ := profileSnapshots.Get(name)
		snapID, _ := raw.(string)
		result, err := restore(snapID, profileHome)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("%s for profile %s failed: %s", what, name, excText(err)))
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
func RestoreConfigModelSettingsAllProfiles(ctx context.Context, profileSnapshots *jsonutil.Object, invokingHome string) []*jsonutil.Object {
	return restoreAllProfiles(ctx, profileSnapshots, invokingHome, "Config model-settings restore check", func(snapID, h string) (*jsonutil.Object, error) {
		return RestoreConfigModelSettingsIfRewritten(ctx, snapID, h)
	})
}

// Cron jobs safety net of every sibling profile, one result per restored profile
func RestoreCronJobsAllProfiles(profileSnapshots *jsonutil.Object, invokingHome string) []*jsonutil.Object {
	return restoreAllProfiles(context.Background(), profileSnapshots, invokingHome, "Cron restore check", RestoreCronJobsIfEmptied)
}
