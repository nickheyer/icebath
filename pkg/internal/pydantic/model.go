package pydantic

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// What a model does with input keys no field declares, the extra setting of ConfigDict
type Extra int

const (
	ExtraIgnore Extra = iota
	ExtraForbid
	ExtraAllow
)

// The model_config keys the contracts set
type Config struct {
	Extra          Extra
	PopulateByName bool
	FromAttributes bool
}

// One declared field with its annotation, default and alias
type Field struct {
	Name     string
	Type     Type
	alias    string
	exclude  bool
	required bool
	def      any
	factory  func() any
}

// A field with no default
func Req(name string, t Type) Field { return Field{Name: name, Type: t, required: true} }

// A field with a plain default value, a Go int standing for a Python int
func Def(name string, t Type, value any) Field {
	return Field{Name: name, Type: t, def: pyInt(value)}
}

// A field whose default comes from default_factory
func Fac(name string, t Type, factory func() any) Field {
	return Field{Name: name, Type: t, factory: factory}
}

// Field(alias=name), the key the field validates from and dumps to by alias
func (f Field) Alias(name string) Field {
	f.alias = name
	return f
}

// Field(exclude=True), left out of every dump
func (f Field) Exclude() Field {
	f.exclude = true
	return f
}

// The default_factory list
func NewList() any { return []any{} }

// The default_factory dict
func NewDict() any { return pyjson.NewObject() }

// Keys tried in order to find the field in a dict input, the alias first
func (f Field) lookupKeys(byName bool) []string {
	if f.alias == "" {
		return []string{f.Name}
	}
	if byName {
		return []string{f.alias, f.Name}
	}
	return []string{f.alias}
}

// Fresh default value of the field
func (f Field) defaultValue() any {
	if f.factory != nil {
		return f.factory()
	}
	return pyjson.Clone(f.def)
}

// A BaseModel class, its fields in declaration order and the inherited ones first
type Model struct {
	Name   string
	Config Config
	Fields []Field
	Root   Type
	After  func(*Instance) error
}

// Model class with its config and fields
func NewModel(name string, cfg Config, fields ...Field) *Model {
	return &Model{Name: name, Config: cfg, Fields: fields}
}

// Subclass of base, an override keeping the place of the field it replaces and new fields appended
func Extend(base *Model, name string, cfg Config, fields ...Field) *Model {
	all := append([]Field(nil), base.Fields...)
	for _, f := range fields {
		replaced := false
		for i := range all {
			if all[i].Name == f.Name {
				all[i], replaced = f, true
				break
			}
		}
		if !replaced {
			all = append(all, f)
		}
	}
	return &Model{Name: name, Config: cfg, Fields: all, After: base.After}
}

// RootModel class validating its input against the root annotation
func NewRootModel(name string, root Type) *Model {
	return &Model{Name: name, Root: root}
}

// The model with a model_validator mode after, its error becoming a value_error
func (m *Model) WithAfter(fn func(*Instance) error) *Model {
	m.After = fn
	return m
}

func (m *Model) Label() string { return m.Name }

// Field names in declaration order, the model_fields keys
func (m *Model) ModelFields() []string {
	if m.Root != nil {
		return []string{"root"}
	}
	names := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		names[i] = f.Name
	}
	return names
}

// Every problem model_validate finds in the value, nil when it validates
func (m *Model) ModelValidate(value any) *ValidationError {
	_, err := m.Validate(value)
	return err
}

// The validated instance model_validate returns, or the error it raises
func (m *Model) Validate(value any) (*Instance, *ValidationError) {
	st := &state{exactness: exact, fieldsSet: -1}
	out, errs := m.validate(st, value)
	if errs != nil {
		return nil, &ValidationError{Title: m.Name, Issues: errs}
	}
	return out.(*Instance), nil
}

// Reads one key of the input, the dict item or the attribute
type getter func(key string) (any, bool)

