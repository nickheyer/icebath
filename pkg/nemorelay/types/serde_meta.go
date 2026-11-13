package types

import (
	"reflect"
	"strings"
	"sync"
)

// fieldMeta describes how one Go struct field maps onto its serde field
type fieldMeta struct {
	index        []int
	name         string
	rustName     string
	option       bool
	hasDefault   bool
	defaultText  *string
	skipNone     bool
	skipEmpty    bool
	skipNull     bool
	flatten      bool
	timestamp    bool
	leapIndex    []int
	serdeSkip    bool
	rustHidden   bool
	presentIndex []int
}

// structMeta describes the serde shape of a Go struct type
type structMeta struct {
	rustName    string
	fields      []fieldMeta
	denyUnknown bool
	hasFlatten  bool
	wireNames   []string
}

type rustNamed interface {
	rustName() string
}

type serdeDenyUnknown interface {
	serdeDenyUnknownFields()
}

// serdeSerializer types produce an intermediate value that the encoder writes in their place
type serdeSerializer interface {
	serdeSerialize() (any, error)
}

// serdeDeserializer types decode themselves from a JSON value with serde error texts
type serdeDeserializer interface {
	serdeDeserialize(v any) error
}

// rustDebugger types write their own Rust Debug form
type rustDebugger interface {
	rustDebug(b *strings.Builder)
}

var structMetaCache sync.Map

var (
	rustNamedType        = reflect.TypeFor[rustNamed]()
	serdeDenyUnknownType = reflect.TypeFor[serdeDenyUnknown]()
)

// Builds and caches the serde field layout of a struct type
func metaOf(t reflect.Type) *structMeta {
	if cached, ok := structMetaCache.Load(t); ok {
		return cached.(*structMeta)
	}
	m := &structMeta{rustName: t.Name()}
	if t.Implements(rustNamedType) {
		m.rustName = reflect.Zero(t).Interface().(rustNamed).rustName()
	}
	m.denyUnknown = t.Implements(serdeDenyUnknownType)
	byName := map[string]int{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		byName[sf.Name] = i
	}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		fm := fieldMeta{index: sf.Index}
		jsonTag := sf.Tag.Get("json")
		name, _, _ := strings.Cut(jsonTag, ",")
		if name == "-" {
			fm.serdeSkip = true
			name = ""
		}
		if name == "" && !fm.serdeSkip {
			name = sf.Name
		}
		fm.name = name
		fm.rustName = name
		for _, opt := range strings.Split(sf.Tag.Get("serde"), ",") {
			key, value, hasValue := strings.Cut(opt, "=")
			switch key {
			case "default":
				fm.hasDefault = true
				if hasValue {
					text := value
					fm.defaultText = &text
				}
			case "option":
				fm.option = true
			case "skip_none":
				fm.skipNone = true
			case "skip_empty":
				fm.skipEmpty = true
			case "skip_null":
				fm.skipNull = true
				fm.option = true
			case "flatten":
				fm.flatten = true
			case "timestamp":
				fm.timestamp = true
			}
		}
		if sf.Anonymous && jsonTag == "" {
			fm.flatten = true
		}
		if rustTag, ok := sf.Tag.Lookup("rust"); ok {
			if rustTag == "-" {
				fm.rustHidden = true
			} else {
				fm.rustName = rustTag
			}
		} else if fm.serdeSkip {
			fm.rustHidden = true
		}
		if sf.Type.Kind() == reflect.Pointer {
			fm.option = true
		}
		if j, ok := byName[sf.Name+"Present"]; ok && sf.Type.Kind() == reflect.Interface {
			fm.presentIndex = t.Field(j).Index
		}
		if j, ok := byName[sf.Name+"Leap"]; ok && fm.timestamp {
			fm.leapIndex = t.Field(j).Index
		}
		if fm.flatten {
			m.hasFlatten = true
		} else if !fm.serdeSkip {
			m.wireNames = append(m.wireNames, fm.name)
		}
		m.fields = append(m.fields, fm)
	}
	actual, _ := structMetaCache.LoadOrStore(t, m)
	return actual.(*structMeta)
}

// Reports whether an optional field holds Rust None
func (f *fieldMeta) isNone(fv reflect.Value, parent reflect.Value) bool {
	switch fv.Kind() {
	case reflect.Pointer, reflect.Map:
		return fv.IsNil()
	case reflect.Slice:
		return fv.IsNil()
	case reflect.Interface:
		if !fv.IsNil() {
			return false
		}
		if f.presentIndex != nil {
			return !parent.FieldByIndex(f.presentIndex).Bool()
		}
		return true
	}
	return false
}

// Reports whether serialization omits the field
func (f *fieldMeta) skipOnSerialize(fv reflect.Value, parent reflect.Value) bool {
	if f.serdeSkip {
		return true
	}
	if f.skipNone && f.isNone(fv, parent) {
		return true
	}
	if f.skipNull && (fv.Kind() == reflect.Interface && fv.IsNil()) {
		return true
	}
	if f.skipEmpty {
		switch fv.Kind() {
		case reflect.Slice, reflect.Map, reflect.String, reflect.Array:
			return fv.Len() == 0
		}
	}
	return false
}
