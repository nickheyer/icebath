package plugins

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/pm"
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
	resolvedHome, err := pypath.Realpath(h, pypath.Lax)
	if err != nil {
		return &AdmissionRefused{Msg: err.Error()}
	}
	if !pm.ManagerLinked() {
		return admitWithoutPackageManager(ctx, resolvedHome, enabled, disabled, opts)
	}
	extra := []any{}
	for _, d := range opts.ExtraDirs {
		r, err := pypath.Realpath(d, pypath.Lax)
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

// Writes a plugin set change into the home config of a build without the package manager, refusing a new plugin that needs Python packages
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
	changes := config.Changes{Set: []config.KeyValue{
		{Key: "plugins.enabled", Value: sortedCopy(sortedKeys(enabled))},
		{Key: "plugins.disabled", Value: sortedCopy(sortedKeys(disabled))},
	}}
	record, present, err := settledUpdateRecord(h, disabled, opts.Settled)
	if err != nil {
		return &AdmissionRefused{Msg: err.Error()}
	}
	switch {
	case len(record) > 0:
		changes.Set = append(changes.Set, config.KeyValue{Key: "plugins." + pm.DisabledByUpdateRecord, Value: record})
	case present:
		changes.Unset = append(changes.Unset, "plugins."+pm.DisabledByUpdateRecord)
	}
	if err := (config.FileBackend{}).WritePath(ctx, filepath.Join(h, "config.yaml"), changes); err != nil {
		return &AdmissionRefused{Msg: err.Error()}
	}
	return nil
}

// Record of plugins an update disabled without the ones just settled or no longer disabled, and whether the config holds one
func settledUpdateRecord(h string, disabled map[string]bool, settled []string) (map[string]any, bool, error) {
	user, err := config.ReadUser(h)
	if err != nil {
		return nil, false, err
	}
	plugins, _ := user["plugins"].(map[string]any)
	raw, present := plugins[pm.DisabledByUpdateRecord]
	record, _ := raw.(map[string]any)
	kept := map[string]any{}
	for name, why := range record {
		if disabled[name] && !slices.Contains(settled, name) {
			kept[name] = why
		}
	}
	return kept, present, nil
}

// AdmitOptions carries the keyword arguments of admit_plugin_set_change
type AdmitOptions = admitOptions

// Public admission of a plugin set change, refused without touching config when the union cannot resolve
func AdmitPluginSetChange(ctx context.Context, enabled, disabled map[string]bool, opts AdmitOptions) error {
	return admitPluginSetChange(ctx, enabled, disabled, opts)
}