// Attribute reader of an object input, an instance field or a struct field by its json name
func attributes(v any) (getter, bool) {
	if inst, ok := v.(*Instance); ok {
		return func(key string) (any, bool) { return inst.lookup(key) }, true
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	return func(key string) (any, bool) {
		for _, sf := range reflect.VisibleFields(rv.Type()) {
			if !sf.IsExported() || sf.Anonymous {
				continue
			}
			if name, _, _ := strings.Cut(sf.Tag.Get("json"), ","); name == key {
				return rv.FieldByIndex(sf.Index).Interface(), true
			}
		}
		return nil, false
	}, true
}

func (m *Model) validate(st *state, v any) (any, []ErrorDetails) {
	if inst, ok := v.(*Instance); ok && inst.model == m {
		st.fieldsSet = inst.setCount
		return inst, nil
	}
	if m.Root != nil {
		root, errs := m.Root.validate(st, v)
		if errs != nil {
			return nil, errs
		}
		return m.after(&Instance{model: m, root: root, setCount: 1}, v)
	}
	var get getter
	var dict *pyjson.Object
	if d, ok := dictOf(v); ok {
		dict = d
		get = func(key string) (any, bool) { return d.Get(key) }
		st.floor(strict)
	} else if g, ok := attributes(v); ok && m.Config.FromAttributes {
		get = g
	} else if m.Config.FromAttributes {
		return nil, fail("model_attributes_type", "Input should be a valid dictionary or object to extract fields from", v)
	} else {
		return nil, fail("model_type", "Input should be a valid dictionary or instance of "+m.Name, v)
	}
	inst := &Instance{model: m, values: pyjson.NewObject()}
	used := map[string]bool{}
	var errs []ErrorDetails
	for _, f := range m.Fields {
		keys := f.lookupKeys(m.Config.PopulateByName)
		key, raw, found := "", any(nil), false
		for _, k := range keys {
			if raw, found = get(k); found {
				key = k
				break
			}
		}
		if !found {
			if f.required {
				errs = append(errs, under(keys[0], fail("missing", "Field required", v))...)
				continue
			}
			inst.values.Set(f.Name, f.defaultValue())
			continue
		}
		used[key] = true
		val, e := f.Type.validate(st, raw)
		if e != nil {
			errs = append(errs, under(key, e)...)
			continue
		}
		inst.values.Set(f.Name, val)
		inst.setCount++
	}
	if dict != nil && m.Config.Extra != ExtraIgnore {
		for _, k := range dict.Keys {
			if used[k] {
				continue
			}
			if m.Config.Extra == ExtraForbid {
				errs = append(errs, under(k, fail("extra_forbidden", "Extra inputs are not permitted", dict.Vals[k]))...)
				continue
			}
			if inst.extra == nil {
				inst.extra = pyjson.NewObject()
			}
			inst.extra.Set(k, dict.Vals[k])
		}
	}
	if errs != nil {
		return nil, errs
	}
	st.fieldsSet = inst.setCount
	return m.after(inst, v)
}

// Runs the after validator, its error a value_error over the whole input
func (m *Model) after(inst *Instance, input any) (any, []ErrorDetails) {
	if m.After != nil {
		if err := m.After(inst); err != nil {
			return nil, fail("value_error", "Value error, "+err.Error(), input)
		}
	}
	return inst, nil
}

// A validated model, field values by name in declaration order then the extra keys
type Instance struct {
	model    *Model
	values   *pyjson.Object
	extra    *pyjson.Object
	root     any
	setCount int
}

// Value of a field or extra key, the root value of a RootModel under root
func (i *Instance) lookup(name string) (any, bool) {
	if i.model.Root != nil {
		return i.root, name == "root"
	}
	if v, ok := i.values.Get(name); ok {
		return v, true
	}
	if i.extra != nil {
		return i.extra.Get(name)
	}
	return nil, false
}

// Attribute value of a field or extra key, panicking on a name the instance lacks
func (i *Instance) Get(name string) any {
	v, ok := i.lookup(name)
	if !ok {
		panic("AttributeError: '" + i.model.Name + "' object has no attribute '" + name + "'")
	}
	return v
}

// Class name and fields the way a pydantic model repr reads
func (i *Instance) PyRepr() string {
	if i.model.Root != nil {
		return i.model.Name + "(root=" + pyrepr.Repr(i.root) + ")"
	}
	parts := []string{}
	for _, k := range i.values.Keys {
		parts = append(parts, k+"="+pyrepr.Repr(i.values.Vals[k]))
	}
	if i.extra != nil {
		for _, k := range i.extra.Keys {
			parts = append(parts, k+"="+pyrepr.Repr(i.extra.Vals[k]))
		}
	}
	return i.model.Name + "(" + strings.Join(parts, ", ") + ")"
}

func (i *Instance) PyTypeName() string { return i.model.Name }

// The model_dump keyword arguments the callers pass
type DumpOptions struct {
	ByAlias     bool
	ExcludeNone bool
}

// model_dump(mode="json") of the instance, a RootModel dumping its root value
func (i *Instance) Dump(o DumpOptions) any {
	if i.model.Root != nil {
		return serialize(i.model.Root, i.root, o)
	}
	out := pyjson.NewObject()
	for _, f := range i.model.Fields {
		v := i.values.Vals[f.Name]
		if f.exclude || (o.ExcludeNone && v == nil) {
			continue
		}
		key := f.Name
		if o.ByAlias && f.alias != "" {
			key = f.alias
		}
		out.Set(key, serialize(f.Type, v, o))
	}
	if i.extra != nil {
		for _, k := range i.extra.Keys {
			if v := i.extra.Vals[k]; !(o.ExcludeNone && v == nil) {
				out.Set(k, dumpValue(v, o))
			}
		}
	}
	return out
}

// model_dump_json of the instance, compact JSON with non ASCII text kept
func (i *Instance) DumpJSON(o DumpOptions) string {
	text, err := pyjson.Dumps(jsonFloats(i.Dump(o)), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return text
}

// Dumped tree with every float written the way pydantic-core serializes JSON, infinities and NaN as null
func jsonFloats(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
		return json.Number(ryuText(x))
	case float32:
		return jsonFloats(float64(x))
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = jsonFloats(item)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys {
			out.Set(k, jsonFloats(x.Vals[k]))
		}
		return out
	}
	return v
}

// Shortest round trip text of a finite float in the layout pydantic-core writes JSON floats
func ryuText(f float64) string {
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(sci, "-") {
		sign, sci = "-", sci[1:]
	}
	mant, expText, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expText)
	if digits == "0" {
		return sign + "0.0"
	}
	length := len(digits)
	kk := exp + 1
	k := kk - length
	switch {
	case 0 <= k && kk <= 16:
		return sign + digits + strings.Repeat("0", k) + ".0"
	case 0 < kk && kk <= 16:
		return sign + digits[:kk] + "." + digits[kk:]
	case -5 < kk && kk <= 0:
		return sign + "0." + strings.Repeat("0", -kk) + digits
	}
	expPart := "e" + strconv.Itoa(kk-1)
	if kk-1 >= 0 {
		expPart = "e+" + strconv.Itoa(kk-1)
	}
	if length == 1 {
		return sign + digits + expPart
	}
	return sign + digits[:1] + "." + digits[1:] + expPart
}

