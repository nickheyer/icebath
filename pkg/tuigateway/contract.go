package tuigateway

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Route a contract entry travels
type Kind string

const (
	// Client method the gateway's method table serves
	KindMethod Kind = "method"
	// Client method the session authority serves
	KindCanonical Kind = "canonical"
	// Question the gateway asks the client
	KindRequest Kind = "request"
	// Notification the gateway sends
	KindEvent Kind = "event"
)

// One declared method, request or event with the Go types of its params and of its result or payload
type Entry struct {
	Kind   Kind
	Name   string
	Params reflect.Type
	Result reflect.Type
}

var (
	entriesMu sync.RWMutex
	entries   = map[Kind]map[string]*Entry{}
)

// Records an entry, panicking when its route already declares the name
func declare(kind Kind, name string, params, result reflect.Type) *Entry {
	entriesMu.Lock()
	defer entriesMu.Unlock()
	table := entries[kind]
	if table == nil {
		table = map[string]*Entry{}
		entries[kind] = table
	}
	if _, dup := table[name]; dup {
		panic("contract declared twice: " + string(kind) + " " + name)
	}
	entry := &Entry{Kind: kind, Name: name, Params: params, Result: result}
	table[name] = entry
	return entry
}

// Go type of T
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// Entry of a route by name, nil when the route declares none
func Lookup(kind Kind, name string) *Entry {
	entriesMu.RLock()
	defer entriesMu.RUnlock()
	return entries[kind][name]
}

// Entries of a route sorted by name
func Entries(kind Kind) []*Entry {
	entriesMu.RLock()
	out := make([]*Entry, 0, len(entries[kind]))
	for _, e := range entries[kind] {
		out = append(out, e)
	}
	entriesMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Wire names of the top level fields of the result or payload, in declaration order
func (e *Entry) ResultNames() []string { return fieldNames(e.Result) }

// Whether the params declare the named key
func (e *Entry) TakesParam(name string) bool {
	_, found := wireFields(e.Params)[name]
	return found && e.Params != nil
}

// Wire names of the top level fields of a struct type, in declaration order
func fieldNames(t reflect.Type) []string {
	if t == nil {
		return nil
	}
	fields := wireFields(t)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return fields[names[i]].order < fields[names[j]].order })
	return names
}

// Client method whose params are P and whose result is R
type Method[P, R any] struct{ entry *Entry }

// Declares a method the gateway's method table serves
func NewMethod[P, R any](name string) Method[P, R] {
	return Method[P, R]{declare(KindMethod, name, typeOf[P](), typeOf[R]())}
}

// Declares a method the session authority serves
func NewCanonicalMethod[P, R any](name string) Method[P, R] {
	return Method[P, R]{declare(KindCanonical, name, typeOf[P](), typeOf[R]())}
}

// Wire name of the method
func (m Method[P, R]) Name() string { return m.entry.Name }

// Declared entry of the method
func (m Method[P, R]) Entry() *Entry { return m.entry }

// Params a client sent, refused with the field named when they do not fit
func (m Method[P, R]) Params(raw any) (P, error) {
	var p P
	if err := Strict(paramsOrEmpty(raw), &p); err != nil {
		return p, &ParamsRefusal{Method: m.entry.Name, Fit: err}
	}
	return p, nil
}

// Result an answer carries, unknown and missing keys tolerated and a wrong type refused
func (m Method[P, R]) Result(raw any) (R, error) {
	var r R
	return r, Loose(raw, &r)
}

// Question the gateway asks a client, its params P and the answer R
type Request[P, R any] struct{ entry *Entry }

// Declares a server to client request
func NewRequest[P, R any](name string) Request[P, R] {
	return Request[P, R]{declare(KindRequest, name, typeOf[P](), typeOf[R]())}
}

// Wire name of the request
func (q Request[P, R]) Name() string { return q.entry.Name }

// Params the client reads, unknown and missing keys tolerated
func (q Request[P, R]) Params(raw any) (P, error) {
	var p P
	return p, Loose(paramsOrEmpty(raw), &p)
}

// Answer the gateway reads, refused with the field named when it does not fit
func (q Request[P, R]) Answer(raw any) (R, error) {
	var r R
	if err := Strict(raw, &r); err != nil {
		return r, &ParamsRefusal{Method: q.entry.Name, Fit: err}
	}
	return r, nil
}

// Notification whose payload is T
type Event[T any] struct{ entry *Entry }

// Payload of an event that carries none
type NoPayload struct{}

// Declares an event
func NewEvent[T any](name string) Event[T] {
	return Event[T]{declare(KindEvent, name, nil, typeOf[T]())}
}

// Wire name of the event
func (ev Event[T]) Name() string { return ev.entry.Name }

// Payload a client reads, unknown and missing keys tolerated
func (ev Event[T]) Payload(raw any) (T, error) {
	var t T
	return t, Loose(raw, &t)
}

// Wire payload of a typed value
func (ev Event[T]) Wire(payload T) map[string]any {
	plain, _ := Plain(payload).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

// Params that do not fit their method, naming the method and the field
type ParamsRefusal struct {
	Method string
	Fit    error
}

func (e *ParamsRefusal) Error() string {
	text := "invalid params for " + e.Method + ": " + e.Fit.Error()
	if e.Unknown() {
		text += " — the client and the Icebath backend are out of sync (different versions); run `icebath update` and restart both"
	}
	return text
}

func (e *ParamsRefusal) Unwrap() error { return e.Fit }

// Field paths the refusal names
func (e *ParamsRefusal) Fields() []string {
	if fit, isFit := e.Fit.(*FitError); isFit {
		return []string{fit.Field}
	}
	return []string{"params"}
}

// Whether the refusal is a key the method does not declare, the mark of a client of another version
func (e *ParamsRefusal) Unknown() bool {
	fit, isFit := e.Fit.(*FitError)
	return isFit && fit.Problem == problemUnknown
}

// Value that does not fit its contract type, naming the field
type FitError struct {
	Field   string
	Problem string
}

func (e *FitError) Error() string {
	field := e.Field
	if field == "" {
		field = "params"
	}
	return field + ": " + e.Problem
}

const (
	problemUnknown  = "unknown field"
	problemRequired = "field required"
)

// Field path of a key under a parent path
func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// Field path of a list index under a parent path
func indexPath(parent string, i int) string { return fmt.Sprintf("%s[%d]", parent, i) }

// Text of a typed enum's allowed values
func enumText(values []string) string { return strings.Join(values, ", ") }
