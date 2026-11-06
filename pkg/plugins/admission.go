package plugins

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
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

// Member dirs of the candidate selection, every recorded home selection kept without an active plugins dir
func CandidateMemberDirs(ctx context.Context, enabled, disabled []string, activePluginsDir string, extraDirs []string) ([]string, error) {
	proposedHome := ""
	if activePluginsDir != "" {
		proposedHome = pypath.PathParent(activePluginsDir)
	}
	return pmCandidateMembers(ctx, extraDirs, proposedHome, enabled, disabled)
}

// Candidate member trees through the package manager selection, the proposed home overriding its recorded config
func pmCandidateMembers(ctx context.Context, extraDirs []string, proposedHome string, enabled, disabled []string) ([]string, error) {
	return pm.CandidateMembers(ctx, extraDirs, proposedHome, enabled, disabled)
}

// AdmitOptions carries the keyword arguments of admit_plugin_set_change
type AdmitOptions = admitOptions

// Public admission of a plugin set change, refused without touching config when the union cannot resolve
func AdmitPluginSetChange(ctx context.Context, enabled, disabled map[string]bool, opts AdmitOptions) error {
	return admitPluginSetChange(ctx, enabled, disabled, opts)
}
