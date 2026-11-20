package tools

// Whether a terminal cwd value carries no directory choice of its own
func CwdNamesNoDirectory(value string) bool { return cwdPlaceholders[value] }
