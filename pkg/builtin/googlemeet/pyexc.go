package googlemeet

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Python exception classes the handlers raise, named so tool dispatch reports the upstream class
type (
	KeyError       struct{ Key string }
	ValueError     struct{ Msg string }
	TypeError      struct{ Msg string }
	AttributeError struct{ Msg string }
	OverflowError  struct{ Msg string }
)

func (e *KeyError) Error() string       { return pyrepr.StrRepr(e.Key) }
func (e *ValueError) Error() string     { return e.Msg }
func (e *TypeError) Error() string      { return e.Msg }
func (e *AttributeError) Error() string { return e.Msg }
func (e *OverflowError) Error() string  { return e.Msg }
