//go:build windows

package gateway

import "os"

// Opens a blob read only, Windows having no O_NOFOLLOW flag
func openBlobNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}
