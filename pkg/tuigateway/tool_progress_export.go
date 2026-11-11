package tuigateway

// Forced redaction of verbose trail text capped to the TUI budget
func RedactVerboseText(text string) string { return redactTUIVerboseText(text) }
