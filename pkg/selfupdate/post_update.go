//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One named home or machine maintenance step
type PostUpdateStep struct {
	Name string
	Run  func(ctx context.Context) (map[string]any, error)
}

// Home steps boot runs in their order, gated by the per home record, every entry point syncing skills itself
var BootHomeSteps = []PostUpdateStep{
	{"adopt_blessed_checkout", func(ctx context.Context) (map[string]any, error) { return StepAdoptBlessedCheckout(ctx, "") }},
	{"migrate_config", StepMigrateConfig},
	{"state_db_guard", StepStateDBGuard},
	{"drop_live_plugin_catalog", StepDropLivePluginCatalog},
	{"expose_cli", StepExposeCLI},
}

// Collaborators of the config migration step, the module attributes tests patch
var (
	stampMissingConfigFn = func(ctx context.Context, h string) (bool, error) {
		return config.StampMissingConfig(ctx, h, provider.ConfigFacadeStampHooks())
	}
	readConfigVersionStampFn = func(ctx context.Context, h string) (*int, int, error) {
		return config.ReadConfigVersionStamp(ctx, h, false)
	}
	checkConfigVersionFn     = func(ctx context.Context, h string) (int, int, error) { return config.CheckConfigVersion(ctx, h, false) }
	agentBrowserEnvPendingFn = func(h string) bool {
		return config.AgentBrowserEnvPending(h, provider.ConfigFacadeBrowserEnvSources(h))
	}
	migrateConfigFn = func(ctx context.Context, h string) (*config.MigrationResults, error) {
		return config.MigrateConfig(ctx, h, false, true)
	}
)

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

// Migrates config.yaml to the current schema non interactively, backups restored when the version does not advance
func StepMigrateConfig(ctx context.Context) (map[string]any, error) {
	h := home.Dir(ctx)
	if _, err := stampMissingConfigFn(ctx, h); err != nil {
		return nil, err
	}
	stamp, latest, err := readConfigVersionStampFn(ctx, h)
	if err != nil {
		return nil, err
	}
	current := 0
	if stamp != nil {
		current = *stamp
	}
	if current >= latest {
		return map[string]any{"ok": true, "skipped": "up-to-date"}, nil
	}
	if current < config.SupportFloorVersion && !(stamp == nil && agentBrowserEnvPendingFn(h)) {
		pkgLog().Warn("config migration skipped: " + config.SupportFloorMessage(h))
		return map[string]any{"ok": true, "skipped": "below-support-floor"}, nil
	}
	backups, err := backupExisting([]string{home.ConfigPath(h), home.EnvPath(h)})
	if err != nil {
		return nil, err
	}
	results, err := migrateConfigFn(ctx, h)
	if err != nil {
		restoreBackups(backups)
		return nil, err
	}
	post, _, err := checkConfigVersionFn(ctx, h)
	if err != nil {
		return nil, err
	}
	if post < latest {
		restored := restoreBackups(backups)
		names := "none"
		if len(restored) > 0 {
			names = strings.Join(restored, ", ")
		}
		return nil, fmt.Errorf("migration did not advance config version to %d (still %d); restored: %s", latest, post, names)
	}
	if results != nil {
		for _, notice := range results.Notices {
			provider.DeferStartupNotice(ctx, notice)
		}
	}
	return map[string]any{"ok": true, "migrated": fmt.Sprintf("%d->%d", current, latest)}, nil
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
func pyISONowUTC() string {
	now := time.Now().UTC()
	if now.Nanosecond()/1000 == 0 {
		return now.Format("2006-01-02T15:04:05+00:00")
	}
	return now.Format("2006-01-02T15:04:05.000000+00:00")
}

// Writes a JSON document to a mkstemp sibling in dir and renames it over target
func writeJSONViaTemp(dir, prefix, target string, payload *pyjson.Object) error {
	text, err := pyjson.Dumps(payload, pyjson.Indent(2))
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
	resolvedRoot, err := pypath.Resolve(root)
	if err != nil {
		return map[string]any{"ok": true, "skipped": "unresolvable-root"}, nil
	}
	isBlessed := false
	for _, candidate := range blessed {
		resolved, err := pypath.Resolve(candidate)
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
		stamp := pyjson.ObjectOf("schemaVersion", 2, "updateMechanism", "self", "source", "adoption", "adoptedAt", pyISONowUTC())
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
