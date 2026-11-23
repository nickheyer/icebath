// Package defaults installs the catalog plugins marked default into every home once
package defaults

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Argv marker of the detached child that installs the default plugins of every home
const Entry = "__icebath_default_plugins__"

const (
	// File in the plugins directory of a home listing the default plugins it was given once, so a removed or disabled default stays the user's choice
	givenFile = ".default-plugins.json"
	// Failure stamp name of a whole pass, kept in the dependency home root
	backoffName = "pass"
	// Reason a pass ran nothing where lazy installs are off
	skippedLazy = "lazy-installs-disabled"
)

// Whether a home may fetch plugins: allowed, denied by security.allow_lazy_installs or the env opt out, or an unreadable config that keeps the home owed
const (
	FetchAllowed    = "allowed"
	FetchDenied     = "denied"
	FetchUnreadable = "unreadable"
)

// Time the update tail waits for another installer of the default set
const UpdateWait = 900 * time.Second

// Identity of the installed code a settled pass is recorded under, installed by the update package
var Identity func(ctx context.Context) string

// Whether this binary dispatches the detached child entry, false in a binary that would re-execute into something else
var entryDispatched atomic.Bool

// What one pass over the default plugins did: the plugins it installed, what is still to fetch and why it ran nothing
type Outcome struct {
	Installed []string
	Owed      []string
	Skipped   string
}

// Catalog entries marked default that this OS and this icebath can run, from the live catalog else the in-tree one
func Entries(ctx context.Context) []*plugins.PluginCatalogEntry {
	var out []*plugins.PluginCatalogEntry
	for _, entry := range plugins.LoadCatalogLive(ctx) {
		if entry.Default && plugins.DefaultCatalogTiers[entry.Tier] && plugins.CatalogEntryRunsHere(ctx, entry) {
			out = append(out, entry)
		}
	}
	return out
}

// Default plugins a home records as given once, none when it has no record yet
func recorded(h string) (map[string]bool, error) {
	done := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(home.PluginsDir(h), givenFile))
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(home.PluginsDir(h), givenFile), err)
	}
	for _, name := range names {
		done[name] = true
	}
	return done, nil
}

// Adds names to the record of the default plugins a home was given
func record(h string, names map[string]bool) error {
	done, err := recorded(h)
	if err != nil {
		return err
	}
	for name := range names {
		done[name] = true
	}
	sorted := make([]string, 0, len(done))
	for name := range done {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	data, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return err
	}
	return config.AtomicWrite(filepath.Join(home.PluginsDir(h), givenFile), append(data, '\n'), 0o600)
}

// Directory a home installed a catalog entry into, the one its install record ties to the catalog name else the directory of that name, empty when it has none
func installedDir(h string, entry *plugins.PluginCatalogEntry) string {
	pluginsDir := home.PluginsDir(h)
	rows, _ := plugins.ReadInstallMetadata(h)
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		row, ok := rows[name].(map[string]any)
		if !ok {
			continue
		}
		named := row["catalog_name"]
		if block, ok := row["catalog"].(map[string]any); ok {
			named = block["name"]
		}
		if named == entry.Name && isDir(filepath.Join(pluginsDir, name)) {
			return filepath.Join(pluginsDir, name)
		}
	}
	if dir := filepath.Join(pluginsDir, entry.Name); isDir(dir) {
		return dir
	}
	return ""
}

// Name the plugin.yaml of a tree declares, empty for a missing or malformed manifest
func manifestName(target string) string {
	raw, err := os.ReadFile(filepath.Join(target, "plugin.yaml"))
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return ""
	}
	doc, err := config.SafeLoad(text)
	if err != nil {
		return ""
	}
	manifest, ok := doc.(map[string]any)
	if !ok || !pyrepr.Truthy(manifest["name"]) {
		return ""
	}
	return pyrepr.Str(manifest["name"])
}

