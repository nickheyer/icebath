package settings

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// One settings screen row: the schema of a setting, its value in effect and where that value came from
type Row struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Short    string   `json:"short"`
	Help     string   `json:"help,omitempty"`
	Group    GroupID  `json:"group"`
	Order    int      `json:"order"`
	Kind     Kind     `json:"kind"`
	Default  any      `json:"default"`
	Null     string   `json:"null,omitempty"`
	Choices  []Choice `json:"choices,omitempty"`
	Open     bool     `json:"open,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Scopes   []string `json:"scopes"`
	Apply    string   `json:"apply"`
	Advanced bool     `json:"advanced,omitempty"`
	Slash    string   `json:"slash,omitempty"`
	Env      string   `json:"env,omitempty"`
	Value    any      `json:"value"`
	Source   Source   `json:"source"`
	Locked   bool     `json:"locked,omitempty"`
	// The env override says the opposite of the setting: on there turns it off
	EnvInvert bool `json:"env_invert,omitempty"`
	// The session the row was read for holds its own value
	SessionSet bool `json:"session_set,omitempty"`
	// That session's own value, present when SessionSet
	Session any `json:"session,omitempty"`
	// Value without the session's own: what the profile gives every other session
	Profile any `json:"profile,omitempty"`
	// Source of Profile
	ProfileSource Source `json:"profile_source,omitempty"`
	// Entries of a map setting in name order, each with its full key and value
	Entries []MapEntry `json:"entries,omitempty"`
	// Schema of one entry of a map setting or one item of a list setting, when they follow one
	EntrySchema *Row `json:"entry_schema,omitempty"`
	// Schema of each field of an object setting or of a map entry that is an object
	Fields []Row `json:"fields,omitempty"`
	// Chat apps that override this setting, keyed by app, with their own values
	Platforms map[string]any `json:"platforms,omitempty"`
}

// One named entry of a map setting
type MapEntry struct {
	Name  string `json:"name"`
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// Float view of an optional bound
func boundOf[T int | float64](p *T) *float64 {
	if p == nil {
		return nil
	}
	f := float64(*p)
	return &f
}

// Float view of an optional duration bound in the entry's unit
func durationBound(p *time.Duration, unit time.Duration) *float64 {
	if p == nil {
		return nil
	}
	f := float64(*p) / float64(unit)
	return &f
}

// Short name of a duration unit
func unitName(unit time.Duration) string {
	switch unit {
	case time.Millisecond:
		return "ms"
	case time.Minute:
		return "min"
	case time.Hour:
		return "h"
	case 24 * time.Hour:
		return "days"
	}
	return "s"
}

// Schema part of a row: kind, choices, bounds and what unset means, read from the entry's own type
func schemaOf(row *Row, e Entry) {
	if child, isChild := e.(*mapChild); isChild {
		e = child.Entry
	}
	row.Kind = e.Kind()
	switch t := e.(type) {
	case *Bool:
		row.Null = t.Null
	case *Int:
		row.Null, row.Unit, row.Min, row.Max = t.Null, t.Unit, boundOf(t.Min), boundOf(t.Max)
	case *Float:
		row.Null, row.Unit, row.Min, row.Max = t.Null, t.Unit, boundOf(t.Min), boundOf(t.Max)
	case *Duration:
		row.Null, row.Unit = t.Null, unitName(t.unit())
		row.Min, row.Max = durationBound(t.Min, t.unit()), durationBound(t.Max, t.unit())
	case *Enum:
		row.Null, row.Choices, row.Open = t.Null, t.Values, t.Open
	case *String:
		row.Null = t.Null
		if t.Masked {
			row.Kind = KindSecret
		}
	case *List:
		row.Null = t.Null
		if t.Item != nil {
			item := Row{Key: t.Key + ".*", Label: t.Item.Info().Label, Short: t.Item.Info().Short, Help: t.Item.Info().Help,
				Default: t.Item.DefaultValue()}
			schemaOf(&item, t.Item)
			row.EntrySchema = &item
		}
	case *Map:
		if t.Value != nil {
			entry := Row{Key: t.Key + ".*", Label: t.Value.Info().Label, Short: t.Value.Info().Short, Help: t.Value.Info().Help,
				Default: t.Value.DefaultValue()}
			schemaOf(&entry, t.Value)
			row.EntrySchema = &entry
		}
	case *Object:
		for _, f := range t.Fields {
			fm := f.Info()
			field := Row{Key: fm.Key, Label: fm.Label, Short: fm.Short, Help: fm.Help, Default: f.DefaultValue()}
			schemaOf(&field, f)
			row.Fields = append(row.Fields, field)
		}
	}
}

// Whether a row hides its value, showing only whether one is set
func masked(e Entry) bool {
	if child, isChild := e.(*mapChild); isChild {
		e = child.Entry
	}
	switch t := e.(type) {
	case *Secret:
		return true
	case *String:
		return t.Masked
	}
	return false
}

// Row of one setting with its value in effect for ctx, secrets reduced to set or not set
func Describe(ctx context.Context, e Entry) Row {
	m := e.Info()
	v := resolveEntry(ctx, e)
	row := Row{Key: m.Key, Label: m.Label, Short: m.Short, Help: m.Help, Group: m.Group, Order: m.Order,
		Default: e.DefaultValue(), Scopes: m.Scopes.Names(), Apply: m.Apply.String(), Advanced: m.Advanced, Slash: m.Slash,
		Env: m.Env, EnvInvert: m.EnvInvert, Value: v.Value, Source: v.Source, Locked: v.Locked}
	schemaOf(&row, e)
	if mp, isMap := e.(*Map); isMap {
		row.Entries = mapEntries(mp, v.Value)
	}
	if m.Platforms {
		row.Platforms = PlatformOverrides(ctx, e)
	}
	if sid := SessionFrom(ctx); sid != "" && m.Scopes&Session != 0 {
		if sv, ok := sessionValue(ctx, sid, m.Key); ok {
			row.SessionSet, row.Session = true, sv
		}
		profile := resolveEntry(WithoutSession(ctx), e)
		row.Profile, row.ProfileSource = profile.Value, profile.Source
	}
	if masked(e) {
		row.Default = false
		row.Value = row.Value != nil && row.Value != ""
		if row.SessionSet {
			row.Session = true
		}
		if row.Profile != nil {
			row.Profile = row.Profile != ""
		}
	}
	return row
}

// Entries of a map setting's value in name order
func mapEntries(m *Map, value any) []MapEntry {
	entries := config.AsMap(value)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]MapEntry, 0, len(names))
	for _, name := range names {
		out = append(out, MapEntry{Name: name, Key: m.Key + "." + name, Value: entries[name]})
	}
	return out
}

// Filter of a settings listing: a group, a key prefix, a search text and whether advanced rows are included
type Query struct {
	Group    GroupID  `json:"group,omitempty"`
	Prefix   string   `json:"prefix,omitempty"`
	Search   string   `json:"search,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Advanced bool     `json:"advanced,omitempty"`
}

