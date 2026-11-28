package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// How a key answers to the names the settings hold, one rule for every scope: a config.yaml, the administrator's, Remote Config and what one
// session sets. A key is split on its dots and each part answers to the name spelled exactly as it is. A key icebath declares, a section, a
// setting or a field of an entry, is matched in any case when no name is spelled exactly so, and is written as it is declared. So is the
// name of a thing that has sections of its own, such as a provider or a server, whose names a lower case rule makes. The name of an entry
// of a table and every name inside a group of values answer only to the name spelled exactly, unless the table says that its names ignore
// case, as HTTP headers do. Anything else that nothing declares answers in any case and keeps the spelling it has

// How one part of a key answers to the names held
type match uint8

const (
	// Only to the name spelled exactly as the part: a name a person gives
	exact match = iota
	// To the name spelled exactly, else to the first in order that matches in any case, which keeps its spelling: a name nothing declares
	loose
	// As loose, but written as the part spells it: a key icebath declares, or the name of an entry of a table whose names ignore case
	spelled
)

// Key as the settings keep it: its parts, and how each answers to the names held
type keyPath struct {
	parts []string
	match []match
}

func (p keyPath) String() string {
	return strings.Join(p.parts, ".")
}

func (p *keyPath) add(part string, how match) {
	p.parts = append(p.parts, part)
	p.match = append(p.match, how)
}

// Path of a key icebath registered, every part written as the key spells it
func registered(key string) keyPath {
	var path keyPath
	for part := range strings.SplitSeq(key, ".") {
		path.add(part, spelled)
	}
	return path
}

// Whether two spellings name the same thing as the part answers to names
func same(a, b string, how match) bool {
	return a == b || how != exact && strings.EqualFold(a, b)
}

// Whether the key sits below the prefix, the prefix matched as the part answers to names
func below(key, prefix string, how match) bool {
	return len(key) > len(prefix) && key[len(prefix)] == '.' && same(key[:len(prefix)], prefix, how)
}

// Name of the names held that the part answers to: the one spelled exactly as it is, else, unless it answers only so, the first in order
// that matches it in any case; false when none does
func pick(names []string, part string, how match) (string, bool) {
	if slices.Contains(names, part) {
		return part, true
	}
	found, ok := "", false
	if how == exact {
		return found, ok
	}
	for _, name := range names {
		if strings.EqualFold(name, part) && (!ok || name < found) {
			found, ok = name, true
		}
	}
	return found, ok
}

// Name of the mapping that the part answers to, false when none does
func pickIn(m map[string]any, part string, how match) (string, bool) {
	if _, ok := m[part]; ok || how == exact {
		return part, ok
	}
	return pick(slices.Collect(maps.Keys(m)), part, how)
}

// Value under the path in a tree of settings, false when it holds nothing there
func lookup(tree map[string]any, path keyPath) (any, bool) {
	var node any = tree
	for i, part := range path.parts {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := pickIn(m, part, path.match[i])
		if !ok {
			return nil, false
		}
		node = m[name]
	}
	return node, true
}

// Path with each part that keeps the spelling held spelled as the tree holds it, as far as the tree holds the path
func resolve(tree map[string]any, path keyPath) keyPath {
	out := keyPath{parts: slices.Clone(path.parts), match: slices.Clone(path.match)}
	var node any = tree
	for i, part := range path.parts {
		m, ok := node.(map[string]any)
		if !ok {
			break
		}
		name, ok := pickIn(m, part, path.match[i])
		if !ok {
			break
		}
		if path.match[i] == loose {
			out.parts[i] = name
		}
		node = m[name]
	}
	return out
}

// Copy of a value of tables and lists that can be changed without changing the value
func copied(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = copied(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = copied(item)
		}
		return out
	}
	return value
}

// Value of a YAML document with every mapping keyed by text, as settings are read
func plain(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = plain(item)
		}
		return v
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprint(key)] = plain(item)
		}
		return out
	case []any:
		for i, item := range v {
			v[i] = plain(item)
		}
		return v
	}
	return value
}

// Why a name a person types for a thing, such as an entry of a table, can't be one part of a key, nil when it can
func KeyName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("Type a name.")
	case strings.Contains(name, "."):
		return errors.New("A name can't hold a dot. Use a dash or an underscore instead.")
	}
	return nil
}

// Why a key can't be written, nil when it can: a part of it is empty
func writableKey(path keyPath) error {
	if len(path.parts) == 0 || slices.Contains(path.parts, "") {
		return fmt.Errorf("%q can't be the key of a setting. Check for a dot at its start or end, or two dots in a row.", path.String())
	}
	return nil
}

