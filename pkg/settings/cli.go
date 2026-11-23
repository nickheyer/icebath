package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

func init() {
	config.RegisterSettingsHooks(config.SettingsHooks{
		Known: func(key string) bool { return FindKey(key) != nil },
		Set:   cliSet,
		Reset: cliReset,
		Get:   cliGet,
		Show:  cliShow,
		Keys:  allKeys,
	})
}

// Key of every registered setting
func allKeys() []string {
	entries := All()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Info().Key)
	}
	return out
}

// Text config show prints for a row's value
func valueText(r Row) string {
	switch v := r.Value.(type) {
	case nil:
		if r.Null != "" {
			return "not set (" + r.Null + ")"
		}
		return "not set"
	case bool:
		if r.Kind == KindSecret {
			return map[bool]string{true: "set", false: "not set"}[v]
		}
		return map[bool]string{true: "on", false: "off"}[v]
	case string:
		if v == "" {
			return "not set"
		}
		return v
	case []any:
		if len(v) == 0 {
			return "none"
		}
		return strings.Join(config.AsStrings(v), ", ")
	case map[string]any:
		return fmt.Sprintf("%d entries", len(v))
	}
	if r.Unit != "" {
		return fmt.Sprintf("%v %s", r.Value, r.Unit)
	}
	return fmt.Sprint(r.Value)
}

// Every settings group with its everyday rows and the advanced rows that are set, for config show
func cliShow(ctx context.Context) []config.ShowGroup {
	var out []config.ShowGroup
	for _, g := range Groups() {
		group := config.ShowGroup{Title: g.Label}
		for _, r := range Rows(ctx, Query{Group: g.ID, Advanced: true}) {
			if r.Advanced && r.Source == SourceDefault {
				continue
			}
			group.Rows = append(group.Rows, config.ShowRow{Label: r.Label, Value: valueText(r), Source: string(r.Source)})
		}
		if len(group.Rows) > 0 {
			out = append(out, group)
		}
	}
	return out
}

// Value the text typed on the command line stands for: lists, mappings and free values parsed as YAML, text otherwise
func cliValue(e Entry, key, text string) (any, error) {
	switch e.Kind() {
	case KindList, KindMap, KindObject, KindAny:
		return config.ParseCLIValue(key, text)
	}
	return text, nil
}

// Writes typed text to a setting in the profile through Set, the stored value back
func cliSet(ctx context.Context, key, text string) (any, bool, error) {
	e := FindKey(key)
	if e == nil {
		return nil, false, nil
	}
	value, err := cliValue(e, key, text)
	if err != nil {
		return nil, true, err
	}
	change, err := Set(ctx, key, value, Global)
	if err != nil {
		return nil, true, err
	}
	return change.New.Value, true, nil
}

// Removes a setting's value from the profile through Reset, whether it held one
func cliReset(ctx context.Context, key string) (bool, bool, error) {
	if FindKey(key) == nil {
		return false, false, nil
	}
	change, err := Reset(ctx, key, Global)
	if err != nil {
		return false, true, err
	}
	return change.Old.Source == SourceGlobal, true, nil
}

// Value a setting has in the profile
func cliGet(ctx context.Context, key string) (any, bool) {
	if FindKey(key) == nil {
		return nil, false
	}
	v, err := Resolve(ctx, key)
	if err != nil {
		return nil, false
	}
	return v.Value, true
}
