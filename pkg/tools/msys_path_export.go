package tools

// Native Windows form of a Git Bash drive path, unchanged off Windows
func MsysToWindowsPath(p string) string { return msysToWindowsPath(p) }
