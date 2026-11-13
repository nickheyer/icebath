//go:build unix

package home

import "path/filepath"

// Absolute path of an existing location with every symlink followed, an error when any part is missing
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
