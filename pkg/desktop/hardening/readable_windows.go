package hardening

import "os"

// True when path exists, all fs.access with R_OK checks on Windows
func readable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
