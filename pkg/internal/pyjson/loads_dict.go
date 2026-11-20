package pyjson

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Decoded JSON object of text, the AttributeError a dict get call raises on any other decoded value
func LoadsDict(text string) (*Object, error) {
	v, err := Loads(text)
	if err != nil {
		return nil, err
	}
	d, ok := v.(*Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return d, nil
}

// Value of key, def when the key is absent as dict get with a default returns
func (o *Object) GetOr(key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}