// Whether one of the names is in plugins.enabled and in plugins.disabled of a home, read with the managed overlay applied the way the loader reads it
func listed(h string, names map[string]bool) (enabled, disabled bool) {
	cfg, err := yamlLayers(h)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("default plugins: unreadable config for %s: %s", h, err))
		return false, false
	}
	lists := mapping(cfg["plugins"])
	in := func(key string) bool {
		entries, ok := lists[key].([]any)
		if !ok {
			return false
		}
		for _, name := range entries {
			if names[pyrepr.Str(name)] {
				return true
			}
		}
		return false
	}
	return in("enabled"), in("disabled")
}

// Whether a home already has an entry: a tree the user put there, or one an earlier automatic attempt left that someone has since enabled or disabled; a tree a refused enable left behind is retried
func settled(h string, entry *plugins.PluginCatalogEntry) bool {
	target := installedDir(h, entry)
	if target == "" {
		return pathExists(placedMarker(h, entry))
	}
	if !pathExists(failureStamp(h, entry.Name)) {
		return true
	}
	names := map[string]bool{entry.Name: true, filepath.Base(target): true}
	if name := manifestName(target); name != "" {
		names[name] = true
	}
	enabled, disabled := listed(h, names)
	return enabled || disabled
}

// Marker written when a failed automatic attempt left the tree of an entry on disk
func placedMarker(h string, entry *plugins.PluginCatalogEntry) string {
	return filepath.Join(home.CacheDir(h), "default-plugins.placed-"+entry.Name)
}

// Fetch policy of a home read without binding its profile, nothing fetched unless allowed and an unreadable config keeping the home owed
func FetchPolicy(h string) string {
	switch pystr.Lower(pystr.Strip(os.Getenv(pm.EnvDisableLazyInstalls))) {
	case "1", "true", "yes":
		return FetchDenied
	}
	cfg, err := yamlLayers(h)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("default plugins: unreadable config for %s: %s", h, err))
		return FetchUnreadable
	}
	value, found := mapping(cfg["security"])["allow_lazy_installs"]
	if !found {
		return FetchAllowed
	}
	if allowed, isBool := value.(bool); isBool && allowed {
		return FetchAllowed
	}
	return FetchDenied
}

// Installs one catalog plugin into a home at its catalog pin and enables it, a leftover tree of an earlier attempt discarded first; never forced, so it does not overwrite another plugin at the destination
func installOne(ctx context.Context, h, name, discard string) (ok bool, reason string) {
	defer func() {
		if r := recover(); r != nil {
			ok, reason = false, fmt.Sprint(r)
		}
	}()
	ctx = home.WithOverride(ctx, h)
	if discard != "" {
		if err := plugins.RemoveInstalledPlugin(ctx, h, discard); err != nil {
			return false, err.Error()
		}
	}
	consent := true
	result, err := plugins.UnattendedInstallPlugin(ctx, plugins.UnattendedInstallOptions{Enable: true, CatalogName: name, AssumeDepsConsent: &consent})
	if err != nil {
		return false, err.Error()
	}
	if pyrepr.Truthy(result["ok"]) {
		return true, ""
	}
	if pyrepr.Truthy(result["error"]) {
		return false, pyrepr.Str(result["error"])
	}
	return false, ""
}

// Mapping value of a config section, nil for any other value
func mapping(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case config.Tree:
		return m
	}
	return nil
}

