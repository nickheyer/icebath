//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/plugins/defaults"
	"github.com/nickheyer/icebath/pkg/pm"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// One named home or machine maintenance step
type PostUpdateStep struct {
	Name string
	Run  func(ctx context.Context) (map[string]any, error)
}

// Home steps boot runs in their order, gated by the per home record, every entry point syncing skills itself
var BootHomeSteps = []PostUpdateStep{
	{"adopt_blessed_checkout", func(ctx context.Context) (map[string]any, error) { return StepAdoptBlessedCheckout(ctx, "") }},
	{"state_db_guard", StepStateDBGuard},
	{"drop_live_plugin_catalog", StepDropLivePluginCatalog},
	{"expose_cli", StepExposeCLI},
	{"install_default_plugins", defaults.StepInstall},
}

// Gives the default plugin installer the identity of the installed code its settled mark is recorded under
func init() {
	defaults.Identity = func(ctx context.Context) string { return CurrentInstallIdentity(ctx, pm.InstallRoot()) }
}

// Explicit pm install of one package, the pm attribute tests patch
var pmEnsureFn = func(ctx context.Context, name string) error {
	_, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: true})
	return err
}

// Puts every backup that still exists back over its original, the restored originals returned
func restoreBackups(backups []backupPair) []string {
	var restored []string
	for _, b := range backups {
		if info, err := os.Stat(b.backup); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := copyFile2(b.backup, b.original); err != nil {
			continue
		}
		restored = append(restored, b.original)
	}
	return restored
}

// Verifies the active home's state.db is intact, damage reported loudly and never repaired here
func StepStateDBGuard(ctx context.Context) (map[string]any, error) {
	statePath := filepath.Join(home.Dir(ctx), "state.db")
	if !pathPresent(statePath) {
		return map[string]any{"ok": true, "skipped": "no-state-db"}, nil
	}
	result := backup.VerifySQLiteIntegrity(statePath, backup.IntegrityOptions{})
	if result.Valid {
		return map[string]any{"ok": true}, nil
	}
	message := result.Message
	if message == "" {
		message = "unknown error"
	}
	pkgLog().Error(fmt.Sprintf("state.db failed integrity check after a code update: %s — "+
		"restore a backup with `icebath backup` tooling or contact support", message))
	return map[string]any{"ok": false, "error": message}, nil
}

// Drops the active home's cached live plugin catalog after a code change
func StepDropLivePluginCatalog(ctx context.Context) (map[string]any, error) {
	plugins.InvalidateLiveCacheForHome(home.Dir(ctx))
	return map[string]any{"ok": true}, nil
}

// Python isoformat of the current UTC instant with microseconds
func isoNowUTC() string {
	now := time.Now().UTC()
	if now.Nanosecond()/1000 == 0 {
		return now.Format("2006-01-02T15:04:05+00:00")
	}
	return now.Format("2006-01-02T15:04:05.000000+00:00")
}

// Writes a JSON document to a mkstemp sibling in dir and renames it over target
func writeJSONViaTemp(dir, prefix, target string, payload *jsonutil.Object) error {
	text, err := jsonutil.Encode(payload, jsonutil.Indent(2), jsonutil.ASCII())
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, prefix+"*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(text + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

// One time adoption of a shipped stampless checkout at a blessed root, writing its birth certificate
func StepAdoptBlessedCheckout(ctx context.Context, projectRoot string) (map[string]any, error) {
	root := projectRoot
	if root == "" {
		root = pm.RepoRoot()
	}
	blessed := []string{filepath.Join(home.Dir(ctx), "icebath-agent"), "/usr/local/lib/icebath-agent"}
	if !pathPresent(filepath.Join(root, ".git")) {
		return map[string]any{"ok": true, "skipped": "not-a-checkout"}, nil
	}
	stampPath := pm.InstallStampPath(root)
	if pathPresent(stampPath) {
		return map[string]any{"ok": true, "skipped": "already-stamped"}, nil
	}
	resolvedRoot, err := pathutil.Resolve(root)
	if err != nil {
		return map[string]any{"ok": true, "skipped": "unresolvable-root"}, nil
	}
	isBlessed := false
	for _, candidate := range blessed {
		resolved, err := pathutil.Resolve(candidate)
		if err != nil {
			continue
		}
		if resolved == resolvedRoot {
			isBlessed = true
			break
		}
	}
	if !isBlessed {
		return map[string]any{"ok": true, "skipped": "not-a-blessed-root"}, nil
	}
	identified, err := WriteSourceStamp(root, true)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("blessed-checkout adoption skipped (unwritable): %s", errchain.OSText(err)))
		return map[string]any{"ok": true, "skipped": "unwritable: " + errchain.OSText(err)}, nil
	}
	if identified == nil {
		stamp := jsonutil.ObjectOf("schemaVersion", 2, "updateMechanism", "self", "source", "adoption", "adoptedAt", isoNowUTC())
		if err := writeJSONViaTemp(root, ".install-stamp.", stampPath, stamp); err != nil {
			pkgLog().Debug(fmt.Sprintf("blessed-checkout adoption skipped (unwritable): %s", errchain.OSText(err)))
			return map[string]any{"ok": true, "skipped": "unwritable: " + errchain.OSText(err)}, nil
		}
	}
	pkgLog().Info(fmt.Sprintf("adopted blessed checkout at %s (updateMechanism: self)", root))
	return map[string]any{"ok": true, "adopted": root}, nil
}

// Publishes the durable commands, over this checkout
func StepExposeCLI(ctx context.Context) (map[string]any, error) {
	result, err := pm.ExposeCLI(ctx, pm.RepoRoot(), true)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ok": result.OK}
	if result.Error != "" {
		out["error"] = result.Error
	}
	if result.Skipped != "" {
		out["skipped"] = result.Skipped
	}
	if result.Written != nil {
		written := make([]any, 0, len(result.Written))
		for _, w := range result.Written {
			written = append(written, w)
		}
		out["written"] = written
	}
	if result.Path != "" {
		out["path"] = result.Path
	}
	return out, nil
}

// Runs steps in order, a failing or raising step recorded without stopping the rest
func RunSteps(ctx context.Context, steps []PostUpdateStep) map[string]map[string]any {
	results := map[string]map[string]any{}
	for _, step := range steps {
		result, err := runStepGuarded(ctx, step)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("post-update step %s failed: %s", step.Name, err.Error()))
			results[step.Name] = map[string]any{"ok": false, "error": err.Error()}
			continue
		}
		results[step.Name] = result
	}
	return results
}

// One step with a panic read as its raised error
func runStepGuarded(ctx context.Context, step PostUpdateStep) (result map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return step.Run(ctx)
}
