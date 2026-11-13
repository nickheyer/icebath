package plugins

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/home"
)

// Config key recording the plugins an update turned off and why
const disabledByUpdateKey = "disabled_by_update"

// The candidate plugin set was refused with config untouched
type AdmissionRefused struct{ Msg string }

func (e *AdmissionRefused) Error() string { return e.Msg }

// Proposed selection details beside the candidate sets
type admitOptions struct {
	ActivePluginsDir string
	ExpectedConfig   string
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

// Writes the proposed plugins.enabled and plugins.disabled lists, refusing when config.yaml changed since it was read
func admitPluginSetChange(ctx context.Context, enabled, disabled map[string]bool, opts admitOptions) error {
	h := home.Dir(ctx)
	if opts.ActivePluginsDir != "" {
		h = filepath.Dir(opts.ActivePluginsDir)
	}
	return updateUserConfig(ctx, h, func(doc map[string]any) error {
		if opts.ExpectedConfig != "" {
			current, err := pluginSelectionVersion(h)
			if err != nil {
				return &AdmissionRefused{Msg: err.Error()}
			}
			if current != opts.ExpectedConfig {
				return &AdmissionRefused{Msg: "Plugin configuration changed since this selection was read; retry."}
			}
		}
		section, ok := doc["plugins"].(map[string]any)
		if !ok {
			if doc["plugins"] != nil {
				return &AdmissionRefused{Msg: "plugins must be a mapping in " + filepath.Join(h, "config.yaml")}
			}
			section = map[string]any{}
			doc["plugins"] = section
		}
		section["enabled"] = sortedCopy(sortedKeys(enabled))
		section["disabled"] = sortedCopy(sortedKeys(disabled))
		settleDisabledByUpdate(section, opts.Settled, disabled)
		return nil
	})
}

// Forgets the update records of settled plugins and of plugins no longer disabled, dropping an empty record
func settleDisabledByUpdate(section map[string]any, settled []string, disabled map[string]bool) {
	record, ok := section[disabledByUpdateKey].(map[string]any)
	if !ok {
		delete(section, disabledByUpdateKey)
		return
	}
	for _, name := range settled {
		delete(record, name)
	}
	for name := range record {
		if !disabled[name] {
			delete(record, name)
		}
	}
	if len(record) == 0 {
		delete(section, disabledByUpdateKey)
	}
}
