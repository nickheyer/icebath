package settings

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// Every registered setting by key and by alias
var registry = struct {
	sync.RWMutex
	byKey   map[string]Entry
	bySlash map[string]Entry
	// Settings an environment variable name overrides, in registration order
	byEnv map[string][]Entry
	order []string
	// Map settings whose entries may also sit beside them
	beside []*Map
}{byKey: map[string]Entry{}, bySlash: map[string]Entry{}, byEnv: map[string][]Entry{}}

// Adds a setting to the registry, panicking on a duplicate key or alias or a missing description
func Register[E Entry](e E) E {
	m := e.Info()
	if m.Key == "" || strings.HasPrefix(m.Key, ".") || strings.HasSuffix(m.Key, ".") {
		panic(fmt.Sprintf("settings: invalid key %q", m.Key))
	}
	if m.Label == "" || m.Short == "" {
		panic(fmt.Sprintf("settings: %s needs a Label and a Short", m.Key))
	}
	if _, ok := GroupOf(m.Group); !ok {
		panic(fmt.Sprintf("settings: %s names unknown group %q", m.Key, m.Group))
	}
	if m.Scopes == 0 {
		m.Scopes = Global
	}
	if _, isBool := any(e).(*Bool); m.EnvInvert && (!isBool || m.Env == "") {
		panic(fmt.Sprintf("settings: %s inverts an env override it does not have or that is not on or off", m.Key))
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.byKey[m.Key]; dup {
		panic(fmt.Sprintf("settings: %s registered twice", m.Key))
	}
	registry.byKey[m.Key] = e
	registry.order = append(registry.order, m.Key)
	if m.Env != "" {
		registry.byEnv[m.Env] = append(registry.byEnv[m.Env], e)
	}
	if mp, ok := any(e).(*Map); ok && mp.Beside {
		registry.beside = append(registry.beside, mp)
	}
	if m.Slash != "" {
		if other, dup := registry.bySlash[m.Slash]; dup {
			panic(fmt.Sprintf("settings: shortcut %q names both %s and %s", m.Slash, other.Info().Key, m.Key))
		}
		registry.bySlash[m.Slash] = e
	}
	return e
}

// Registers the config blocks a plugin manifest declares as free-form map settings, skipping keys and sections the registry already covers
func RegisterPluginKeys(plugin string, keys []string) {
	for _, key := range keys {
		if key == "" || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || FindKey(key) != nil || isSection(key) {
			continue
		}
		registry.Lock()
		_, dup := registry.byKey[key]
		registry.Unlock()
		if dup {
			continue
		}
		Register(&Map{Meta: Meta{Key: key, Label: "Plugin settings", Short: "Settings the " + plugin + " plugin reads.",
			Group: GroupSystem, Order: 9000, Advanced: true, Apply: Restart}})
	}
}

// Setting with a key, nil when none
func Lookup(key string) Entry {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byKey[key]
}

// Setting an environment variable name overrides, the first registered when several take the name, nil when it overrides none
func ByEnv(name string) Entry {
	registry.RLock()
	defer registry.RUnlock()
	if entries := registry.byEnv[name]; len(entries) > 0 {
		return entries[0]
	}
	return nil
}

// Setting a key, a slash shortcut or an entry of a map setting names, nil when none
func Find(name string) Entry {
	if e := FindKey(name); e != nil {
		return e
	}
	registry.RLock()
	defer registry.RUnlock()
	return registry.bySlash[name]
}

// Setting a config key or an entry of a map setting names, never a slash shortcut; nil when none
func FindKey(name string) Entry {
	registry.RLock()
	e := registry.byKey[name]
	registry.RUnlock()
	if e != nil {
		return e
	}
	return mapChildOf(name)
}

// One named entry of a map setting, or one field of such an entry, checked by the map's entry schema
type mapChild struct {
	Entry
	meta   Meta
	parent *Map
	// Default the map's own default gives this child
	def any
}

func (c *mapChild) Info() *Meta { return &c.meta }

func (c *mapChild) DefaultValue() any {
	if c.def != nil {
		return c.def
	}
	return c.Entry.DefaultValue()
}

// Map setting a key is an entry or entry field of, with the entry name and the field names under it, nil when the key is under no map setting
func mapParentOf(key string) (parent *Map, name string, fields []string) {
	segs := config.SplitPath(key)
	for n := len(segs) - 1; n > 0; n-- {
		registry.RLock()
		p, _ := registry.byKey[strings.Join(segs[:n], ".")].(*Map)
		registry.RUnlock()
		if p == nil {
			continue
		}
		return p, segs[n], segs[n+1:]
	}
	for n := len(segs) - 1; n > 0; n-- {
		if p := besideMapOf(strings.Join(segs[:n], "."), segs[n]); p != nil {
			return p, segs[n], segs[n+1:]
		}
	}
	return nil, "", nil
}

// Section a map setting sits in, empty at the top level
func parentSection(key string) string {
	segs := config.SplitPath(key)
	return strings.Join(segs[:len(segs)-1], ".")
}

// Map setting that takes entries beside it in section under name, nil when none does or another setting there takes the name
func besideMapOf(section, name string) *Map {
	registry.RLock()
	var found *Map
	for _, m := range registry.beside {
		if parentSection(m.Key) == section {
			found = m
			break
		}
	}
	registry.RUnlock()
	if found == nil || name == "" || name != strings.TrimSpace(name) || !besideFree(section+"."+name) {
		return nil
	}
	return found
}

// Whether a key beside a map is taken by no setting and no section of settings
func besideFree(key string) bool {
	return Lookup(key) == nil && !isSection(key)
}

// Typed entries written beside a map setting in the profile's config.yaml, by name
func besideEntries(ctx context.Context, m *Map) map[string]any {
	section := parentSection(m.Key)
	l := readLayers(ctx)
	held := config.AsMap(l.doc)
	if section != "" {
		v, _ := lookupPath(l.doc, section)
		held = config.AsMap(v)
	}
	out := map[string]any{}
	for name, raw := range held {
		key := name
		if section != "" {
			key = section + "." + name
		}
		if config.AsMap(raw) == nil || !besideFree(key) {
			continue
		}
		v, _ := storedValue(ctx, l.doc, key)
		if m.Value == nil {
			out[name] = v
			continue
		}
		if typed, ok := readPart(m.Value, v); ok {
			out[name] = typed
		}
	}
	return out
}

// Key of one entry of a map setting, dots in the entry name kept literal
func (m *Map) EntryKey(name string) string {
	return m.Key + "." + strings.ReplaceAll(name, ".", `\.`)
}

// Entry of a map setting a dotted key names: one named entry, or a field any depth inside an entry that follows a schema; nil when the key is under no map, the entry name is blank or padded with spaces, or the schema has no such field
func mapChildOf(key string) Entry {
	parent, name, fields := mapParentOf(key)
	if parent == nil || name == "" || name != strings.TrimSpace(name) {
		return nil
	}
	meta := parent.Meta
	meta.Key, meta.Slash, meta.Validate, meta.Companions, meta.Derive = key, "", nil, nil, nil
	def := config.AsMap(parent.DefaultValue())[name]
	if parent.Value == nil {
		v, _ := lookupPath(config.AsMap(parent.DefaultValue()), strings.TrimPrefix(key, parent.Key+"."))
		return &mapChild{Entry: &anyValue{}, meta: meta, parent: parent, def: v}
	}
	entry := parent.Value
	for rest := fields; len(rest) > 0; {
		obj, ok := entry.(*Object)
		if !ok {
			return nil
		}
		field, taken := obj.fieldAt(rest)
		if field == nil {
			return nil
		}
		for _, part := range rest[:taken] {
			def = config.AsMap(def)[part]
		}
		entry, rest = field, rest[taken:]
	}
	info := entry.Info()
	meta.Validate = info.Validate
	if len(fields) > 0 {
		meta.Label, meta.Short, meta.Help = info.Label, info.Short, info.Help
	}
	return &mapChild{Entry: entry, meta: meta, parent: parent, def: def}
}

// Every setting in group and order
func All() []Entry {
	registry.RLock()
	out := make([]Entry, 0, len(registry.order))
	for _, key := range registry.order {
		out = append(out, registry.byKey[key])
	}
	registry.RUnlock()
	sortEntries(out)
	return out
}

// Sorts settings by group, everyday before advanced, then order and key
func sortEntries(entries []Entry) {
	groupOrder := map[GroupID]int{}
	for _, g := range groups {
		groupOrder[g.ID] = g.Order
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Info(), entries[j].Info()
		if groupOrder[a.Group] != groupOrder[b.Group] {
			return groupOrder[a.Group] < groupOrder[b.Group]
		}
		if a.Advanced != b.Advanced {
			return !a.Advanced
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Key < b.Key
	})
}
