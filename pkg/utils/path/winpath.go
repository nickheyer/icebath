package pathutil

import "strings"

func isWindowsSep(c byte) bool { return c == '\\' || c == '/' }

// Index of the first Windows separator in p at or after from, -1 when there is none
func indexWindowsSep(p string, from int) int {
	for i := from; i < len(p); i++ {
		if isWindowsSep(p[i]) {
			return i
		}
	}
	return -1
}

// Drive letter or UNC share, root separator and the rest of a Windows path on any host, the three concatenating back to p
func WindowsSplitRoot(p string) (drive, root, rest string) {
	switch {
	case len(p) >= 2 && isWindowsSep(p[0]) && isWindowsSep(p[1]):
		start := 2
		if len(p) >= 8 && p[2] == '?' && isWindowsSep(p[3]) && strings.EqualFold(p[4:7], "UNC") && isWindowsSep(p[7]) {
			start = 8
		}
		server := indexWindowsSep(p, start)
		if server < 0 {
			return p, "", ""
		}
		share := indexWindowsSep(p, server+1)
		if share < 0 {
			return p, "", ""
		}
		return p[:share], p[share : share+1], p[share+1:]
	case len(p) >= 1 && isWindowsSep(p[0]):
		return "", p[:1], p[1:]
	case len(p) >= 2 && p[1] == ':':
		if len(p) >= 3 && isWindowsSep(p[2]) {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	}
	return "", "", p
}

// Drive letter or UNC share of a Windows path on any host and everything after it
func WindowsSplitDrive(p string) (drive, rest string) {
	drive, root, tail := WindowsSplitRoot(p)
	return drive, root + tail
}

// Whether a Windows path is absolute on any host: a drive letter followed by a separator, or a leading double separator
func WindowsIsAbs(p string) bool {
	return len(p) >= 2 && isWindowsSep(p[0]) && isWindowsSep(p[1]) || len(p) >= 3 && p[1] == ':' && isWindowsSep(p[2])
}

// Windows path cleaned on any host: backslash separators, empty and dot elements dropped, dot-dot folded below the anchor, a dot for nothing
func WindowsClean(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	drive, root, rest := WindowsSplitRoot(p)
	elems := make([]string, 0, strings.Count(rest, `\`)+1)
	for elem := range strings.SplitSeq(rest, `\`) {
		switch {
		case elem == "" || elem == ".":
		case elem != "..":
			elems = append(elems, elem)
		case len(elems) > 0 && elems[len(elems)-1] != "..":
			elems = elems[:len(elems)-1]
		case root == "":
			elems = append(elems, elem)
		}
	}
	if drive == "" && root == "" && len(elems) == 0 {
		return "."
	}
	return drive + root + strings.Join(elems, `\`)
}

// Elements joined and cleaned under Windows rules on any host: a rooted element restarts the path on the current drive, another drive restarts it, empty for nothing but empty elements
func WindowsJoin(base string, elem ...string) string {
	if p := windowsJoinRaw(base, elem); p != "" {
		return WindowsClean(p)
	}
	return ""
}

// Windows join of the elements without cleaning, the drive letter case of the last element naming the drive kept
func windowsJoinRaw(base string, elems []string) string {
	drive, root, rest := WindowsSplitRoot(base)
	buf := []byte(rest)
	for _, elem := range elems {
		eDrive, eRoot, eRest := WindowsSplitRoot(elem)
		switch {
		case eRoot != "":
			if eDrive != "" || drive == "" {
				drive = eDrive
			}
			root, buf = eRoot, append(buf[:0], eRest...)
			continue
		case eDrive != "" && eDrive != drive:
			if !strings.EqualFold(eDrive, drive) {
				drive, root, buf = eDrive, "", append(buf[:0], eRest...)
				continue
			}
			drive = eDrive
		}
		if len(buf) > 0 && !isWindowsSep(buf[len(buf)-1]) {
			buf = append(buf, '\\')
		}
		buf = append(buf, eRest...)
	}
	if len(buf) > 0 && root == "" && drive != "" && !strings.ContainsRune(`:\/`, rune(drive[len(drive)-1])) {
		return drive + `\` + string(buf)
	}
	return drive + root + string(buf)
}

// Head of a Windows path up to its last separator with trailing separators dropped unless they are the root, and the final element
func windowsSplit(p string) (head, tail string) {
	drive, root, rest := WindowsSplitRoot(p)
	i := len(rest)
	for i > 0 && !isWindowsSep(rest[i-1]) {
		i--
	}
	return drive + root + strings.TrimRight(rest[:i], `\/`), rest[i:]
}

// Windows path without its final element on any host, cleaned, a dot when no directory is left and the share itself for a share root, the way filepath.Dir treats Windows paths
func WindowsDir(p string) string {
	drive, _, _ := WindowsSplitRoot(p)
	i := len(p) - 1
	for i >= len(drive) && !isWindowsSep(p[i]) {
		i--
	}
	dir := WindowsClean(p[len(drive) : i+1])
	drive = strings.ReplaceAll(drive, "/", `\`)
	if dir == "." && len(drive) > 2 {
		return drive
	}
	return drive + dir
}

// Final element of a Windows path on any host after trailing separators, a dot for an empty path and a backslash for a bare anchor, the way filepath.Base treats Windows paths
func WindowsBase(p string) string {
	if p == "" {
		return "."
	}
	for len(p) > 0 && isWindowsSep(p[len(p)-1]) {
		p = p[:len(p)-1]
	}
	drive, _, _ := WindowsSplitRoot(p)
	p = p[len(drive):]
	i := len(p) - 1
	for i >= 0 && !isWindowsSep(p[i]) {
		i--
	}
	if p = p[i+1:]; p == "" {
		return `\`
	}
	return p
}

// Comparison key of a Windows path on any host, lower case with backslash separators
func WindowsFoldCase(p string) string { return strings.ToLower(strings.ReplaceAll(p, "/", `\`)) }
