package tools

// Text keeping head and tail code points around an elision marker when longer than both
func ElideMiddle(text string, head, tail int) string { return elideMiddle(text, head, tail) }
