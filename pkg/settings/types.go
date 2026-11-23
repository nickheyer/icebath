package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Pointer to a bound, for the Min and Max fields of a literal
func at[T int | float64 | time.Duration](n T) *T { return &n }

// Words a boolean setting accepts beside true and false
var boolWords = map[string]bool{"1": true, "on": true, "true": true, "yes": true, "y": true, "enable": true, "enabled": true,
	"0": false, "off": false, "false": false, "no": false, "n": false, "disable": false, "disabled": false}

// Lower stripped text of a scalar
func word(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.ToLower(strings.TrimSpace(t))
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
}

// Whether a written value means unset: nothing, empty text or the way YAML spells no value
func blank(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.TrimSpace(s) {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

// Whether a stored value is empty text, which a setting that may be unset reads as no value
func emptyText(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// On or off setting
type Bool struct {
	Meta
	Default bool
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (b *Bool) Kind() Kind { return KindBool }

func (b *Bool) DefaultValue() any {
	if b.Null != "" {
		return nil
	}
	return b.Default
}

func (b *Bool) Coerce(v any) (any, error) {
	if on, ok := v.(bool); ok {
		return on, nil
	}
	if on, ok := boolWords[word(v)]; ok {
		return on, nil
	}
	return nil, invalid(b.Key, "takes on or off")
}

func (b *Bool) read(v any) (any, bool) {
	if v == nil || (b.Null != "" && emptyText(v)) {
		return nil, b.Null != ""
	}
	if on, ok := boolWords[word(v)]; ok {
		return on, true
	}
	return config.AsBool(v)
}

// Value in effect for ctx
func (b *Bool) Get(ctx context.Context) bool {
	if on, ok := resolveTyped(ctx, b).(bool); ok {
		return on
	}
	return b.Default
}

// Whole number setting with optional bounds and a unit
type Int struct {
	Meta
	Default int
	Min     *int
	Max     *int
	// Unit shown after the number, such as tokens or MB
	Unit string
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (n *Int) Kind() Kind { return KindInt }

func (n *Int) DefaultValue() any {
	if n.Null != "" {
		return nil
	}
	return n.Default
}

func (n *Int) Coerce(v any) (any, error) {
	if blank(v) && n.Null != "" {
		return nil, nil
	}
	i, ok := config.AsInt(v)
	if !ok {
		return nil, invalid(n.Key, "takes a whole number")
	}
	if n.Min != nil && i < *n.Min {
		return nil, invalid(n.Key, "must be at least %d", *n.Min)
	}
	if n.Max != nil && i > *n.Max {
		return nil, invalid(n.Key, "must be at most %d", *n.Max)
	}
	return i, nil
}

func (n *Int) read(v any) (any, bool) {
	if v == nil || (n.Null != "" && emptyText(v)) {
		return nil, n.Null != ""
	}
	return config.AsInt(v)
}

// Value in effect for ctx, the default when unset
func (n *Int) Get(ctx context.Context) int {
	if i, ok := resolveTyped(ctx, n).(int); ok {
		return i
	}
	return n.Default
}

// Value in effect for ctx and whether it is set at all
func (n *Int) Lookup(ctx context.Context) (int, bool) {
	i, ok := resolveTyped(ctx, n).(int)
	return i, ok
}

// Decimal number setting with optional bounds and a unit
type Float struct {
	Meta
	Default float64
	Min     *float64
	Max     *float64
	Unit    string
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (f *Float) Kind() Kind { return KindFloat }

func (f *Float) DefaultValue() any {
	if f.Null != "" {
		return nil
	}
	return f.Default
}

func (f *Float) Coerce(v any) (any, error) {
	if blank(v) && f.Null != "" {
		return nil, nil
	}
	x, ok := config.AsFloat(v)
	if !ok || math.IsNaN(x) || math.IsInf(x, 0) {
		return nil, invalid(f.Key, "takes a number")
	}
	if f.Min != nil && x < *f.Min {
		return nil, invalid(f.Key, "must be at least %s", strconv.FormatFloat(*f.Min, 'g', -1, 64))
	}
	if f.Max != nil && x > *f.Max {
		return nil, invalid(f.Key, "must be at most %s", strconv.FormatFloat(*f.Max, 'g', -1, 64))
	}
	return x, nil
}

func (f *Float) read(v any) (any, bool) {
	if v == nil || (f.Null != "" && emptyText(v)) {
		return nil, f.Null != ""
	}
	return config.AsFloat(v)
}

// Value in effect for ctx, the default when unset
func (f *Float) Get(ctx context.Context) float64 {
	if x, ok := resolveTyped(ctx, f).(float64); ok {
		return x
	}
	return f.Default
}

// Value in effect for ctx and whether it is set at all
func (f *Float) Lookup(ctx context.Context) (float64, bool) {
	x, ok := resolveTyped(ctx, f).(float64)
	return x, ok
}

// Length of time stored in config.yaml as a number of Unit
type Duration struct {
	Meta
	Default time.Duration
	// Unit the stored number counts, seconds when zero
	Unit time.Duration
	Min  *time.Duration
	Max  *time.Duration
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (d *Duration) Kind() Kind { return KindDuration }

// Unit the stored number counts
func (d *Duration) unit() time.Duration {
	if d.Unit <= 0 {
		return time.Second
	}
	return d.Unit
}

// Stored number of a duration, whole when it divides evenly
func (d *Duration) stored(v time.Duration) any {
	if v%d.unit() == 0 {
		return int(v / d.unit())
	}
	return float64(v) / float64(d.unit())
}

func (d *Duration) DefaultValue() any {
	if d.Null != "" {
		return nil
	}
	return d.stored(d.Default)
}

// Duration a stored or written value spells
func (d *Duration) parse(v any) (time.Duration, bool) {
	if x, ok := config.AsFloat(v); ok {
		if math.IsNaN(x) {
			return 0, false
		}
		if scaled := x * float64(d.unit()); math.Abs(scaled) < math.MaxInt64 {
			return time.Duration(scaled), true
		}
		if x < 0 {
			return time.Duration(math.MinInt64), true
		}
		return time.Duration(math.MaxInt64), true
	}
	if s, ok := v.(string); ok {
		if parsed, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func (d *Duration) Coerce(v any) (any, error) {
	if blank(v) && d.Null != "" {
		return nil, nil
	}
	parsed, ok := d.parse(v)
	if !ok {
		return nil, invalid(d.Key, "takes a length of time such as 30s or 5m")
	}
	if x, isNumber := config.AsFloat(v); isNumber && math.Abs(x*float64(d.unit())) >= math.MaxInt64 {
		return nil, invalid(d.Key, "is too long, the most it takes is %d days", int(time.Duration(math.MaxInt64)/(24*time.Hour)))
	}
	if d.Min != nil && parsed < *d.Min {
		return nil, invalid(d.Key, "must be at least %s", d.text(*d.Min))
	}
	if d.Max != nil && parsed > *d.Max {
		return nil, invalid(d.Key, "must be at most %s", d.text(*d.Max))
	}
	return d.stored(parsed), nil
}

func (d *Duration) read(v any) (any, bool) {
	if v == nil || (d.Null != "" && emptyText(v)) {
		return nil, d.Null != ""
	}
	parsed, ok := d.parse(v)
	if !ok {
		return nil, false
	}
	return parsed, true
}

// Length of time said as the number the setting stores with its unit
func (d *Duration) text(v time.Duration) string {
	return withUnit(d.stored(v), unitName(d.unit()))
}

// Value in effect for ctx and whether it is set at all
func (d *Duration) Lookup(ctx context.Context) (time.Duration, bool) {
	v, ok := resolveTyped(ctx, d).(time.Duration)
	return v, ok
}

// Value in effect for ctx, the default when unset
func (d *Duration) Get(ctx context.Context) time.Duration {
	if v, ok := resolveTyped(ctx, d).(time.Duration); ok {
		return v
	}
	return d.Default
}

// One of a fixed set of words
type Enum struct {
	Meta
	Default string
	Values  []Choice
	// Pair a bare toggle flips between, empty when the setting does not toggle
	Off, On string
	// Accepts words outside Values, Validate deciding
	Open bool
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (e *Enum) Kind() Kind { return KindEnum }

func (e *Enum) DefaultValue() any {
	if e.Null != "" {
		return nil
	}
	return e.Default
}

// Canonical spelling of the value a word names, letter case ignored
func (e *Enum) choice(w string) (string, bool) {
	for _, c := range e.Values {
		if strings.EqualFold(c.Value, w) {
			return c.Value, true
		}
	}
	return "", false
}

// Words of the values in order
func (e *Enum) Words() []string {
	out := make([]string, len(e.Values))
	for i, c := range e.Values {
		out[i] = c.Value
	}
	return out
}

// Words YAML reads as a boolean, by the boolean they become
var yamlBoolWords = map[bool][]string{true: {"on", "true", "yes"}, false: {"off", "false", "no"}}

// Value a written or stored word names, false when none; a boolean stands for the toggle pair or the word YAML read it from
func (e *Enum) match(v any) (string, bool) {
	if on, isBool := v.(bool); isBool {
		if e.On != "" {
			if on {
				return e.On, true
			}
			return e.Off, true
		}
		for _, w := range yamlBoolWords[on] {
			if c, ok := e.choice(w); ok {
				return c, true
			}
		}
		return "", false
	}
	text := ""
	switch t := v.(type) {
	case nil:
	case string:
		text = strings.TrimSpace(t)
	default:
		text = strings.TrimSpace(fmt.Sprint(t))
	}
	if c, ok := e.choice(text); ok {
		return c, true
	}
	if e.Open && text != "" {
		return text, true
	}
	return "", false
}

func (e *Enum) Coerce(v any) (any, error) {
	if blank(v) && e.Null != "" {
		return nil, nil
	}
	if w, ok := e.match(v); ok {
		return w, nil
	}
	return nil, invalid(e.Key, "takes one of %s", strings.Join(e.Words(), ", "))
}

func (e *Enum) read(v any) (any, bool) {
	if v == nil || (e.Null != "" && emptyText(v)) {
		return nil, e.Null != ""
	}
	return e.match(v)
}

// Value in effect for ctx, the default when unset
func (e *Enum) Get(ctx context.Context) string {
	if w, ok := resolveTyped(ctx, e).(string); ok {
		return w
	}
	return e.Default
}

// Value a bare toggle moves a setting to from its value in effect, nil when the setting does not toggle
func Flipped(ctx context.Context, e Entry) any {
	switch t := e.(type) {
	case *Bool:
		return !t.Get(ctx)
	case *Enum:
		if t.On == "" {
			return nil
		}
		if t.Get(ctx) == t.Off {
			return t.On
		}
		return t.Off
	}
	return nil
}

// Free text setting
type String struct {
	Meta
	Default string
	// Holds a credential, so it is shown only as set or not set
	Masked bool
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (s *String) Kind() Kind { return KindString }

func (s *String) DefaultValue() any {
	if s.Null != "" {
		return nil
	}
	return s.Default
}

func (s *String) Coerce(v any) (any, error) {
	if v == nil && s.Null != "" {
		return nil, nil
	}
	return scalarText(s.Key, v)
}

func (s *String) read(v any) (any, bool) {
	if v == nil {
		return nil, s.Null != ""
	}
	return readText(v)
}

// Value in effect for ctx, the default when unset
func (s *String) Get(ctx context.Context) string {
	if text, ok := resolveTyped(ctx, s).(string); ok {
		return text
	}
	return s.Default
}

// File or folder location
type Path struct {
	Meta
	Default string
}

func (p *Path) Kind() Kind        { return KindPath }
func (p *Path) DefaultValue() any { return p.Default }

func (p *Path) Coerce(v any) (any, error) { return scalarText(p.Key, v) }

func (p *Path) read(v any) (any, bool) {
	if v == nil {
		return "", true
	}
	return readText(v)
}

// Value in effect for ctx
func (p *Path) Get(ctx context.Context) string {
	if text, ok := resolveTyped(ctx, p).(string); ok {
		return text
	}
	return p.Default
}

// Credential kept in the profile .env under Env and only ever shown as set or not set
type Secret struct {
	Meta
}

func (s *Secret) Kind() Kind        { return KindSecret }
func (s *Secret) DefaultValue() any { return nil }

func (s *Secret) Coerce(v any) (any, error) {
	text, err := scalarText(s.Key, v)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(text, "\r\n") {
		return nil, invalid(s.Key, "must be one line")
	}
	return text, nil
}

func (s *Secret) read(v any) (any, bool) {
	if v == nil {
		return nil, true
	}
	return readText(v)
}

// Value in effect for ctx, empty when not set
func (s *Secret) Get(ctx context.Context) string {
	text, _ := resolveTyped(ctx, s).(string)
	return text
}

// Ordered list of values
type List struct {
	Meta
	Default []any
	// Kind of each item, string when empty
	Of Kind
	// Schema each item follows, nil for items no schema checks
	Item Entry
	// Text given in place of a list is one item, such as a pattern that may hold commas, instead of items split at commas
	Whole bool
	// Unset by default when not empty, the text saying what unset means
	Null string
}

func (l *List) Kind() Kind { return KindList }

func (l *List) DefaultValue() any {
	if l.Null != "" {
		return nil
	}
	return config.Clone(append([]any{}, l.Default...))
}

// Items checked by the item schema, each refusal naming the position of its item
func (l *List) coerceItems(items []any) (any, error) {
	if l.Item == nil {
		return config.Clone(items), nil
	}
	out := make([]any, len(items))
	for i, item := range items {
		coerced, err := coercePart(l.Item, item)
		if err != nil {
			return nil, invalid(l.Key+"."+strconv.Itoa(i), "%s", reasonOf(err))
		}
		out[i] = coerced
	}
	return out, nil
}

// Items a written or stored value spells: a list as it is and, for a list of text, a JSON list decoded, other text split at commas or kept whole, and a lone switch or number as its one item
func (l *List) items(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	}
	if l.Of != "" && l.Of != KindString {
		return nil, false
	}
	switch t := v.(type) {
	case string:
		text := strings.TrimSpace(t)
		if text == "" {
			return []any{}, true
		}
		if strings.HasPrefix(text, "[") {
			var decoded []any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				return decoded, true
			}
		}
		if l.Whole {
			return []any{text}, true
		}
		out := []any{}
		for _, s := range config.AsStrings(text) {
			out = append(out, s)
		}
		return out, true
	case bool, int, int64, float64:
		return []any{fmt.Sprint(t)}, true
	}
	return nil, false
}

func (l *List) Coerce(v any) (any, error) {
	if v == nil && l.Null != "" {
		return nil, nil
	}
	items, ok := l.items(v)
	if !ok {
		return nil, invalid(l.Key, "takes a list")
	}
	return l.coerceItems(items)
}

func (l *List) read(v any) (any, bool) {
	if v == nil {
		return nil, l.Null != ""
	}
	if l.Null != "" && emptyText(v) {
		return nil, true
	}
	items, ok := l.items(v)
	if !ok {
		return nil, false
	}
	if l.Item == nil {
		return items, true
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		if typed, ok := readPart(l.Item, item); ok {
			out = append(out, typed)
		}
	}
	return out, true
}

// Items in effect for ctx
func (l *List) Get(ctx context.Context) []any {
	if items, ok := resolveTyped(ctx, l).([]any); ok {
		return items
	}
	return l.Default
}

// Items in effect for ctx as text, items that are not scalars skipped
func (l *List) Strings(ctx context.Context) []string {
	return config.AsStrings(l.Get(ctx))
}

// Mapping of names to values, each value optionally following an entry schema
type Map struct {
	Meta
	Default map[string]any
	// Schema of each value, nil for free form values
	Value Entry
}

func (m *Map) Kind() Kind { return KindMap }

func (m *Map) DefaultValue() any {
	if m.Default == nil {
		return map[string]any{}
	}
	return config.Clone(m.Default)
}

func (m *Map) Coerce(v any) (any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	in := config.AsMap(v)
	if in == nil {
		return nil, invalid(m.Key, "takes a mapping")
	}
	out := make(map[string]any, len(in))
	for name, item := range in {
		if m.Value == nil {
			out[name] = config.Clone(item)
			continue
		}
		coerced, err := coercePart(m.Value, item)
		if err != nil {
			return nil, invalid(m.Key+"."+name, "%s", reasonOf(err))
		}
		out[name] = coerced
	}
	return out, nil
}

func (m *Map) read(v any) (any, bool) {
	if v == nil {
		return map[string]any{}, true
	}
	in := config.AsMap(v)
	if in == nil {
		return nil, false
	}
	if m.Value == nil {
		return in, true
	}
	out := make(map[string]any, len(in))
	for name, item := range in {
		if typed, ok := readPart(m.Value, item); ok {
			out[name] = typed
		}
	}
	return out, true
}

// Sets one entry of the map in its default scope through Set, any name allowed
func (m *Map) SetEntry(ctx context.Context, name string, value any) (Change, error) {
	return Set(ctx, m.EntryKey(name), value, 0)
}

// Removes one entry of the map from its default scope through Reset
func (m *Map) DeleteEntry(ctx context.Context, name string) (Change, error) {
	return Reset(ctx, m.EntryKey(name), 0)
}

// Mapping in effect for ctx
func (m *Map) Get(ctx context.Context) map[string]any {
	if out, ok := resolveTyped(ctx, m).(map[string]any); ok {
		return out
	}
	return map[string]any{}
}

// Mapping of named fields, each field following its own entry whose Key is the field name, a dotted Key naming a field nested inside mappings
type Object struct {
	Meta
	Fields []Entry
}

func (o *Object) Kind() Kind { return KindObject }

// Entry of a field, nil when the object has no such field
func (o *Object) Field(name string) Entry {
	for _, f := range o.Fields {
		if f.Info().Key == name {
			return f
		}
	}
	return nil
}

// Whether fields of the object are nested under a dotted name
func (o *Object) holds(name string) bool {
	for _, f := range o.Fields {
		if strings.HasPrefix(f.Info().Key, name+".") {
			return true
		}
	}
	return false
}

// Field the leading names of a path spell and how many names it takes, nil when they spell none
func (o *Object) fieldAt(names []string) (Entry, int) {
	for n := len(names); n > 0; n-- {
		if slices.ContainsFunc(names[:n], func(name string) bool { return strings.Contains(name, ".") }) {
			continue
		}
		if f := o.Field(strings.Join(names[:n], ".")); f != nil {
			return f, n
		}
	}
	return nil, 0
}

// One value of a stored mapping in the object's terms: the dotted name it sits at, its field, nil when no field takes it, and the value
type objectPart struct {
	name  string
	field Entry
	value any
}

// Values of a stored mapping in name order, a mapping walked into where fields are nested under its name
func (o *Object) parts(prefix string, in map[string]any) []objectPart {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []objectPart
	for _, name := range names {
		path, item := prefix+name, in[name]
		if f := o.Field(path); f != nil {
			out = append(out, objectPart{name: path, field: f, value: item})
			continue
		}
		if o.holds(path) {
			if item == nil {
				continue
			}
			if nested := config.AsMap(item); nested != nil {
				out = append(out, o.parts(path+".", nested)...)
				continue
			}
		}
		out = append(out, objectPart{name: path, value: item})
	}
	return out
}

// Stores a value at a dotted name of a mapping, making the mappings on the way
func putNested(m map[string]any, name string, v any) {
	names := strings.Split(name, ".")
	for _, part := range names[:len(names)-1] {
		next, ok := m[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[part] = next
		}
		m = next
	}
	m[names[len(names)-1]] = v
}

func (o *Object) DefaultValue() any {
	out := map[string]any{}
	for _, f := range o.Fields {
		if v := f.DefaultValue(); v != nil {
			putNested(out, f.Info().Key, v)
		}
	}
	return out
}

func (o *Object) Coerce(v any) (any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	in := config.AsMap(v)
	if in == nil {
		return nil, invalid(o.Key, "takes a mapping")
	}
	out := make(map[string]any, len(in))
	for _, part := range o.parts("", in) {
		if part.field == nil {
			if o.holds(part.name) {
				return nil, invalid(o.Key+"."+part.name, "takes a mapping")
			}
			return nil, invalid(o.Key+"."+part.name, "is not a field here; fields: %s", strings.Join(o.fieldNames(), ", "))
		}
		coerced, err := coercePart(part.field, part.value)
		if err != nil {
			return nil, invalid(o.Key+"."+part.name, "%s", reasonOf(err))
		}
		if coerced != nil {
			putNested(out, part.name, coerced)
		}
	}
	return out, nil
}

// Names of the fields in order
func (o *Object) fieldNames() []string {
	out := make([]string, len(o.Fields))
	for i, f := range o.Fields {
		out[i] = f.Info().Key
	}
	return out
}

func (o *Object) read(v any) (any, bool) {
	if v == nil {
		return map[string]any{}, true
	}
	in := config.AsMap(v)
	if in == nil {
		return nil, false
	}
	out := make(map[string]any, len(in))
	for _, part := range o.parts("", in) {
		if part.field == nil {
			continue
		}
		if typed, ok := readPart(part.field, part.value); ok {
			putNested(out, part.name, typed)
		}
	}
	return out, true
}

// Stored form of a value an entry takes, its kind's check then the entry's own check applied
func coercePart(e Entry, v any) (any, error) {
	coerced, err := e.Coerce(v)
	if err != nil {
		return nil, err
	}
	if check := e.Info().Validate; check != nil && coerced != nil {
		if err := check(coerced); err != nil {
			return nil, invalid(e.Info().Key, "%s", err.Error())
		}
	}
	return coerced, nil
}

// Whether a stored value passes the entry's own check; a derived setting's check guards only what may be written
func passes(e Entry, v any) bool {
	m := e.Info()
	if m.Validate == nil || m.Derive != nil || v == nil {
		return true
	}
	coerced, err := e.Coerce(v)
	return err != nil || coerced == nil || m.Validate(coerced) == nil
}

// Typed form of a stored value, false when the entry's kind does not take it or its own check refuses it
func readPart(e Entry, v any) (any, bool) {
	typed, ok := e.read(v)
	if !ok || !passes(e, v) {
		return nil, false
	}
	return typed, true
}

// Free value under a map whose entries follow no schema
type anyValue struct {
	Meta
}

func (a *anyValue) Kind() Kind                { return KindAny }
func (a *anyValue) DefaultValue() any         { return nil }
func (a *anyValue) Coerce(v any) (any, error) { return config.Clone(v), nil }
func (a *anyValue) read(v any) (any, bool)    { return v, true }

// Reason text of a value refusal
func reasonOf(err error) string {
	if ve, ok := err.(*ValueError); ok {
		return ve.Reason
	}
	return err.Error()
}

// Text of a scalar written value
func scalarText(key string, v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(t), nil
	case bool, int, int64, float64:
		return fmt.Sprint(t), nil
	}
	return "", invalid(key, "takes text")
}

// Text of a stored scalar
func readText(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool, int, int64, float64:
		return fmt.Sprint(t), true
	}
	return nil, false
}
