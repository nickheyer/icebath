package config

import "strings"

// The declared field of one entry of the table that a path inside the table names, the entry's name then the field's key, as a setting of
// its own keyed by the table's key, the entry's name as the path spells it and the field's key as declared; false for a table whose entries
// declare no named fields, or a path that names none of them
func (s Setting) EntryField(path string) (Setting, bool) {
	if s.Entry == nil {
		return Setting{}, false
	}
	name, field, ok := strings.Cut(path, ".")
	if !ok || name == "" || strings.Contains(field, ".") {
		return Setting{}, false
	}
	declared, ok := s.Entry.field(field)
	if !ok {
		return Setting{}, false
	}
	declared.Key = s.Key + "." + name + "." + declared.Key
	return declared, true
}

// The setting that the table declares for the part of its value a path inside it names, keyed by the table's key and the path, the names of
// entries as the path spells them and the keys of fields as declared: the value of an entry that is one value, a declared field of an entry
// of named fields, or such a part of a table one of those is; false for a path the table declares nothing for
func (s Setting) Part(path string) (Setting, bool) {
	name, rest, more := strings.Cut(path, ".")
	if s.Entry == nil || name == "" {
		return Setting{}, false
	}
	part, single := s.Entry.single()
	if single {
		part.Key = s.Key + "." + name
	} else {
		field, deeper, further := strings.Cut(rest, ".")
		declared, ok := s.Entry.field(field)
		if !more || !ok {
			return Setting{}, false
		}
		declared.Key = s.Key + "." + name + "." + declared.Key
		part, rest, more = declared, deeper, further
	}
	switch {
	case !more:
		return part, true
	case part.Kind() == Table:
		return part.Part(rest)
	}
	return Setting{}, false
}

// Whether the entries of the table are things of several named fields, each of which is kept under its own key below the entry
func (s Setting) Things() bool {
	if s.Entry == nil {
		return false
	}
	for _, field := range s.Entry.Fields {
		if field.Key != "" {
			return true
		}
	}
	return false
}
