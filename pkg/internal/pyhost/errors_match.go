package pyhost

import "errors"

// Borrowed exception object err carries, ok false when err holds no Python exception
func (g *GIL) ExceptionObject(err error) (uintptr, bool) {
	var pe *PyError
	if !errors.As(err, &pe) || pe.exc == nil {
		return 0, false
	}
	return pe.exc.obj, true
}

// Reports whether err carries a Python exception that is an instance of cls, as an except clause naming cls would catch it
func (g *GIL) ExceptionMatches(err error, cls uintptr) (bool, error) {
	obj, ok := g.ExceptionObject(err)
	if !ok {
		return false, nil
	}
	return g.IsInstance(obj, cls)
}

// Error raising the exception object exc chained to cause, as raise exc from cause inside its except block does
func (g *GIL) RaiseFrom(exc uintptr, cause error) error {
	if obj, ok := g.ExceptionObject(cause); ok {
		if err := g.SetAttr(exc, "__context__", Borrowed(obj)); err != nil {
			return err
		}
		if err := g.SetAttr(exc, "__cause__", Borrowed(obj)); err != nil {
			return err
		}
	}
	return g.describe(g.NewRef(exc))
}
