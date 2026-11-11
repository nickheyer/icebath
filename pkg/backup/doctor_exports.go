package backup

// Critical state files and directories of a home a quick snapshot copies, as a fresh slice
func QuickStateFiles() []string { return append([]string(nil), quickStateFiles...) }
