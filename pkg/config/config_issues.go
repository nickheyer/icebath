package config

// One detected config structure problem with severity error or warning
type ConfigIssue struct {
	Severity string
	Message  string
	Hint     string
}

// Appends an issue built from its severity, message and fix hint
func AddIssue(issues *[]ConfigIssue, severity, message, hint string) {
	*issues = append(*issues, ConfigIssue{Severity: severity, Message: message, Hint: hint})
}
