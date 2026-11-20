package pypath

import "strings"

// pathlib relative_to as posix text, false when p does not sit under root component wise
func PathRelativeTo(p, root string) (string, bool) {
	pa, pp := PathParts(p)
	ra, rp := PathParts(root)
	norm := func(s string) string {
		if windows {
			return strings.ToLower(strings.ReplaceAll(s, "/", `\`))
		}
		return s
	}
	if norm(pa) != norm(ra) || len(rp) > len(pp) {
		return "", false
	}
	for i := range rp {
		if norm(rp[i]) != norm(pp[i]) {
			return "", false
		}
	}
	if rest := pp[len(rp):]; len(rest) > 0 {
		return strings.Join(rest, "/"), true
	}
	return ".", true
}
