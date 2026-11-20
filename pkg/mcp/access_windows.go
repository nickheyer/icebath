//go:build windows

package mcp

import "os"

// Reports whether the file exists, all Windows os.access X_OK checks
func accessX(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