// Sections one lookup goes through: the fixed ones first, then those that come and go, made once and only when the fixed ones leave a key
// unfound. While sections that come and go are made, only the fixed ones, since making them reads keys
type finder struct {
	values      *Config
	registrants []Registrant
	// Fixed sections, each batch as it was registered
	batches   [][]Section
	fixedOnly bool
	moving    []Section
	made      bool
}

// Finder of every section, whatever reads through it
func (c *Config) lookup() *finder {
	return c.snapshot().finder(c)
}

// Finder of the sections a key of these settings is planned with: the fixed ones alone while sections that come and go are made
func (c *Config) planner() *finder {
	f := c.lookup()
	f.fixedOnly = c.making
	return f
}

// Sections that come and go, made the first time they are asked for
func (f *finder) movingSections() []Section {
	if f.made {
		return f.moving
	}
	f.made = true
	if f.fixedOnly {
		return nil
	}
	for _, registrant := range f.registrants {
		if _, fixed := registrant.(Sections); !fixed {
			f.moving = append(f.moving, registrant.Sections(f.values.makingSections())...)
		}
	}
	return f.moving
}

// Setting registered under the key with its section: one spelled exactly as the key first, then one that matches it in any case
func (f *finder) setting(key string) (Section, Setting, bool) {
	key = strings.TrimSpace(key)
	for _, how := range []match{exact, spelled} {
		for _, fixed := range f.batches {
			if section, setting, ok := find(fixed, key, how); ok {
				return section, setting, true
			}
		}
		if section, setting, ok := find(f.movingSections(), key, how); ok {
			return section, setting, true
		}
	}
	return Section{}, Setting{}, false
}

// Section registered under the key: one spelled exactly as the key first, then one that matches it in any case
func (f *finder) section(key string) (Section, bool) {
	key = strings.TrimSpace(key)
	for _, how := range []match{exact, spelled} {
		for _, fixed := range f.batches {
			if section, ok := findSection(fixed, key, how); ok {
				return section, true
			}
		}
		if section, ok := findSection(f.movingSections(), key, how); ok {
			return section, true
		}
	}
	return Section{}, false
}

func findSection(sections []Section, key string, how match) (Section, bool) {
	for _, section := range sections {
		if same(section.Key, key, how) {
			return section, true
		}
	}
	return Section{}, false
}

// Table setting a key below it names a part of, with the path of the part inside the table as the key spells it
func (f *finder) under(key string) (Setting, string, bool) {
	parts := strings.Split(strings.TrimSpace(key), ".")
	for n := len(parts) - 1; n > 0; n-- {
		if _, setting, ok := f.setting(strings.Join(parts[:n], ".")); ok {
			if setting.Kind() != Table {
				break
			}
			return setting, strings.Join(parts[n:], "."), true
		}
	}
	return Setting{}, "", false
}

// Key as the settings keep it: a setting's key as it is registered, a key inside a table setting with the names of its entries as written
// and its declared fields as declared, any other with the sections it names spelled as registered
func (f *finder) path(key string) keyPath {
	key = strings.TrimSpace(key)
	if key == "" {
		return keyPath{}
	}
	if _, setting, ok := f.setting(key); ok {
		return registered(setting.Key)
	}
	if table, rest, ok := f.under(key); ok {
		path := registered(table.Key)
		inside := table.inside(rest, f.fixedOnly)
		path.parts, path.match = append(path.parts, inside.parts...), append(path.match, inside.match...)
		return path
	}
	var path keyPath
	for part := range strings.SplitSeq(key, ".") {
		path.add(part, loose)
	}
	for n := 1; n <= len(path.parts); n++ {
		section, ok := f.section(strings.Join(path.parts[:n], "."))
		if !ok {
			break
		}
		path.parts[n-1], path.match[n-1] = section.Key[strings.LastIndex(section.Key, ".")+1:], spelled
	}
	return path
}

// Parts of a path inside the table's value: the name of an entry as written, answering in any case where the table says that its names
// ignore case; a declared field of an entry as declared; the names inside a group of values as written. While sections that come and go
// are made, open, the names inside an entry of a table that declares no fields are declared by nothing known, and answer in any case
func (s Setting) inside(rest string, open bool) keyPath {
	var path keyPath
	table, entry, free := &s, (*Entry)(nil), exact
	for part := range strings.SplitSeq(rest, ".") {
		switch {
		case table != nil:
			how := exact
			if table.Entry != nil && table.Entry.IgnoreCase {
				how = spelled
			}
			path.add(part, how)
			if open && len(table.Entry.fields()) == 0 {
				free = loose
			}
			entry, table = table.Entry, nil
			if field, one := entry.single(); one {
				entry = nil
				if field.Kind() == Table {
					table = &field
				}
			}
		case entry != nil:
			field, ok := entry.field(part)
			entry = nil
			if !ok {
				path.add(part, free)
				continue
			}
			path.add(field.Key, spelled)
			if field.Kind() == Table {
				table = &field
			}
		default:
			path.add(part, free)
		}
	}
	return path
}