// Whether an entry passes a query
func (q Query) match(e Entry) bool {
	m := e.Info()
	if len(q.Keys) > 0 {
		for _, k := range q.Keys {
			if k == m.Key || k == m.Slash {
				return true
			}
		}
		return false
	}
	if q.Group != "" && m.Group != q.Group {
		return false
	}
	if q.Prefix != "" && m.Key != q.Prefix && !strings.HasPrefix(m.Key, q.Prefix+".") {
		return false
	}
	if m.Advanced && !q.Advanced && q.Search == "" {
		return false
	}
	if q.Search != "" {
		needle := strings.ToLower(q.Search)
		hay := strings.ToLower(m.Key + " " + m.Label + " " + m.Short + " " + m.Slash)
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

// Rows of every setting a query selects, in screen order; keys naming an entry of a map setting answer that entry's row
func Rows(ctx context.Context, q Query) []Row {
	var out []Row
	seen := map[string]bool{}
	for _, e := range All() {
		if q.match(e) {
			out = append(out, Describe(ctx, e))
			seen[e.Info().Key] = true
		}
	}
	for _, k := range q.Keys {
		if e := Find(k); e != nil && !seen[e.Info().Key] {
			out = append(out, Describe(ctx, e))
			seen[e.Info().Key] = true
		}
	}
	return out
}
