package cpythonutil

// Read only attribute computed by Go from the borrowed instance pointer
type Property func(g *GIL, self uintptr) (any, error)

// New class built by type(name, bases, namespace) whose RawFunc and Property members run Go code, a new reference
func (g *GIL) NewClass(module, name string, bases []any, members map[string]any) (uintptr, error) {
	ns := make(map[string]any, len(members)+2)
	ns["__module__"] = module
	ns["__qualname__"] = name
	var held []uintptr
	defer func() {
		for _, o := range held {
			pyDecRef(o)
		}
	}()
	var functools uintptr
	for key, member := range members {
		switch m := member.(type) {
		case RawFunc:
			if functools == 0 {
				f, err := g.Import("functools")
				if err != nil {
					return 0, err
				}
				functools = f
				held = append(held, f)
			}
			fn, err := g.NewRawFunc(key, m)
			if err != nil {
				return 0, err
			}
			held = append(held, fn)
			// A builtin function binds no self, functools.partialmethod makes it a method that does
			bound, err := g.CallMethod(functools, "partialmethod", []any{Borrowed(fn)}, nil)
			if err != nil {
				return 0, err
			}
			held = append(held, bound)
			ns[key] = Borrowed(bound)
		case Property:
			fn, err := g.propertyGetter(key, m)
			if err != nil {
				return 0, err
			}
			held = append(held, fn)
			prop, err := g.CallMethod(g.h.builtins, "property", []any{Borrowed(fn)}, nil)
			if err != nil {
				return 0, err
			}
			held = append(held, prop)
			ns[key] = Borrowed(prop)
		default:
			ns[key] = member
		}
	}
	baseTuple, err := g.tuple(bases)
	if err != nil {
		return 0, err
	}
	held = append(held, baseTuple)
	return g.CallMethod(g.h.builtins, "type", []any{name, Borrowed(baseTuple), ns}, nil)
}

// Native getter for a Property, which property calls with the instance as its only argument
func (g *GIL) propertyGetter(name string, p Property) (uintptr, error) {
	return g.NewRawFunc(name, func(g *GIL, args, _ uintptr) (uintptr, error) {
		if pyTupleSize(args) == 0 {
			return 0, &Exception{Type: "TypeError", Message: name + "() missing self"}
		}
		v, err := p(g, pyTupleGetItem(args, 0))
		if err != nil {
			return 0, err
		}
		return g.ToPython(v)
	})
}
