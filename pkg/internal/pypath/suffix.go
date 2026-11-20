package pypath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Final component of the path as pathlib names it, empty for a root or dot path
func Name(p string) string {
	name := filepath.Base(filepath.Clean(p))
	if name == "." || name == string(filepath.Separator) || name == "/" {
		return ""
	}
	return name
}

// Extension of the final component including its dot, empty when the name has none
func Suffix(p string) string {
	name := Name(p)
	i := strings.LastIndexByte(name, '.')
	if 0 < i && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// Path with the final extension replaced or appended, the pathlib ValueError for an invalid suffix or empty name
func WithSuffix(p, suffix string) (string, error) {
	if strings.ContainsRune(suffix, '/') || strings.ContainsRune(suffix, filepath.Separator) ||
		(suffix != "" && !strings.HasPrefix(suffix, ".")) || suffix == "." {
		return "", fmt.Errorf("Invalid suffix %s", strReprSuffix(suffix))
	}
	name := Name(p)
	if name == "" {
		return "", fmt.Errorf("%s has an empty name", strReprSuffix(p))
	}
	old := Suffix(p)
	clean := filepath.Clean(p)
	dir := clean[:len(clean)-len(name)]
	return dir + name[:len(name)-len(old)] + suffix, nil
}

func strReprSuffix(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
