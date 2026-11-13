package selfupdate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var installRepairLog = slog.Default().With("logger", "icebath_cli.main_install_repair")

// Windows gate of the launcher heal and quarantine sweep, the module attribute hermes tests patch
var installRepairIsWindows = func() bool { return runtime.GOOS == "windows" }

// Rename the quarantine restore retries, the os attribute hermes tests patch
var quarantineRename = os.Rename

// Backoff in milliseconds between restore attempts of a quarantined shim
var QuarantineRestoreBackoffMS = []int{0, 100, 250, 500, 1000}

// Fresh quarantine files can belong to an update still running
const quarantineGraceSeconds = 15 * 60

// Renames quarantined shims back, retrying a lock, the pairs still quarantined after the retries returned
func RestoreQuarantinedShims(moved [][2]string, stream io.Writer, backoffMS []int) [][2]string {
	if stream == nil {
		stream = os.Stderr
	}
	if backoffMS == nil {
		backoffMS = QuarantineRestoreBackoffMS
	}
	failed := [][2]string{}
	for _, pair := range moved {
		original, quarantined := pair[0], pair[1]
		var lastErr error
		for _, delay := range backoffMS {
			if pathPresent(original) || !pathPresent(quarantined) {
				lastErr = nil
				break
			}
			if delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			if err := quarantineRename(quarantined, original); err != nil {
				lastErr = err
				continue
			}
			lastErr = nil
			break
		}
		if lastErr == nil {
			continue
		}
		failed = append(failed, pair)
		name := filepath.Base(original)
		stem := name
		if strings.HasSuffix(strings.ToLower(name), ".exe") {
			stem = name[:len(name)-4]
		}
		fmt.Fprintf(stream, "  ✖ FAILED to restore %s (%s) — it is still quarantined as %s.\n"+
			"    `%s` will NOT be on PATH until it is put back. Run this, then re-run the update:\n"+
			"      move \"%s\" \"%s\"\n", name, pypath.OSErrorClass(lastErr), filepath.Base(quarantined), stem, quarantined, original)
	}
	return failed
}

// The .old.<unix-ms> stamp of a quarantine file name, false for a name this code never made
func QuarantineStampMS(name string) (int64, bool) {
	base := filepath.Base(name)
	idx := strings.LastIndex(base, ".old.")
	if idx < 0 {
		return 0, false
	}
	value, ok := pyint.Parse(base[idx+len(".old."):])
	if !ok || !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

// The venv Scripts directory of the checkout when it exists, empty otherwise
func venvScriptsDir(root string) string {
	venv, err := pm.ProjectVenvDir(root)
	if err != nil || venv == "" {
		return ""
	}
	scripts := pm.VenvBinDir(venv, installRepairIsWindows())
	if info, err := os.Stat(scripts); err != nil || !info.IsDir() {
		return ""
	}
	return scripts
}

// Entry point shims an update may rewrite, Windows exe launchers only
func icebathExeShims(scriptsDir string) []string {
	if !installRepairIsWindows() {
		return nil
	}
	names := map[string]bool{"icebath": true, "icebath-agent": true, "icebath-acp": true, "icebath-gateway": true}
	for _, n := range pm.WindowsBinLaunchers() {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, filepath.Join(scriptsDir, n+".exe"))
	}
	return out
}