// Whether the path is a directory
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Whether the path exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Creates a file or refreshes its modification time
func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// Removes a file, a missing one counting as removed
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Install installs and enables every default plugin a home never had, entries nil reading the catalog; one plugin's failure is reported through say and listed as owed, never returned
func Install(ctx context.Context, h string, say func(string), entries []*plugins.PluginCatalogEntry) (Outcome, error) {
	var result Outcome
	switch FetchPolicy(h) {
	case FetchUnreadable:
		result.Owed = []string{h}
		return result, nil
	case FetchDenied:
		result.Skipped = skippedLazy
		return result, nil
	}
	if entries == nil {
		entries = Entries(ctx)
	}
	done, err := recorded(h)
	if err != nil {
		return result, err
	}
	var pending []*plugins.PluginCatalogEntry
	for _, entry := range entries {
		if !done[entry.Name] {
			pending = append(pending, entry)
		}
	}
	have := map[string]bool{}
	for _, entry := range pending {
		if settled(h, entry) {
			have[entry.Name] = true
		}
	}
	for _, entry := range pending {
		if have[entry.Name] {
			continue
		}
		placed := placedMarker(h, entry)
		leftover := installedDir(h, entry)
		if leftover != "" {
			if err := removeFile(placed); err != nil {
				return result, err
			}
		}
		ok, reason := installOne(ctx, h, entry.Name, leftover)
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if ok {
			if err := removeFile(failureStamp(h, entry.Name)); err != nil {
				return result, err
			}
			if err := removeFile(placed); err != nil {
				return result, err
			}
			have[entry.Name] = true
			result.Installed = append(result.Installed, entry.Name)
			say(fmt.Sprintf("  ✓ Installed default plugin '%s' for %s", entry.Name, homeLabel(h)))
			continue
		}
		result.Owed = append(result.Owed, entry.Name)
		noteFailure(h, entry.Name)
		if installedDir(h, entry) != "" {
			if err := touch(placed); err != nil {
				return result, err
			}
		}
		say(fmt.Sprintf("  ⚠ Default plugin '%s' could not be installed for %s: %s. Icebath retries at a later start; "+
			"`icebath plugins install %s` installs it now.", entry.Name, homeLabel(h), firstCause(reason), entry.Name))
	}
	if len(have) > 0 {
		if err := record(h, have); err != nil {
			return result, err
		}
	}
	return result, nil
}

// State file of the default plugin installer in the dependency home root
func stateFile(root, name string) string {
	return filepath.Join(home.CacheDir(root), "default-plugins."+name)
}

// One pass over every profile home sharing this install, the catalog read once and one home's failure never stopping the next; waits up to wait for another installer and fails with a lock timeout past that
func installAllHomes(ctx context.Context, say func(string), wait time.Duration) (Outcome, error) {
	var summary Outcome
	homes, err := pm.DependencyHomes(ctx)
	if err != nil {
		return summary, err
	}
	var allowed []string
	for _, h := range homes {
		switch FetchPolicy(h) {
		case FetchUnreadable:
			summary.Owed = append(summary.Owed, h)
		case FetchAllowed:
			allowed = append(allowed, h)
		}
	}
	if len(allowed) == 0 {
		return summary, nil
	}
	entries := Entries(ctx)
	if !pyrepr.Truthy(plugins.FetchLiveCatalog(ctx, false)["entries"]) {
		summary.Owed = append(summary.Owed, "live catalog")
	}
	if len(entries) == 0 {
		return summary, nil
	}
	release, err := provider.FileLock(stateFile(pm.DependencyHomeRoot(ctx), "lock"), wait, "another default plugin install is running")
	if err != nil {
		return summary, err
	}
	defer release()
	for _, h := range allowed {
		outcome, err := Install(ctx, h, say, entries)
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("default plugins skipped for %s: %s", h, err))
			outcome = Outcome{Owed: []string{h}}
		}
		summary.Installed = append(summary.Installed, outcome.Installed...)
		summary.Owed = append(summary.Owed, outcome.Owed...)
	}
	return summary, nil
}

// InstallAndRecord is the one coordinator of the update tail and the detached child: it passes over every home again until no rescan was asked for meanwhile, then records what the boot step reads, a failure beginning the backoff and a clean pass marking this revision settled
func InstallAndRecord(ctx context.Context, say func(string), wait time.Duration) (Outcome, error) {
	root := pm.DependencyHomeRoot(ctx)
	rerun, settledMark := stateFile(root, "rerun"), stateFile(root, "settled")
	var outcome Outcome
	for {
		started := time.Now()
		var err error
		if outcome, err = installAllHomes(ctx, say, wait); err != nil {
			return outcome, err
		}
		if info, err := os.Stat(rerun); err != nil || info.ModTime().Before(started) {
			break
		}
	}
	if len(outcome.Owed) > 0 {
		noteFailure(root, backoffName)
		return outcome, removeFile(settledMark)
	}
	if err := removeFile(failureStamp(root, backoffName)); err != nil {
		return outcome, err
	}
	if err := os.MkdirAll(filepath.Dir(settledMark), 0o777); err != nil {
		return outcome, err
	}
	return outcome, os.WriteFile(settledMark, []byte(identity(ctx)), 0o644)
}

