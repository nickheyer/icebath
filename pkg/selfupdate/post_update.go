package selfupdate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

var postUpdateLog = slog.Default().With("logger", "icebath_cli.post_update")

// One named home or machine maintenance step
type PostUpdateStep struct {
	Name string
	Run  func(ctx context.Context) (map[string]any, error)
}

// Home steps in their hermes order, boot bootstrap gating them with the per home record
var HomeSteps = []PostUpdateStep{
	{"adopt_blessed_checkout", func(ctx context.Context) (map[string]any, error) { return StepAdoptBlessedCheckout(ctx, "") }},
	{"migrate_config", StepMigrateConfig},
	{"sync_skills", StepSyncSkills},
	{"state_db_guard", StepStateDBGuard},
	{"drop_live_plugin_catalog", StepDropLivePluginCatalog},
	{"expose_cli", StepExposeCLI},
}

// Home steps boot runs, every entry point already syncing skills itself
var BootHomeSteps = bootHomeStepsOf(HomeSteps)

// Machine steps only the explicit scope CLI runs
var MachineSteps = []PostUpdateStep{{"provision_runtimes", StepProvisionRuntimes}}

func bootHomeStepsOf(steps []PostUpdateStep) []PostUpdateStep {
	out := []PostUpdateStep{}
	for _, s := range steps {
		if s.Name != "sync_skills" {
			out = append(out, s)
		}
	}
	return out
}

// Collaborators of the config migration step, the module attributes hermes tests patch
var (
	stampMissingConfigFn     = func(h string) (bool, error) { return config.StampMissingConfig(h, provider.ConfigFacadeStampHooks()) }
	readConfigVersionStampFn = func(h string) (*int, int, error) { return config.ReadConfigVersionStamp(h, false) }
	checkConfigVersionFn     = func(h string) (int, int, error) { return config.CheckConfigVersion(h, false) }
	agentBrowserEnvPendingFn = func(h string) bool {
		return config.AgentBrowserEnvPending(h, provider.ConfigFacadeBrowserEnvSources(h))
	}
	migrateConfigFn = func(ctx context.Context, h string) (*config.MigrationResults, error) {
		return config.MigrateConfig(ctx, h, false, true)
	}
)

// Collaborators of the runtime provisioning step, the pm attributes hermes tests patch
var (
	pmDriftFn               = func(ctx context.Context) (*pm.DriftReport, error) { return pm.Drift(ctx, true) }
	pmSealedFn              = pm.Sealed
	pmLazyInstallsAllowedFn = pm.LazyInstallsAllowed
	pmSyncVenvFn            = func(ctx context.Context) error {
		return pm.SyncVenv(ctx, pm.SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true})
	}
	pmEnsureFn = func(ctx context.Context, name string) error {
		_, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: true})
		return err
	}
)

// A free backup name beside the file, bak-<stamp> then bak-<stamp>.<n>
func backupPath(path, stamp string) (string, error) {
	base := path + ".bak-" + stamp
	if !pathPresent(base) {
		return base, nil
	}
	for index := 1; index < 1000; index++ {
		candidate := fmt.Sprintf("%s.bak-%s.%d", path, stamp, index)
		if !pathPresent(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not choose a backup path for %s", path)
}

func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Copies a file with its mode and modification time like shutil.copy2
func copyFile2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// Original to backup pairs of the regular files among paths, copied with a shared timestamp
type backupPair struct{ original, backup string }

func backupExisting(paths []string) ([]backupPair, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	var backups []backupPair
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		dest, err := backupPath(path, stamp)
		if err != nil {
			return backups, err
		}
		if err := copyFile2(path, dest); err != nil {
			return backups, err
		}
		backups = append(backups, backupPair{path, dest})
	}
	return backups, nil
}

// Backup copies of the regular files among paths for callers outside the package, in path order
func BackupExisting(paths []string) ([]string, error) {
	pairs, err := backupExisting(paths)
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.backup)
	}
	return out, err
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
	if _, err := stampMissingConfigFn(h); err != nil {
		return nil, err
	}
	stamp, latest, err := readConfigVersionStampFn(h)
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
		postUpdateLog.Warn("config migration skipped: " + config.SupportFloorMessage(h))
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
	post, _, err := checkConfigVersionFn(h)
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

// Length of a list valued result field, zero for anything else
func listLength(v any) int {
	if v == nil {
		return 0
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return rv.Len()
	}
	return 0
}