// Fields an entry declares, none for no entry
func (e *Entry) fields() []Setting {
	if e == nil {
		return nil
	}
	return e.Fields
}

// Field of an entry that holds one value, false for an entry of named fields or of none
func (e *Entry) single() (Setting, bool) {
	if e == nil || len(e.Fields) != 1 || e.Fields[0].Key != "" {
		return Setting{}, false
	}
	return e.Fields[0], true
}

// Named field of an entry the name answers to as a declared key does
func (e *Entry) field(name string) (Setting, bool) {
	var keys []string
	for _, field := range e.Fields {
		if field.Key != "" {
			keys = append(keys, field.Key)
		}
	}
	key, ok := pick(keys, name, spelled)
	if !ok {
		return Setting{}, false
	}
	at := slices.IndexFunc(e.Fields, func(field Setting) bool { return field.Key == key })
	return e.Fields[at], true
}

// Part of a table's value under a path inside it, nil when it holds nothing there
func (s Setting) dig(value any, rest string) any {
	table, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	found, _ := lookup(table, s.inside(rest, false))
	return found
}

// What icebath declares at one place in the tree of settings: the names below it as declared and the setting kept there
type declared struct {
	below   map[string]*declared
	setting *Setting
	// Answers only to its name spelled exactly: the name of a thing that has sections of its own
	exact bool
}

// Tree of what the sections declare. A section that comes and goes under another names a thing with the parts its key adds to the key of
// the section it sits under, and those answer only exactly
func schema(fixed, moving []Section) *declared {
	root := &declared{}
	for _, section := range fixed {
		root.declare(section, -1)
	}
	for _, section := range moving {
		own := -1
		if section.Parent != "" {
			own = strings.Count(section.Parent, ".") + 1
		}
		root.declare(section, own)
	}
	return root
}

// Adds the section and its settings, the parts of its key from the one at own on names when own is not negative
func (d *declared) declare(section Section, own int) {
	d.at(section.Key, own)
	for i := range section.Settings {
		setting := section.Settings[i]
		d.at(setting.Key, -1).setting = &setting
	}
}

// Place of the key, made when it is not there; the parts from the one at own on are names when own is not negative
func (d *declared) at(key string, own int) *declared {
	node := d
	for i, part := range strings.Split(key, ".") {
		if node.below == nil {
			node.below = map[string]*declared{}
		}
		next := node.below[part]
		if next == nil {
			next = &declared{exact: own >= 0 && i >= own}
			node.below[part] = next
		}
		node = next
	}
	return node
}

// Declared name below the place that the name answers to and what is declared there; the name itself and nil for one nothing declares
func (d *declared) child(name string) (string, *declared) {
	if d == nil {
		return name, nil
	}
	if next, ok := d.below[name]; ok {
		return name, next
	}
	var keys []string
	for key, next := range d.below {
		if !next.exact {
			keys = append(keys, key)
		}
	}
	if key, ok := pick(keys, name, spelled); ok {
		return key, d.below[key]
	}
	return name, nil
}

// What one entry of the table at the place holds: the thing of the name when it has sections of its own, with the fields the table declares
func (d *declared) entry(name string) *declared {
	fields := entryOf(d.setting.Entry)
	thing := d.below[name]
	switch {
	case thing == nil:
		return fields
	case fields == nil || fields.setting != nil:
		return thing
	}
	out := &declared{below: maps.Clone(thing.below), setting: thing.setting, exact: thing.exact}
	if out.below == nil {
		out.below = map[string]*declared{}
	}
	for key, field := range fields.below {
		if _, ok := out.below[key]; !ok {
			out.below[key] = field
		}
	}
	return out
}

// What one entry declares: its named fields, or the one value it is; nil for an entry of no fields
func entryOf(entry *Entry) *declared {
	if entry == nil || len(entry.Fields) == 0 {
		return nil
	}
	if field, one := entry.single(); one {
		return &declared{setting: &field}
	}
	out := &declared{below: map[string]*declared{}}
	for i := range entry.Fields {
		if field := entry.Fields[i]; field.Key != "" {
			out.below[field.Key] = &declared{setting: &field}
		}
	}
	return out
}

