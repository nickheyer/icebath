package tools

// Configured approval mode normalized to manual, smart or off, unknown strings falling back to manual
func NormalizeApprovalMode(mode any) string { return normalizeApprovalMode(mode) }
