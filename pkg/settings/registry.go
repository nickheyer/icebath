package settings

import (
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
	order   []string
}{byKey: map[string]Entry{}, bySlash: map[string]Entry{}}

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
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.byKey[m.Key]; dup {
		panic(fmt.Sprintf("settings: %s registered twice", m.Key))
	}
	registry.byKey[m.Key] = e
	registry.order = append(registry.order, m.Key)
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

// Map setting a key is an entry or entry field of, with the entry name and the field path, nil when the key is under no map setting
func mapParentOf(key string) (parent *Map, name, field string) {
	segs := config.SplitPath(key)
	for n := len(segs) - 1; n > 0; n-- {
		registry.RLock()
		p, _ := registry.byKey[strings.Join(segs[:n], ".")].(*Map)
		registry.RUnlock()
		if p == nil {
			continue
		}
		return p, segs[n], strings.Join(segs[n+1:], ".")
	}
	return nil, "", ""
}

// Key of one entry of a map setting, dots in the entry name kept literal
func (m *Map) EntryKey(name string) string {
	return m.Key + "." + strings.ReplaceAll(name, ".", `\.`)
}

// Entry of a map setting a dotted key names, nil when the key is under no map with an entry schema
func mapChildOf(key string) Entry {
	parent, name, field := mapParentOf(key)
	if parent == nil || name == "" {
		return nil
	}
	meta := parent.Meta
	meta.Key, meta.Slash, meta.Companions, meta.Derive = key, "", nil, nil
	def := config.AsMap(parent.DefaultValue())[name]
	if parent.Value == nil {
		v, _ := lookupPath(config.AsMap(parent.DefaultValue()), strings.TrimPrefix(key, parent.Key+"."))
		return &mapChild{Entry: &anyValue{}, meta: meta, parent: parent, def: v}
	}
	if field == "" {
		return &mapChild{Entry: parent.Value, meta: meta, parent: parent, def: def}
	}
	obj, ok := parent.Value.(*Object)
	if !ok {
		return nil
	}
	f := obj.Field(field)
	if f == nil {
		return nil
	}
	fm := f.Info()
	meta.Label, meta.Short, meta.Help = fm.Label, fm.Short, fm.Help
	return &mapChild{Entry: f, meta: meta, parent: parent, def: config.AsMap(def)[field]}
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
