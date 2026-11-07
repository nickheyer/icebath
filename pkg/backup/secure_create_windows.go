//go:build windows

package backup

// Leaves creation to SQLite on Windows, where the owner only mode bits carry no meaning
func secureCreate(string) error { return nil }
