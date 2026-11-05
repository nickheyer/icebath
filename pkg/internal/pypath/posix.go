package pypath

import "strings"

// posixpath.normpath, dot parts dropped, dot dot folded, exactly two leading slashes kept
func PosixNormpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, comp := range strings.Split(p, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if comp != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// posixpath.join, an absolute segment restarting the path
func PosixJoin(a string, p ...string) string {
	path := a
	for _, b := range p {
		switch {
		case strings.HasPrefix(b, "/"):
			path = b
		case path == "" || strings.HasSuffix(path, "/"):
			path += b
		default:
			path += "/" + b
		}
	}
	return path
}

// Head with trailing slashes removed unless it is all slashes
func posixHead(p string) string {
	head := p[:strings.LastIndex(p, "/")+1]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// posixpath.split into head and tail
func PosixSplit(p string) (string, string) {
	return posixHead(p), p[strings.LastIndex(p, "/")+1:]
}

// posixpath.basename, the text after the last slash
func PosixBasename(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// posixpath.dirname, the split head
func PosixDirname(p string) string { return posixHead(p) }

// posixpath.splitext through genericpath._splitext
func PosixSplitext(p string) (string, string) { return splitext(p, "/") }

// posixpath.isabs, a leading slash
func PosixIsAbs(p string) bool { return strings.HasPrefix(p, "/") }

// genericpath._splitext over every separator in seps, leading dots never starting an extension
func splitext(p, seps string) (string, string) {
	sepIndex := strings.LastIndexAny(p, seps)
	dotIndex := strings.LastIndex(p, ".")
	if dotIndex > sepIndex {
		for i := sepIndex + 1; i < dotIndex; i++ {
			if p[i] != '.' {
				return p[:dotIndex], p[dotIndex:]
			}
		}
	}
	return p, ""
}

// String form of a PurePosixPath, duplicate slashes and dot parts dropped, a leading double slash kept
func PosixPath(p string) string {
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root = "//"
	case strings.HasPrefix(p, "/"):
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if root == "" && len(parts) == 0 {
		return "."
	}
	return root + strings.Join(parts, "/")
}
