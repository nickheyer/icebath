package pyjsonschema

// Text pprint.pformat gives a value at width 72 with dict insertion order kept
func Pformat(v any) string { return pformatAt(pyValue(v), nil) }