// Dumped form of a value under its declared type, an int a float field holds dumping as a float
func serialize(t Type, v any, o DumpOptions) any {
	switch x := t.(type) {
	case floatType:
		if n, ok := pyrepr.Int(v); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
		if _, ok := floatOf(v); ok {
			return v
		}
	case nullableType:
		if v != nil {
			return serialize(x.inner, v, o)
		}
	case listType:
		if items, ok := v.([]any); ok {
			out := make([]any, len(items))
			for i, item := range items {
				out[i] = serialize(x.item, item, o)
			}
			return out
		}
	case dictType:
		if d, ok := v.(*pyjson.Object); ok {
			out := pyjson.NewObject()
			for _, k := range d.Keys {
				out.Set(k, serialize(x.value, d.Vals[k], o))
			}
			return out
		}
	}
	return dumpValue(v, o)
}

// Dumped form of a value of no declared type, nested instances dumped, containers copied and non finite floats null
func dumpValue(v any, o DumpOptions) any {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return nil
		}
	case *Instance:
		return x.Dump(o)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = dumpValue(item, o)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys {
			out.Set(k, dumpValue(x.Vals[k], o))
		}
		return out
	}
	if d, ok := dictOf(v); ok {
		return dumpValue(d, o)
	}
	if items, _, ok := listOf(v); ok {
		return dumpValue(items, o)
	}
	return v
}

type taggedType struct {
	discriminator string
	tags          []string
	models        []*Model
}

// Field(discriminator=name) over models whose discriminator field is a Literal, one tag per literal value
func Tagged(discriminator string, models ...*Model) Type {
	t := taggedType{discriminator: discriminator}
	for _, m := range models {
		for _, f := range m.Fields {
			if f.Name != discriminator {
				continue
			}
			lit, ok := f.Type.(literalType)
			if !ok {
				panic("discriminator " + discriminator + " of " + m.Name + " is not a Literal")
			}
			for _, v := range lit.values {
				t.tags = append(t.tags, v.(string))
				t.models = append(t.models, m)
			}
		}
	}
	return t
}

func (t taggedType) Label() string {
	names := make([]string, len(t.models))
	for i, m := range t.models {
		names[i] = m.Name
	}
	return "tagged-union[" + strings.Join(names, ",") + "]"
}

func (t taggedType) validate(st *state, v any) (any, []ErrorDetails) {
	var get getter
	if d, ok := dictOf(v); ok {
		get = func(key string) (any, bool) { return d.Get(key) }
	} else if g, ok := attributes(v); ok {
		get = g
	} else {
		return nil, fail("model_attributes_type", "Input should be a valid dictionary or object to extract fields from", v)
	}
	disc := pyrepr.Repr(t.discriminator)
	tag, found := get(t.discriminator)
	if !found {
		return nil, fail("union_tag_not_found", "Unable to extract tag using discriminator "+disc, v)
	}
	if s, ok := tag.(string); ok {
		for i, want := range t.tags {
			if s == want {
				out, errs := t.models[i].validate(st, v)
				if errs != nil {
					return nil, under(want, errs)
				}
				return out, nil
			}
		}
	}
	reprs := make([]string, len(t.tags))
	for i, want := range t.tags {
		reprs[i] = pyrepr.Repr(want)
	}
	return nil, fail("union_tag_invalid", "Input tag '"+pyrepr.Str(tag)+"' found using "+disc+
		" does not match any of the expected tags: "+strings.Join(reprs, ", "), v)
}
