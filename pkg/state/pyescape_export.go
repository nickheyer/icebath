package state

// String bytes.decode unicode_escape yields, the strict UnicodeDecodeError on a malformed escape
func UnicodeEscapeDecode(data []byte) (string, error) { return pyUnicodeEscapeDecode(data) }
