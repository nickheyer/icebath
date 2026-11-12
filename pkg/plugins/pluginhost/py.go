package pluginhost

import (
	"errors"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Ref is an owned Python object reference
type Ref = *pyhost.Ref

// A Python failure carried up the Go stack to the nearest catch
type thrown struct{ err error }

// Stops the current operation with err, recovered by catch at the operation boundary
func throw(err error) { panic(thrown{err}) }

// Recovers a thrown failure into *err, other panics keep unwinding
func catch(err *error) {
	if r := recover(); r != nil {
		t, ok := r.(thrown)
		if !ok {
			panic(r)
		}
		*err = t.err
	}
}

// Python operations on the attached thread state, each failure thrown to the operation boundary
type py struct {
	g     *pyhost.GIL
	scope *[]Ref
}

func (p py) own(o uintptr, err error) Ref {
	if err != nil {
		throw(err)
	}
	return p.track(p.g.Own(o))
}

// Records a reference the operation scope releases when it ends
func (p py) track(r Ref) Ref {
	if p.scope != nil {
		*p.scope = append(*p.scope, r)
	}
	return r
}

// Reference that outlives the operation scope, for tables and helper objects the runtime keeps
func (p py) keep(r Ref) Ref { return p.g.NewRef(r.Ptr()) }

func (p py) imp(name string) Ref { return p.own(p.g.Import(name)) }

func (p py) get(o Ref, name string) Ref { return p.own(p.g.Attr(o.Ptr(), name)) }

// Attribute or nil when lookup fails, the getattr with a None default plugins rely on
func (p py) getOr(o Ref, name string) Ref {
	v, err := p.g.Attr(o.Ptr(), name)
	if err != nil {
		return nil
	}
	return p.track(p.g.Own(v))
}

func (p py) has(o Ref, name string) bool { return p.g.HasAttr(o.Ptr(), name) }

func (p py) set(o Ref, name string, value any) {
	if err := p.g.SetAttr(o.Ptr(), name, value); err != nil {
		throw(err)
	}
}

func (p py) call(fn Ref, args ...any) Ref { return p.own(p.g.Call(fn.Ptr(), args, nil)) }

func (p py) callKw(fn Ref, args []any, kw map[string]any) Ref {
	return p.own(p.g.Call(fn.Ptr(), args, kw))
}

func (p py) m(o Ref, name string, args ...any) Ref {
	return p.own(p.g.CallMethod(o.Ptr(), name, args, nil))
}

func (p py) mKw(o Ref, name string, args []any, kw map[string]any) Ref {
	return p.own(p.g.CallMethod(o.Ptr(), name, args, kw))
}

// Attribute of an imported module
func (p py) attr(module, name string) Ref { return p.get(p.imp(module), name) }

func (p py) val(o Ref) any {
	v, err := p.g.FromPython(o.Ptr())
	if err != nil {
		throw(err)
	}
	return v
}

func (p py) str(o Ref) string {
	s, err := p.g.Str(o.Ptr())
	if err != nil {
		throw(err)
	}
	return s
}

func (p py) repr(o Ref) string {
	s, err := p.g.Repr(o.Ptr())
	if err != nil {
		throw(err)
	}
	return s
}

func (p py) truthy(o Ref) bool {
	b, err := p.g.Truthy(o.Ptr())
	if err != nil {
		throw(err)
	}
	return b
}

func (p py) isNone(o Ref) bool { return o == nil || p.g.IsNone(o.Ptr()) }

func (p py) callable(o Ref) bool { return o != nil && p.g.Callable(o.Ptr()) }

func (p py) isinstance(o, cls Ref) bool {
	ok, err := p.g.IsInstance(o.Ptr(), cls.Ptr())
	if err != nil {
		throw(err)
	}
	return ok
}

func (p py) issubclass(o, cls Ref) bool {
	ok, err := p.g.IsSubclass(o.Ptr(), cls.Ptr())
	if err != nil {
		throw(err)
	}
	return ok
}

// Whether o is a class object
func (p py) isType(o Ref) bool { return p.isinstance(o, p.attr("builtins", "type")) }

// Python object for a Go value, a new owned reference
func (p py) obj(v any) Ref { return p.own(p.g.ToPython(v)) }

func (p py) typeOf(o Ref) Ref { return p.call(p.attr("builtins", "type"), o) }

// Short class name of an object, type(o).__name__
func (p py) typeName(o Ref) string { return p.str(p.get(p.typeOf(o), "__name__")) }

// Each item of an iterable as an owned reference
func (p py) items(o Ref) []Ref {
	var out []Ref
	err := p.g.Iterate(o.Ptr(), func(item uintptr) error {
		out = append(out, p.track(p.g.NewRef(item)))
		return nil
	})
	if err != nil {
		throw(err)
	}
	return out
}

// Sorted dir() names of an object
func (p py) dir(o Ref) []string {
	var out []string
	for _, n := range p.items(p.call(p.attr("builtins", "dir"), o)) {
		out = append(out, p.str(n))
	}
	return out
}

func (p py) is(a, b Ref) bool { return a != nil && b != nil && a.Ptr() == b.Ptr() }

// Runs fn, returning the Python failure it threw when that failure is one of the named exception classes
func (p py) attempt(fn func(), names ...string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t, ok := r.(thrown)
			if !ok {
				panic(r)
			}
			var pe *pyhost.PyError
			if errors.As(t.err, &pe) && pe.Matches(p.g, names...) {
				err = t.err
				return
			}
			panic(r)
		}
	}()
	fn()
	return nil
}

