//go:build windows

package pm

// Reports whether this process may execute a file, which os access with X_OK grants any existing file on Windows
func canExecute(path string) bool { return true }