// Whether the place is a table whose entries are named in any case
func (d *declared) caseless() bool {
	return d != nil && d.setting != nil && d.setting.Kind() == Table && d.setting.Entry != nil && d.setting.Entry.IgnoreCase
}

// Place one name further down: an entry of a table, else the declared name below
func (d *declared) step(name string) *declared {
	if d == nil {
		return nil
	}
	if d.setting != nil {
		if d.setting.Kind() == Table {
			return d.entry(name)
		}
		return nil
	}
	_, next := d.child(name)
	return next
}

// Place of the path below this one
func (d *declared) walk(parts []string) *declared {
	node := d
	for _, part := range parts {
		node = node.step(part)
	}
	return node
}

// What one record of a list of records holds, nil for any other place
func (d *declared) record() *declared {
	if d == nil || d.setting == nil || d.setting.Kind() != List {
		return nil
	}
	return entryOf(d.setting.Entry)
}

// Value with the names icebath declares in it spelled as declared, two that then share a spelling merged into one: the one spelled so
// already wins, else the first in order. Names of entries and names inside groups of values stay as written
func (d *declared) canon(value any) any {
	if d == nil {
		return value
	}
	if items, ok := value.([]any); ok {
		record := d.record()
		if record == nil {
			return value
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = record.canon(item)
		}
		return out
	}
	m, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := make(map[string]any, len(m))
	if d.setting != nil {
		if d.setting.Kind() != Table {
			return value
		}
		for name, item := range m {
			out[name] = d.entry(name).canon(item)
		}
		return out
	}
	from := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		spelling, next := d.child(name)
		item := next.canon(m[name])
		have, taken := from[spelling]
		switch {
		case !taken:
			out[spelling], from[spelling] = item, name
		case name == spelling && have != spelling:
			out[spelling], from[spelling] = merged(item, out[spelling]), name
		default:
			out[spelling] = merged(out[spelling], item)
		}
	}
	return out
}

// Value of the winner with the names only the other holds added, mappings merged all the way down
func merged(winner, other any) any {
	w, ok := winner.(map[string]any)
	o, both := other.(map[string]any)
	if !ok || !both {
		return winner
	}
	out := maps.Clone(w)
	for key, item := range o {
		if have, ok := out[key]; ok {
			out[key] = merged(have, item)
		} else {
			out[key] = item
		}
	}
	return out
}

// Mapping with the keys the administrator pins over the value's, which a pin of keys below a table setting adds to rather than replaces;
// an entry of a table whose names ignore case replaces the one its name matches in any case
func overlay(value, pin any, place *declared) any {
	base, okBase := value.(map[string]any)
	over, okOver := pin.(map[string]any)
	if !okBase || !okOver {
		return pin
	}
	out := maps.Clone(base)
	for _, key := range slices.Sorted(maps.Keys(over)) {
		held := key
		if place.caseless() {
			if name, ok := pickIn(out, key, spelled); ok {
				held = name
			}
		}
		item := overlay(out[held], over[key], place.step(key))
		delete(out, held)
		out[key] = item
	}
	return out
}

// Sections on the way to the key and below it, the fixed ones and those that come and go
func (f *finder) related(key string, moving bool) (fixed, made []Section) {
	near := func(sections []Section) []Section {
		var out []Section
		for _, section := range sections {
			if key == "" || same(section.Key, key, spelled) || below(section.Key, key, spelled) || below(key, section.Key, spelled) {
				out = append(out, section)
			}
		}
		return out
	}
	for _, batch := range f.batches {
		fixed = append(fixed, near(batch)...)
	}
	if moving {
		made = near(f.movingSections())
	}
	return fixed, made
}

// What is declared at the key, from the sections on the way to it and below it: the fixed ones, and those that come and go when they are made
// already, or may be made and the fixed ones leave the names below the key undeclared: a setting that is a table whose entries declare no
// fields, or no setting
func (f *finder) place(path keyPath) *declared {
	key := path.String()
	fixed, _ := f.related(key, false)
	at := schema(fixed, nil).walk(path.parts)
	if !f.made && (f.fixedOnly || at != nil && at.setting != nil && (at.setting.Kind() != Table || len(at.setting.Entry.fields()) > 0)) {
		return at
	}
	fixed, moving := f.related(key, true)
	return schema(fixed, moving).walk(path.parts)
}

// Tree of everything the sections declare, those that come and go too unless they are being made
func (f *finder) schema() *declared {
	fixed, moving := f.related("", !f.fixedOnly)
	return schema(fixed, moving)
}
