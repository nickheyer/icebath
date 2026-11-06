package pyjson

import (
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// JSON object of a utf-8-sig file, empty when it is missing, unreadable, malformed or not an object
func ReadObjectOrEmpty(path string) *Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return NewObject()
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return NewObject()
	}
	v, err := Loads(text)
	if err != nil {
		return NewObject()
	}
	if obj, ok := v.(*Object); ok {
		return obj
	}
	return NewObject()
}
