package googlemeet

import (
	"strconv"
)

// Errors the handlers return, each type name the kind tool dispatch records
type (
	KeyError       struct{ Key string }
	ValueError     struct{ Msg string }
	TypeError      struct{ Msg string }
	AttributeError struct{ Msg string }
)

func (e *KeyError) Error() string       { return strconv.Quote(e.Key) }
func (e *ValueError) Error() string     { return e.Msg }
func (e *TypeError) Error() string      { return e.Msg }
func (e *AttributeError) Error() string { return e.Msg }
