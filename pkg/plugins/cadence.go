package plugins

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const updateCheckMarkersDir = "plugin-update-checks"

const defaultUpdateCheckIntervalHours = 24.0

func updateCheckMarkerPath(h string) string {
	return filepath.Join(h, updateCheckMarkersDir, "last-run")
}

// Markers dir created on demand for write paths
func updateCheckMarkersDirOf(h string) (string, error) {
	d := filepath.Dir(updateCheckMarkerPath(h))
	return d, os.MkdirAll(d, 0o755)
}

// Install records an update check compares against, read before the check runs
func snapshotInstalls(pluginsDir string) map[string]any { return ReadSidecarRows(pluginsDir) }

// Persists a finished check with the install record each row was checked against, never failing
func recordCheckResults(ctx context.Context, results []*CheckResult, checked map[string]any) {
	rows := make([]any, 0, len(results))
	for _, r := range results {
		row := checkResultObject(r)
		row.Set("checked_install", installFingerprint(checked[r.Name]))
		rows = append(rows, row)
	}
	dir, err := updateCheckMarkersDirOf(home.Dir(ctx))
	if err != nil {
		return
	}
	text, err := pyjson.Dumps(rows, pyjson.Indent(2))
	if err != nil {
		return
	}
	_ = atomicWrite(filepath.Join(dir, "results.json"), []byte(text+"\n"), 0o644)
}

// Source, revision and pinned parts of an install record, nil for a non mapping
func installFingerprint(record any) any {
	m, ok := record.(map[string]any)
	if !ok {
		return nil
	}
	o := pyjson.NewObject()
	o.Set("source", m["source"])
	o.Set("revision", m["revision"])
	o.Set("pinned", m["pinned"] == true)
	return o
}

// Last check results file beside the last run marker, derived without creating anything
func updateCheckResultsPath(h string) string {
	return filepath.Join(filepath.Dir(updateCheckMarkerPath(h)), "results.json")
}

// Plugin names mapped to the commit or feed version the last check found for still matching git installs
func CachedGitUpdates(ctx context.Context, installMetadata map[string]any) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(updateCheckResultsPath(home.Dir(ctx)))
	if err != nil {
		return out
	}
	rows, err := pyjson.Loads(string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	if err != nil {
		return out
	}
	list, _ := rows.([]any)
	for _, item := range list {
		row, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		class, _ := row.Get("class")
		available, _ := row.Get("update_available")
		if !pyrepr.Equal(class, "git") || available != true {
			continue
		}
		name, _ := row.Get("name")
		var record any
		if key, ok := name.(string); ok {
			record = installMetadata[key]
		}
		current, _ := installFingerprint(record).(*pyjson.Object)
		checked, _ := row.Get("checked_install")
		latest, _ := row.Get("latest")
		if current != nil {
			if pinned, _ := current.Get("pinned"); pinned != true && pyrepr.Equal(current, checked) && pyrepr.Truthy(latest) {
				out[pyrepr.Str(name)] = pyrepr.Str(latest)
			}
		}
	}
	return out
}

// Reads one plugins config key for the cadence, an error standing for a raising reader
type CadenceConfigGet func(section, key string) (any, error)

// Config reader over the read only home config, nil for any key it cannot read
func defaultCadenceConfigGet(ctx context.Context) CadenceConfigGet {
	return func(section, key string) (any, error) {
		cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
		if err != nil {
			return nil, nil
		}
		value, _ := config.CfgGet(cfg, section, key)
		return value, nil
	}
}

// Hours between update checks from plugins.auto_update_check_hours, zero disabling and 24 by default
func CheckIntervalHours(ctx context.Context, get CadenceConfigGet) float64 {
	if get == nil {
		get = defaultCadenceConfigGet(ctx)
	}
	value, err := get("plugins", "auto_update_check_hours")
	if err != nil || value == nil {
		return defaultUpdateCheckIntervalHours
	}
	hours, ok := pyFloat(value)
	if !ok {
		return defaultUpdateCheckIntervalHours
	}
	if math.IsNaN(hours) || !(hours > 0) {
		return 0
	}
	return hours
}

// Whether plugins.auto_apply opts into unattended git updates, false when the reader fails
func AutoApplyEnabled(ctx context.Context, get CadenceConfigGet) bool {
	if get == nil {
		get = defaultCadenceConfigGet(ctx)
	}
	value, err := get("plugins", "auto_apply")
	return err == nil && pyrepr.Truthy(value)
}

// Whether the interval has passed since the last run marker, true when no marker exists
func CheckDue(ctx context.Context, now, intervalHours *float64, get CadenceConfigGet) bool {
	interval := 0.0
	if intervalHours != nil {
		interval = *intervalHours
	} else {
		interval = CheckIntervalHours(ctx, get)
	}
	if interval <= 0 {
		return false
	}
	at := epochNow()
	if now != nil {
		at = *now
	}
	info, err := os.Stat(updateCheckMarkerPath(home.Dir(ctx)))
	if err != nil {
		return true
	}
	last := float64(info.ModTime().UnixNano()) / 1e9
	return at-last >= interval*3600
}

// Process wide singleflight so overlapping gateway ticks never double fetch or double write receipts
var cadenceTickMu sync.Mutex

