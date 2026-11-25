package pathutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Path with repeated separators and dot elements dropped and every dot-dot kept, so it still names the file the system opens when a dot-dot climbs out of a symlinked directory; a dot when nothing is left
func CleanKeepDotDot(p string) string {
	anchor, elems := Parts(p)
	return fromParts(anchor, elems)
}

// CleanKeepDotDot form of p without its final element, a dot-dot one included, so it is the directory the system walks through to reach that element; the bare anchor or a dot when p has no element
func DirKeepDotDot(p string) string {
	anchor, elems := Parts(p)
	if len(elems) > 0 {
		elems = elems[:len(elems)-1]
	}
	return fromParts(anchor, elems)
}

// Final element of the CleanKeepDotDot form of p, a dot-dot one included, empty for a bare anchor or a path with no element
func NameKeepDotDot(p string) string {
	_, elems := Parts(p)
	if len(elems) == 0 {
		return ""
	}
	return elems[len(elems)-1]
}

// Elements of p after those of root joined with slashes, both taken in CleanKeepDotDot form so a dot-dot is matched as an element and the result names the same file below root; a dot for root itself, false unless the anchor and elements of root begin p; case is ignored on Windows
func RelativeToKeepDotDot(p, root string) (string, bool) {
	pa, pe := Parts(p)
	ra, re := Parts(root)
	if len(re) > len(pe) || !samePath(pa, ra) {
		return "", false
	}
	for i, e := range re {
		if !samePath(pe[i], e) {
			return "", false
		}
	}
	if len(pe) == len(re) {
		return ".", true
	}
	return strings.Join(pe[len(re):], "/"), true
}

// Elements joined as Join joins them, an absolute element replacing everything before it, with the result in CleanKeepDotDot form instead of cleaned; empty when every element is empty
func JoinKeepDotDot(base string, elem ...string) string {
	var joined string
	if runtime.GOOS == "windows" {
		joined = windowsJoinRaw(base, elem)
	} else {
		joined = posixJoinRaw(base, elem)
	}
	if joined == "" {
		return ""
	}
	return CleanKeepDotDot(joined)
}

// Non-empty elements joined with slashes from the last absolute one on, nothing cleaned
func posixJoinRaw(base string, elems []string) string {
	all := append([]string{base}, elems...)
	for i := len(all) - 1; i > 0; i-- {
		if strings.HasPrefix(all[i], "/") {
			all = all[i:]
			break
		}
	}
	var b strings.Builder
	for _, e := range all {
		if e == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		b.WriteString(e)
	}
	return b.String()
}

// Anchor followed by the elements joined with the host separator, a dot when both are empty
func fromParts(anchor string, elems []string) string {
	if out := anchor + strings.Join(elems, string(filepath.Separator)); out != "" {
		return out
	}
	return "."
}
