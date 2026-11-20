package pyjsonschema

// Python exception the port raised, its str text fixed in Msg or rendered on demand
type Exception struct {
	Type   string
	Msg    string
	render func(st *state) string
}

func (e *Exception) Error() string {
	if e.render != nil {
		return e.render(nil)
	}
	return e.Msg
}

// Python class name of the raised exception
func (e *Exception) PyTypeName() string { return e.Type }

func raise(typ, msg string) { panic(&Exception{Type: typ, Msg: msg}) }

// str of an error the port raised as Python computes it callerDepth frames deep, a RecursionError when Python's would fail
func Str(err error, callerDepth int) (text string, failed error) {
	st := newState(callerDepth)
	failed = catch(func() {
		switch e := err.(type) {
		case *SchemaError:
			text = e.err.strAt(st, "metaschema", "schema")
		case *ValidationError:
			text = e.err.strAt(st, "schema", "instance")
		case *Exception:
			if e.render != nil {
				text = e.render(st)
			} else {
				text = e.Msg
			}
		default:
			text = err.Error()
		}
	})
	return text, failed
}

// Runs fn and returns the Python exception it raised, nil when it completed
func catch(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *Exception:
				err = e
			case *SchemaError:
				err = e
			case *ValidationError:
				err = e
			default:
				panic(r)
			}
		}
	}()
	fn()
	return nil
}
