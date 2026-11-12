package harnesshost

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// GIL scope that releases every new reference it handed out when the call returns
type frame struct {
	g     *pyhost.GIL
	owned []uintptr
}

// Keeps a new reference for release, passing an error through untouched
func (f *frame) keep(o uintptr, err error) (uintptr, error) {
	if err != nil {
		return 0, err
	}
	f.owned = append(f.owned, o)
	return o, nil
}

// Drops every reference the frame kept
func (f *frame) release() {
	for i := len(f.owned) - 1; i >= 0; i-- {
		f.g.Decref(f.owned[i])
	}
	f.owned = nil
}

func (f *frame) imp(name string) (uintptr, error) { return f.keep(f.g.Import(name)) }

func (f *frame) attr(o uintptr, name string) (uintptr, error) { return f.keep(f.g.Attr(o, name)) }

func (f *frame) call(callable uintptr, args []any, kwargs map[string]any) (uintptr, error) {
	return f.keep(f.g.Call(callable, args, kwargs))
}

func (f *frame) method(o uintptr, name string, args ...any) (uintptr, error) {
	return f.keep(f.g.CallMethod(o, name, args, nil))
}

// Attribute of a module imported by name
func (f *frame) member(module, name string) (uintptr, error) {
	m, err := f.imp(module)
	if err != nil {
		return 0, err
	}
	return f.attr(m, name)
}

// Reports whether err is a Python exception of one of the named builtin classes
func (f *frame) raised(err error, names ...string) (bool, error) {
	for _, name := range names {
		cls, err2 := f.member("builtins", name)
		if err2 != nil {
			return false, err2
		}
		matched, err2 := f.g.ExceptionMatches(err, cls)
		if err2 != nil || matched {
			return matched, err2
		}
	}
	return false, nil
}

// Python truth value of an object
func (f *frame) truthy(o uintptr) (bool, error) { return f.g.Truthy(o) }

// Result of the binary operator function from the operator module applied to a and b
func (f *frame) op(name string, a, b any) (uintptr, error) {
	fn, err := f.member("operator", name)
	if err != nil {
		return 0, err
	}
	return f.call(fn, []any{a, b}, nil)
}

// Positional and keyword arguments bound to params with defaults the way a def signature binds them
func (f *frame) bind(qualname string, params []string, defaults []any, args, kwargs uintptr) ([]any, error) {
	length, err := f.member("builtins", "len")
	if err != nil {
		return nil, err
	}
	n, err := f.call(length, []any{pyhost.Borrowed(args)}, nil)
	if err != nil {
		return nil, err
	}
	count, err := f.g.FromPython(n)
	if err != nil {
		return nil, err
	}
	given := int(count.(int64))
	if given > len(params) {
		return nil, &pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("%s() takes from 0 to %d positional arguments but %d were given", qualname, len(params), given)}
	}
	bound := make([]any, len(params))
	set := make([]bool, len(params))
	for i := 0; i < given; i++ {
		bound[i], set[i] = pyhost.Borrowed(f.g.TupleItem(args, i)), true
	}
	if kwargs != 0 {
		keys, err := f.g.DictKeys(kwargs)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			index := -1
			for i, name := range params {
				if name == key {
					index = i
				}
			}
			if index < 0 {
				return nil, &pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("%s() got an unexpected keyword argument '%s'", qualname, key)}
			}
			if set[index] {
				return nil, &pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("%s() got multiple values for argument '%s'", qualname, key)}
			}
			value, err := f.method(kwargs, "__getitem__", key)
			if err != nil {
				return nil, err
			}
			bound[index], set[index] = pyhost.Borrowed(value), true
		}
	}
	for i := range bound {
		if !set[i] {
			bound[i] = defaults[i]
		}
	}
	return bound, nil
}

// Python object for a bound value, a borrowed pointer as is or a Go default converted and kept
func (f *frame) object(v any) (uintptr, error) {
	if b, ok := v.(pyhost.Borrowed); ok {
		return uintptr(b), nil
	}
	return f.keep(f.g.ToPython(v))
}