// Identity of the installed code, empty when no update package installed its reader
func identity(ctx context.Context) string {
	if Identity == nil {
		return ""
	}
	return Identity(ctx)
}

// SpawnInstall starts the detached child that installs the defaults of every home unless the last attempt failed recently, h when given having to allow fetching; a running child sees the request when it finishes and scans again, and the call never waits
func SpawnInstall(ctx context.Context, h string) (bool, error) {
	if !entryDispatched.Load() {
		return false, nil
	}
	root := pm.DependencyHomeRoot(ctx)
	if h != "" && FetchPolicy(h) == FetchDenied {
		return false, nil
	}
	if err := removeFile(stateFile(root, "settled")); err != nil {
		return false, err
	}
	if failedRecently(root, backoffName) {
		return false, nil
	}
	if err := touch(stateFile(root, "rerun")); err != nil {
		return false, err
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	start := func(breakaway bool) error {
		cmd := exec.Command(exe, Entry)
		cmd.SysProcAttr = home.DetachAttr(breakaway)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	err = start(true)
	if err != nil && runtime.GOOS == "windows" {
		err = start(false)
	}
	return err == nil, err
}

// DispatchEntry runs the default plugin install and exits when argv carries the entry, otherwise records that this binary handles it
func DispatchEntry(ctx context.Context) {
	entryDispatched.Store(true)
	if len(os.Args) > 1 && os.Args[1] == Entry {
		os.Exit(RunChild(ctx))
	}
}

// RunChild is the detached child, logging to logs/default-plugins.log of the dependency home root; one that finds another installer running leaves its request to that one
func RunChild(ctx context.Context) (code int) {
	root := pm.DependencyHomeRoot(ctx)
	var logFile *os.File
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o777); err == nil {
		logFile, _ = os.OpenFile(filepath.Join(root, "logs", "default-plugins.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	say := func(line string) {
		if logFile != nil {
			fmt.Fprintf(logFile, "%s default-plugins %s\n", time.Now().Format("2006-01-02 15:04:05,000"), line)
		}
	}
	failed := func(reason any) int {
		say(fmt.Sprintf("default plugin install failed: %v", reason))
		noteFailure(root, backoffName)
		return 1
	}
	defer func() {
		if r := recover(); r != nil {
			code = failed(r)
		}
	}()
	outcome, err := InstallAndRecord(ctx, say, time.Second)
	var busy *provider.FileLockTimeoutError
	switch {
	case errors.As(err, &busy):
		return 0
	case err != nil:
		return failed(err)
	case len(outcome.Owed) > 0:
		return 1
	}
	return 0
}

// StepInstall is the boot step, never blocking the start: owed, so later starts check again without network, until a child reports every default settled for this revision; an owed start outside the backoff starts one
func StepInstall(ctx context.Context) (map[string]any, error) {
	owed := func(err error) (map[string]any, error) {
		return map[string]any{"ok": false, "error": err.Error(), "owed": true}, nil
	}
	homes, err := pm.DependencyHomes(ctx)
	if err != nil {
		return owed(err)
	}
	denied := true
	for _, h := range homes {
		if FetchPolicy(h) != FetchDenied {
			denied = false
			break
		}
	}
	if denied {
		return map[string]any{"ok": true, "skipped": skippedLazy}, nil
	}
	mark := stateFile(pm.DependencyHomeRoot(ctx), "settled")
	if raw, err := os.ReadFile(mark); err == nil && pystr.Strip(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")) == identity(ctx) {
		return map[string]any{"ok": true}, nil
	}
	spawned, err := SpawnInstall(ctx, "")
	if err != nil {
		return owed(err)
	}
	return map[string]any{"ok": true, "spawned": spawned, "owed": true}, nil
}
