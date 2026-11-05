package pypath

import "strings"

// Anchor and components of a host pathlib path, empty and dot components dropped
func PathParts(p string) (string, []string) {
	isSep := func(r rune) bool { return r == '/' }
	drive, root := "", ""
	if windows {
		isSep = func(r rune) bool { return r == '/' || r == '\\' }
		var rest string
		drive, root, rest = NtSplitRoot(p)
		if root != "" {
			root = `\`
		}
		drive = strings.ReplaceAll(drive, "/", `\`)
		p = rest
	} else if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		root, p = "//", p[2:]
	} else if strings.HasPrefix(p, "/") {
		root = "/"
	}
	var parts []string
	for _, part := range strings.FieldsFunc(p, isSep) {
		if part != "." {
			parts = append(parts, part)
		}
	}
	return drive + root, parts
}

func pathSep() string {
	if windows {
		return `\`
	}
	return "/"
}

// str of a host pathlib path, dot for an empty one
func PathStr(p string) string {
	anchor, parts := PathParts(p)
	if out := anchor + strings.Join(parts, pathSep()); out != "" {
		return out
	}
	return "."
}

// pathlib name, the final component or empty for a bare anchor
func PathName(p string) string {
	_, parts := PathParts(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// pathlib suffix, the extension splitext gives the name
func PathSuffix(p string) string {
	_, ext := splitext(PathName(p), "/")
	return ext
}

// pathlib stem, the name without its suffix
func PathStem(p string) string {
	name := PathName(p)
	return name[:len(name)-len(PathSuffix(p))]
}

// pathlib parent, the path itself for a bare anchor or dot
func PathParent(p string) string {
	anchor, parts := PathParts(p)
	if len(parts) == 0 {
		return PathStr(p)
	}
	if out := anchor + strings.Join(parts[:len(parts)-1], pathSep()); out != "" {
		return out
	}
	return "."
}

// pathlib parents from the nearest ancestor up to the anchor
func PathParents(p string) []string {
	anchor, parts := PathParts(p)
	var out []string
	for i := len(parts) - 1; i > 0; i-- {
		out = append(out, anchor+strings.Join(parts[:i], pathSep()))
	}
	if len(parts) > 0 {
		if anchor == "" {
			anchor = "."
		}
		out = append(out, anchor)
	}
	return out
}

// pathlib division joining segments, an absolute segment replacing what came before
func PathJoin(p string, segments ...string) string { return PathStr(Join(p, segments...)) }