// Item value of a sequence or mapping, o[key]
func (p py) item(o Ref, key any) Ref { return p.m(o, "__getitem__", key) }

// Builds a native function whose body may throw, failures becoming the raised exception
func (p py) fn(name string, body func(p py, args []any, kw map[string]any) any) Ref {
	return p.own(p.g.NewRawFunc(name, func(g *pyhost.GIL, args, kwargs uintptr) (out uintptr, err error) {
		var scope []Ref
		defer func() {
			for _, r := range scope {
				r.Release()
			}
			g.Drain()
		}()
		defer catch(&err)
		q := py{g: g, scope: &scope}
		return g.ToPython(body(q, q.args(args), q.kwargs(kwargs)))
	}))
}

// Runs an operation body, releasing its references at the end and returning a thrown failure as its error
func run(g *pyhost.GIL, body func(p py)) (err error) {
	var scope []Ref
	defer func() {
		for _, r := range scope {
			r.Release()
		}
		g.Drain()
	}()
	defer catch(&err)
	body(py{g, &scope})
	return nil
}

// Runs a body whose references live on, for the setup of objects the runtime keeps
func runKeep(g *pyhost.GIL, body func(p py)) (err error) {
	defer catch(&err)
	body(py{g: g})
	return nil
}

// Python str() of a decoded wire value
func pyStr(v any) string { return config.PyStr(v) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Argument tuple as Go values, objects kept as scoped references so the scope releases them
func (p py) args(tuple uintptr) []any {
	out := []any{}
	for _, item := range p.items(p.track(p.g.NewRef(tuple))) {
		out = append(out, p.arg(item))
	}
	return out
}

// Keyword dict as Go values with objects kept as scoped references, nil when the call passed none
func (p py) kwargs(dict uintptr) map[string]any {
	if dict == 0 {
		return nil
	}
	out := map[string]any{}
	for _, kv := range p.items(p.m(p.track(p.g.NewRef(dict)), "items")) {
		out[p.str(p.item(kv, 0))] = p.arg(p.item(kv, 1))
	}
	return out
}

func (p py) arg(item Ref) any {
	if p.g.KindOf(item.Ptr()) == pyhost.KindOther {
		return item
	}
	return p.val(item)
}

// Python str() of a Go value or an object reference
func (p py) text(v any) string {
	if r, ok := v.(Ref); ok {
		return p.str(r)
	}
	return pyStr(v)
}
