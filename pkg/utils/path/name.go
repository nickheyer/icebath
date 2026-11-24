package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Final element of the cleaned path, empty for an empty, dot or root path
func Name(p string) string {
	name := filepath.Base(filepath.Clean(p))
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return name
}

// Extension of the final element from its last dot, empty when that dot is one of the leading dots of a name such as .bashrc
func Suffix(p string) string { return suffixOf(Name(p)) }

func suffixOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || strings.TrimLeft(name[:i], ".") == "" {
		return ""
	}
	return name[i:]
}

// Path as written split before the extension of its final element, the extension found the way Suffix finds it
func SplitExt(p string) (root, ext string) {
	i := len(p)
	for i > 0 && !os.IsPathSeparator(p[i-1]) {
		i--
	}
	ext = suffixOf(p[i:])
	return p[:len(p)-len(ext)], ext
}

// Final element without its Suffix
func Stem(p string) string {
	name := Name(p)
	return name[:len(name)-len(suffixOf(name))]
}

// Cleaned path with the Suffix of its final element replaced by suffix, which is empty or starts with a dot and holds no separator, so that any Suffix result fits
func WithSuffix(p, suffix string) (string, error) {
	if suffix != "" && (suffix[0] != '.' || strings.ContainsRune(suffix, '/') || strings.ContainsRune(suffix, filepath.Separator)) {
		return "", fmt.Errorf("invalid suffix %q", suffix)
	}
	clean := filepath.Clean(p)
	name := Name(clean)
	if name == "" {
		return "", fmt.Errorf("path %q has no final element to give a suffix", p)
	}
	return clean[:len(clean)-len(name)] + name[:len(name)-len(suffixOf(name))] + suffix, nil
}

// Anchor of a path, its volume and root separator, and its elements with empty and dot elements dropped and dot-dot kept
func Parts(p string) (anchor string, elems []string) {
	isSep := func(r rune) bool { return r == '/' }
	if runtime.GOOS == "windows" {
		isSep = func(r rune) bool { return r == '/' || r == '\\' }
		drive, root, rest := WindowsSplitRoot(p)
		anchor, p = strings.ReplaceAll(drive, "/", `\`), rest
		if root != "" {
			anchor += `\`
		}
	} else if strings.HasPrefix(p, "/") {
		anchor = "/"
	}
	for elem := range strings.FieldsFuncSeq(p, isSep) {
		if elem != "." {
			elems = append(elems, elem)
		}
	}
	return anchor, elems
}

// Ancestors of the cleaned path from its parent up to its root, or up to a dot for a relative path
func Parents(p string) []string {
	var out []string
	for p = filepath.Clean(p); ; {
		dir := filepath.Dir(p)
		if dir == p {
			return out
		}
		out = append(out, dir)
		p = dir
	}
}
