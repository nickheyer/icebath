package plugins

// Frozen export an updater from an older checkout imports after the swap, the removed compat layer reports nothing
func CompatReport(manifests any, force bool) map[string][]any { return map[string][]any{} }

// Frozen export reporting the compat layer removal as always in effect
func RemovalInEffect(today any) bool { return true }

// Frozen export with no post update notice lines left to report
func SummaryLines(report any, today any) []string { return []string{} }
