package pathutil

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Path of p below root with slash separators, a dot for root itself, false unless p is root or inside it once both are cleaned; case is ignored on Windows
func RelativeTo(p, root string) (string, bool) {
	p, root = filepath.Clean(p), filepath.Clean(root)
	var rest string
	switch {
	case samePath(p, root):
		return ".", true
	case root == ".":
		if filepath.VolumeName(p) != "" || os.IsPathSeparator(p[0]) {
			return "", false
		}
		rest = p
	case len(p) > len(root) && samePath(p[:len(root)], root):
		switch {
		case os.IsPathSeparator(root[len(root)-1]):
			rest = p[len(root):]
		case os.IsPathSeparator(p[len(root)]):
			rest = p[len(root)+1:]
		default:
			return "", false
		}
	default:
		return "", false
	}
	if rest == ".." || strings.HasPrefix(rest, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rest), true
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Slash separated path that leads from base to target lexically on any host, failing when only one of them is absolute or base climbs out of reach
func SlashRel(base, target string) (string, error) {
	base, target = path.Clean(base), path.Clean(target)
	if target == base {
		return ".", nil
	}
	if base == "." {
		base = ""
	}
	if target == "." {
		target = ""
	}
	if strings.HasPrefix(base, "/") != strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("slash path %q cannot be made relative to %q", target, base)
	}
	bl, tl := len(base), len(target)
	var b0, bi, t0, ti int
	for {
		for bi < bl && base[bi] != '/' {
			bi++
		}
		for ti < tl && target[ti] != '/' {
			ti++
		}
		if target[t0:ti] != base[b0:bi] {
			break
		}
		if bi < bl {
			bi++
		}
		if ti < tl {
			ti++
		}
		b0, t0 = bi, ti
	}
	if base[b0:bi] == ".." {
		return "", fmt.Errorf("slash path %q cannot be made relative to %q", target, base)
	}
	if b0 == bl {
		return target[t0:], nil
	}
	ups := strings.Count(base[b0:], "/") + 1
	var b strings.Builder
	b.Grow(3*ups + tl - t0)
	b.WriteString("..")
	for range ups - 1 {
		b.WriteString("/..")
	}
	if t0 < tl {
		b.WriteByte('/')
		b.WriteString(target[t0:])
	}
	return b.String(), nil
}