// Lowercased backslash form of a Windows path with redundant separators collapsed, ntpath normcase of normpath
func normalizeWindowsPath(value string) string {
	path := strings.ReplaceAll(value, "/", `\`)
	prefix := ""
	if strings.HasPrefix(path, `\\`) {
		prefix = `\\`
		path = strings.TrimLeft(path, `\`)
	} else if len(path) >= 2 && path[1] == ':' {
		prefix = path[:2]
		path = path[2:]
		if strings.HasPrefix(path, `\`) {
			prefix += `\`
			path = strings.TrimLeft(path, `\`)
		}
	}
	parts := []string{}
	for _, part := range strings.Split(path, `\`) {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
			} else if !strings.HasSuffix(prefix, `\`) {
				parts = append(parts, part)
			}
		default:
			parts = append(parts, part)
		}
	}
	joined := prefix + strings.Join(parts, `\`)
	if joined == "" {
		joined = "."
	}
	return strings.ToLower(joined)
}

// Drops this install's shim to quarantine pairs from a PendingFileRenameOperations value, the kept entries and the count dropped
func filterPendingShimRenames(entries []string, shims []string) ([]string, int) {
	norm := func(value string) string {
		path := strings.TrimLeft(value, "!")
		path = strings.TrimPrefix(path, `\??\`)
		return normalizeWindowsPath(path)
	}
	shimPaths := map[string]bool{}
	for _, shim := range shims {
		shimPaths[norm(shim)] = true
	}
	kept := []string{}
	removed := 0
	for i := 0; i+1 < len(entries); i += 2 {
		source, target := entries[i], entries[i+1]
		sourceNorm := norm(source)
		if shimPaths[sourceNorm] && strings.HasPrefix(norm(target), sourceNorm+".old.") {
			removed++
			continue
		}
		kept = append(kept, source, target)
	}
	if len(entries)%2 == 1 {
		kept = append(kept, entries[len(entries)-1])
	}
	return kept, removed
}

// Pending shim rename sweep, the module attribute hermes tests patch
var cleanupPendingShimRenamesFn = cleanupPendingShimRenames

// Sweeps and rescues quarantined exe shims of earlier updates, a live update's quarantine left alone
func CleanupQuarantinedExes(ctx context.Context, root, scriptsDir string) {
	if !installRepairIsWindows() {
		return
	}
	if scriptsDir == "" {
		scriptsDir = venvScriptsDir(root)
		if scriptsDir == "" {
			return
		}
	}
	if busy, _ := updaterecord.UpdateInProgress(ctx, root); busy {
		return
	}
	cleanupPendingShimRenamesFn(scriptsDir)
	now := float64(time.Now().UnixNano()) / 1e9
	matches, err := filepath.Glob(filepath.Join(scriptsDir, "*.exe.old.*"))
	if err != nil {
		return
	}
	type candidate struct {
		stamp int64
		stale string
	}
	candidates := []candidate{}
	for _, stale := range matches {
		if stamp, ok := QuarantineStampMS(stale); ok {
			candidates = append(candidates, candidate{stamp, stale})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].stamp > candidates[j].stamp })
	for _, c := range candidates {
		base := filepath.Base(c.stale)
		original := filepath.Join(filepath.Dir(c.stale), base[:strings.LastIndex(base, ".old.")])
		if _, err := os.Stat(original); err != nil {
			RestoreQuarantinedShims([][2]string{{original, c.stale}}, os.Stderr, nil)
			continue
		}
		if now-float64(c.stamp)/1000.0 < quarantineGraceSeconds {
			continue
		}
		os.Remove(c.stale)
	}
}

// Re stages the Windows launchers when they vanish or still boot through the venv, the restored paths returned
func EnsureWindowsBinLaunchers(root string, windows *bool, userPathEntries []string) []string {
	isWindows := installRepairIsWindows()
	if windows != nil {
		isWindows = *windows
	}
	if !isWindows {
		return []string{}
	}
	homeRoot := home.Root()
	launcherPresent := func(target, name string) bool {
		return pathPresent(filepath.Join(target, name+".exe")) || pathPresent(filepath.Join(target, name+".cmd"))
	}
	venvDir, _ := pm.ProjectVenvDir(root)
	localBin := filepath.Join(root, ".icebath", "bin")
	needsAttention := func(target, name string) bool {
		exe := filepath.Join(target, name+".exe")
		if !pathPresent(exe) {
			cmd := filepath.Join(target, name+".cmd")
			info, err := os.Stat(cmd)
			return !(err == nil && info.Mode().IsRegular() && launcherPresent(localBin, name))
		}
		return pm.ExeIsVenvBound(exe, venvDir)
	}
	names := pm.WindowsBinLaunchers()
	anyName := func(pred func(string) bool) bool {
		for _, n := range names {
			if pred(n) {
				return true
			}
		}
		return false
	}
	var targets []string
	restored := []string{}
	if normalizeWindowsPath(filepath.Dir(root)) == normalizeWindowsPath(homeRoot) {
		canonical := filepath.Join(homeRoot, "bin")
		if anyName(func(n string) bool { return !launcherPresent(localBin, n) }) {
			if err := os.MkdirAll(canonical, 0o777); err != nil {
				return []string{}
			}
			written, err := pm.EnsureInstallLaunchers(context.Background(), root, canonical)
			if err != nil {
				return []string{}
			}
			restored = append(restored, written...)
		}
		if len(restored) == 0 && anyName(func(n string) bool { return needsAttention(canonical, n) }) {
			targets = append(targets, canonical)
		}
	}
	legacy := filepath.Join(root, "bin")
	if anyName(func(n string) bool { return needsAttention(legacy, n) }) {
		if userPathEntries == nil {
			userPathEntries = windowsUserPathEntries()
		}
		configured := map[string]bool{}
		for _, entry := range userPathEntries {
			configured[normalizeWindowsPath(entry)] = true
		}
		if configured[normalizeWindowsPath(legacy)] {
			targets = append(targets, legacy)
		}
	}
	if len(targets) == 0 {
		return restored
	}
	for _, target := range targets {
		if err := os.MkdirAll(target, 0o777); err != nil {
			continue
		}
		for _, name := range names {
			if !needsAttention(target, name) {
				continue
			}
			final, err := pm.StageLauncher(context.Background(), name, root, target)
			if err != nil {
				return []string{}
			}
			if final == "" {
				continue
			}
			obsolete := filepath.Join(target, name+".exe")
			if strings.EqualFold(filepath.Ext(final), ".cmd") && pm.ExeIsVenvBound(obsolete, venvDir) {
				os.Remove(obsolete)
			}
			restored = append(restored, final)
		}
	}
	if len(restored) > 0 {
		fmt.Fprintln(os.Stderr, "  ✓ Restored icebath launcher(s): "+strings.Join(restored, ", "))
	}
	return restored
}

// Recovers gateways a killed update left paused, the update command and a booting gateway excepted
func RecoverPausedGateways(ctx context.Context, argv []string) {
	if err := updaterecord.RecoverPauses(ctx, argv); err != nil {
		installRepairLog.Debug(fmt.Sprintf("paused gateway recovery failed: %s", err.Error()))
	}
}

// Gives pm the configured feature scan, which needs the gateway platform registry pm cannot import
func init() { pm.ConfiguredFeaturesMissingDeps = configuredFeaturesMissingDeps }

// Connected platforms whose adapter check fails
func configuredFeaturesMissingDeps(ctx context.Context) []pm.ConfiguredFeatureGap {
	var missing []pm.ConfiguredFeatureGap
	if err := func() error {
		cfg, err := gateway.LoadGatewayConfig(ctx)
		if err != nil {
			return err
		}
		for _, platform := range cfg.GetConnectedPlatforms(ctx) {
			entry := gateway.Registry.Get(ctx, string(platform))
			if entry == nil {
				continue
			}
			if entry.CheckFn == nil {
				return fmt.Errorf("'NoneType' object is not callable")
			}
			if !entry.RunCheck(ctx) {
				hint := entry.InstallHint
				if hint == "" {
					hint = "Run `icebath setup` to install support."
				}
				missing = append(missing, pm.ConfiguredFeatureGap{Feature: entry.Label, Hint: hint, Extra: entry.Name})
			}
		}
		return nil
	}(); err != nil {
		installRepairLog.Debug(fmt.Sprintf("configured-platform dependency check skipped: %s", err.Error()))
	}
	return missing
}