// Syncs bundled skills into the active home, content diffed and convergent
func StepSyncSkills(ctx context.Context) (map[string]any, error) {
	result, err := skills.SyncSkills(ctx, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "copied": listLength(result["copied"]), "updated": listLength(result["updated"])}, nil
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
	postUpdateLog.Error(fmt.Sprintf("state.db failed integrity check after a code update: %s — "+
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
		postUpdateLog.Debug(fmt.Sprintf("blessed-checkout adoption skipped (unwritable): %s", pypath.OSErrorText(err)))
		return map[string]any{"ok": true, "skipped": "unwritable: " + pypath.OSErrorText(err)}, nil
	}
	if identified == nil {
		stamp := pyjson.ObjectOf("schemaVersion", 2, "updateMechanism", "self", "source", "adoption", "adoptedAt", pyISONowUTC())
		if err := writeJSONViaTemp(root, ".install-stamp.", stampPath, stamp); err != nil {
			postUpdateLog.Debug(fmt.Sprintf("blessed-checkout adoption skipped (unwritable): %s", pypath.OSErrorText(err)))
			return map[string]any{"ok": true, "skipped": "unwritable: " + pypath.OSErrorText(err)}, nil
		}
	}
	postUpdateLog.Info(fmt.Sprintf("adopted blessed checkout at %s (updateMechanism: self)", root))
	return map[string]any{"ok": true, "adopted": root}, nil
}

// Explicit refresh of PM's drifted packages, never selected by boot
func StepProvisionRuntimes(ctx context.Context) (map[string]any, error) {
	report, err := pmDriftFn(ctx)
	if err != nil {
		return nil, err
	}
	if report == nil || len(report.Names) == 0 {
		return map[string]any{"ok": true, "skipped": "current"}, nil
	}
	sealed, err := pmSealedFn(ctx)
	if err != nil {
		return nil, err
	}
	if sealed {
		return map[string]any{"ok": true, "skipped": "sealed"}, nil
	}
	if !pmLazyInstallsAllowedFn() {
		return map[string]any{"ok": true, "skipped": "lazy-installs-disabled"}, nil
	}
	refreshed, errors := []any{}, []string{}
	for _, name := range report.Names {
		var err error
		if name == "venv" {
			err = pmSyncVenvFn(ctx)
		} else {
			err = pmEnsureFn(ctx, name)
		}
		if err != nil {
			postUpdateLog.Warn(fmt.Sprintf("provision_runtimes: %s failed: %s", name, err.Error()))
			errors = append(errors, name+": "+err.Error())
			continue
		}
		refreshed = append(refreshed, name)
	}
	if len(errors) > 0 {
		return map[string]any{"ok": false, "error": strings.Join(errors, "; "), "refreshed": refreshed}, nil
	}
	return map[string]any{"ok": true, "refreshed": refreshed}, nil
}

// Publishes the durable commands, hermes_cli/_launchers.py expose_cli over this checkout
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
			postUpdateLog.Warn(fmt.Sprintf("post-update step %s failed: %s", step.Name, err.Error()))
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

var postUpdateScopes = map[string]bool{"home": true, "machine": true, "all": true}

// Argparse of icebath_cli.post_update, the scope and an exit code when the arguments are rejected
func parsePostUpdateArgs(argv []string, stdout, stderr io.Writer) (string, int, bool) {
	const usage = "usage: icebath_cli.post_update [-h] [--scope {home,machine,all}]\n"
	scope := "all"
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprint(stdout, usage+"\noptions:\n  -h, --help            show this help message and exit\n  --scope {home,machine,all}\n")
			return "", 0, true
		case arg == "--scope":
			if i+1 >= len(argv) {
				fmt.Fprint(stderr, usage+"icebath_cli.post_update: error: argument --scope: expected one argument\n")
				return "", 2, true
			}
			i++
			scope = argv[i]
		case strings.HasPrefix(arg, "--scope="):
			scope = strings.TrimPrefix(arg, "--scope=")
		default:
			fmt.Fprintf(stderr, "%sicebath_cli.post_update: error: unrecognized arguments: %s\n", usage, strings.Join(argv[i:], " "))
			return "", 2, true
		}
		if !postUpdateScopes[scope] {
			fmt.Fprintf(stderr, "%sicebath_cli.post_update: error: argument --scope: invalid choice: '%s' (choose from 'home', 'machine', 'all')\n", usage, scope)
			return "", 2, true
		}
	}
	return scope, 0, false
}

// Runs the selected maintenance registry and reports its failures, exit 1 when any step failed
func PostUpdateMain(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	scope, code, done := parsePostUpdateArgs(argv, stdout, stderr)
	if done {
		return code
	}
	var selected []PostUpdateStep
	if scope == "home" || scope == "all" {
		selected = append(selected, HomeSteps...)
	}
	if scope == "machine" || scope == "all" {
		selected = append(selected, MachineSteps...)
	}
	results := RunSteps(ctx, selected)
	failed := false
	for _, step := range selected {
		res := results[step.Name]
		ok := config.PyTruthy(res["ok"])
		if !ok {
			failed = true
		}
		state := "ok"
		if !ok {
			state = fmt.Sprintf("FAILED (%v)", res["error"])
		}
		if skipped := res["skipped"]; config.PyTruthy(skipped) {
			state = fmt.Sprintf("skipped (%v)", skipped)
		}
		fmt.Fprintf(stdout, "  post-update %s: %s\n", step.Name, state)
	}
	if failed {
		return 1
	}
	return 0
}
