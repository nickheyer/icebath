package jsjson

// Keys in insertion order as Object.keys lists them
func (o *OrderedObject) Keys() []string { return append([]string(nil), o.keys...) }