// Seams of one scheduled update check, nil fields left to the caller defaults
type ScheduledCheckOptions struct {
	RunChecks   func(ctx context.Context, pluginsDir string) ([]*CheckResult, error)
	PluginsDir  string
	ApplyUpdate func(ctx context.Context, name string) error
	Log         *slog.Logger
	ConfigGet   CadenceConfigGet
	Now         *float64
}

// One cadence tick of gate, check, receipt and opt in apply, false when not due, disabled or already running
func RunScheduledCheck(ctx context.Context, opts ScheduledCheckOptions) ([]*CheckResult, bool) {
	if opts.Log == nil {
		opts.Log = Logger
	}
	if !CheckDue(ctx, opts.Now, nil, opts.ConfigGet) {
		return nil, false
	}
	if !cadenceTickMu.TryLock() {
		opts.Log.Debug("plugin update check already in flight — skipping tick")
		return nil, false
	}
	defer cadenceTickMu.Unlock()
	return runCheckLocked(ctx, opts), true
}

// Check, receipt, one actionable log line, opt in git apply and the marker stamp of a due tick
func runCheckLocked(ctx context.Context, opts ScheduledCheckOptions) []*CheckResult {
	checkOK, warning := true, ""
	checked := snapshotInstalls(opts.PluginsDir)
	results, err := callCheck(ctx, opts)
	if err != nil {
		warning = "plugin update check failed"
		opts.Log.Warn(warning, "error", err)
		results, checkOK = nil, false
	}
	if checkOK {
		recordCheckResults(ctx, results, checked)
	}
	var updates, needsFixing []*CheckResult
	for _, r := range results {
		if r.UpdateAvailable != nil && *r.UpdateAvailable {
			updates = append(updates, r)
		}
		if r.NeedsFixing != "" {
			needsFixing = append(needsFixing, r)
		}
	}
	writeCheckReceipt(ctx, opts.Log, results, warning, checkOK, len(updates) > 0)
	if len(updates) > 0 {
		opts.Log.Info(fmt.Sprintf("plugin updates available: %s — run `icebath plugins check-updates` "+
			"and `icebath plugins update <name>`", checkNames(updates)))
	}
	if len(needsFixing) > 0 {
		opts.Log.Warn(fmt.Sprintf("plugin update_url mismatches need attention: %s — run "+
			"`icebath plugins trust-update-url <name>` after review", checkNames(needsFixing)))
	}
	if opts.ApplyUpdate != nil && len(updates) > 0 && AutoApplyEnabled(ctx, opts.ConfigGet) {
		for _, r := range updates {
			if r.Class == "git" {
				if err := applyGuarded(ctx, opts.ApplyUpdate, r.Name); err != nil {
					opts.Log.Warn(fmt.Sprintf("auto-apply of %s failed", r.Name), "error", err)
				}
			}
		}
	}
	if dir, err := updateCheckMarkersDirOf(home.Dir(ctx)); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "last-run"), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
	}
	return results
}

// Runs the check seam with a panic turned into the failure Python catches as Exception
func callCheck(ctx context.Context, opts ScheduledCheckOptions) (results []*CheckResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return opts.RunChecks(ctx, opts.PluginsDir)
}

// Runs one apply with a panic or exit turned into an error the tick logs
func applyGuarded(ctx context.Context, apply func(context.Context, string) error, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return apply(ctx, name)
}

// Writes the plugin-check receipt, logging at debug level when the write panics or fails
func writeCheckReceipt(ctx context.Context, log *slog.Logger, results []*CheckResult, warning string, checkOK, anyUpdate bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Debug("plugin-check receipt write failed", "error", fmt.Sprint(r))
		}
	}()
	token := pm.BeginReceipt(ctx, "plugin-check")
	rows := make([]any, len(results))
	for i, r := range results {
		rows[i] = r
	}
	pm.RecordPluginChecks(ctx, rows)
	if warning != "" {
		pm.RecordWarning(ctx, warning)
	}
	outcome, code := "ok", 0
	if !checkOK {
		outcome, code = "failed", 1
	} else if anyUpdate {
		outcome = "updates-available"
	}
	pm.FinalizeReceipt(ctx, outcome, code, token)
}

// Comma joined names of check rows
func checkNames(rows []*CheckResult) string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

// Gateway boot and housekeeping tick filling the real check, plugins dir and non interactive update seams
func MaybeRunGatewayCheck(ctx context.Context, opts ScheduledCheckOptions) ([]*CheckResult, bool, error) {
	if opts.PluginsDir == "" {
		dir, err := pluginsDir(home.Dir(ctx))
		if err != nil {
			return nil, false, err
		}
		opts.PluginsDir = dir
	}
	if opts.RunChecks == nil {
		opts.RunChecks = gatewayRunChecks
	}
	if opts.ApplyUpdate == nil {
		opts.ApplyUpdate = gatewayApplyUpdate
	}
	results, ran := RunScheduledCheck(ctx, opts)
	return results, ran, nil
}

// Real read only update check the manual check-updates command runs, swapped by tests
var gatewayRunChecks = func(ctx context.Context, pluginsDir string) ([]*CheckResult, error) {
	return RunChecks(ctx, pluginsDir, RunChecksOptions{}), nil
}

// Non interactive single plugin update the auto apply path runs, swapped by tests
var gatewayApplyUpdate = func(ctx context.Context, name string) error {
	return CmdUpdate(ctx, name, false, false, os.Stdout)
}
