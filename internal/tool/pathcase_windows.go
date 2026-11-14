package tool

import (
	"path/filepath"
	"strings"
)

func normcase(p string) string {
	return strings.ToLower(filepath.FromSlash(p))
}
