package plugins

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// The candidate plugin set was refused with config and environment untouched
type AdmissionRefused struct{ Msg string }

func (e *AdmissionRefused) Error() string { return e.Msg }

// The resolver proved the candidate set cannot co-install with the pinned dependency set
type DependencyConflict struct{ Cause, Plugin string }

// Conflict message leading with the plugin the user asked for and keeping the resolver cause
func (e *DependencyConflict) Error() string {
	who := "The plugin selection"
	if e.Plugin != "" {
		who = "Plugin '" + e.Plugin + "'"
	}
	return who + " conflicts with the dependencies pinned by Icebath core or an enabled plugin, " +
		"so it was not admitted. Resolver: " + e.Cause
}

// The AdmissionRefused base so a refusal check also catches a conflict
func (e *DependencyConflict) Unwrap() error { return &AdmissionRefused{Msg: e.Error()} }

// Proposed selection details handed to the PM publisher beside the candidate sets
type admitOptions struct {
	ActivePluginsDir string
	ExtraDirs        []string
	ExpectedConfig   string
	Plugin           string
	Settled          []string
}

// Sorted copy of a name list
func sortedCopy(items []string) []any {
	s := append([]string{}, items...)
	sort.Strings(s)
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// PM discovers and validates the proposed union under its install lock without this process writing config
func admitPluginSetChange(ctx context.Context, enabled, disabled map[string]bool, opts admitOptions) error {
	h := home.Dir(ctx)
	if opts.ActivePluginsDir != "" {
		h = filepath.Dir(opts.ActivePluginsDir)
	}
	resolvedHome, err := pathutil.Resolve(h)
	if err != nil {
		return &AdmissionRefused{Msg: err.Error()}
	}
	if !pm.ManagerLinked() {
		return admitWithoutPackageManager(ctx, resolvedHome, enabled, disabled, opts)
	}
	extra := []any{}
	for _, d := range opts.ExtraDirs {
		r, err := pathutil.Resolve(d)
		if err != nil {
			return &AdmissionRefused{Msg: err.Error()}
		}
		extra = append(extra, r)
	}
	data := map[string]any{
		"home": resolvedHome, "enabled": sortedCopy(sortedKeys(enabled)), "disabled": sortedCopy(sortedKeys(disabled)),
		"extra_dirs": extra, "settled": sortedCopy(opts.Settled),
	}
	if opts.ExpectedConfig != "" {
		data["expected_config"] = opts.ExpectedConfig
	}
	err = pm.SyncVenv(ctx, pm.SyncVenvOptions{Explicit: true, Plugins: &pm.Selection{Data: data}})
	if err == nil {
		return nil
	}
	var conflict *pm.ResolutionConflict
	if errors.As(err, &conflict) && conflict.InstallError != nil {
		return &DependencyConflict{Cause: conflict.InstallError.Cause, Plugin: opts.Plugin}
	}
	return &AdmissionRefused{Msg: err.Error()}
}

// Writes a plugin set change into the home config of a build without the package manager, the disabled list first so no step between the writes runs a plugin the change turns off, refusing a new plugin that needs Python packages
func admitWithoutPackageManager(ctx context.Context, h string, enabled, disabled map[string]bool, opts admitOptions) error {
	before := getEnabledSet(h)
	for _, name := range sortedKeys(enabled) {
		if before[name] {
			continue
		}
		entry, err := FindPluginEntry(h, name)
		if err != nil {
			return &AdmissionRefused{Msg: err.Error()}
		}
		if entry == nil {
			continue
		}
		if reqs := declaredRequirements(&Manifest{Path: entry.Dir}); len(reqs) > 0 {
			return &AdmissionRefused{Msg: linktier.Missing(fmt.Sprintf("plugin '%s' needs the Python packages %s, and the tool package manager that installs them",
				name, strings.Join(reqs, ", ")), linktier.Extras)}
		}
	}
	if opts.ExpectedConfig != "" {
		current, err := pluginSelectionVersion(h)
		if err != nil {
			return &AdmissionRefused{Msg: err.Error()}
		}
		if current != opts.ExpectedConfig {
			return &AdmissionRefused{Msg: "Plugin configuration changed since this selection was read; retry."}
		}
	}
	hctx := homeContext(h)
	record := settledUpdateRecord(hctx, disabled, opts.Settled)
	writes := []struct {
		key   string
		value any
	}{{settings.PluginsDisabled.Key, sortedCopy(sortedKeys(disabled))}, {settings.PluginsEnabled.Key, sortedCopy(sortedKeys(enabled))}, {settings.PluginsDisabledByUpdate.Key, nil}}
	if len(record) > 0 {
		writes[2].value = record
	}
	for _, w := range writes {
		if _, err := settings.Set(hctx, w.key, w.value, settings.Global); err != nil {
			return &AdmissionRefused{Msg: err.Error()}
		}
	}
	return nil
}

// Record of plugins an update disabled without the ones just settled or no longer disabled
func settledUpdateRecord(ctx context.Context, disabled map[string]bool, settled []string) map[string]any {
	kept := map[string]any{}
	for name, why := range settings.PluginsDisabledByUpdate.Get(ctx) {
		if disabled[name] && !slices.Contains(settled, name) {
			kept[name] = why
		}
	}
	return kept
}

// AdmitOptions carries the keyword arguments of admit_plugin_set_change
type AdmitOptions = admitOptions

// Public admission of a plugin set change, refused without touching config when the union cannot resolve
func AdmitPluginSetChange(ctx context.Context, enabled, disabled map[string]bool, opts AdmitOptions) error {
	return admitPluginSetChange(ctx, enabled, disabled, opts)
}
