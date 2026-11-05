package pypath

import (
	"os"
	"strings"
)

func isNTSep(c byte) bool { return c == '\\' || c == '/' }

// ntpath.splitroot into drive or UNC share, root separator and the rest
func NtSplitRoot(p string) (string, string, string) {
	norm := strings.ReplaceAll(p, "/", `\`)
	switch {
	case strings.HasPrefix(norm, `\\`):
		start := 2
		if len(norm) >= 8 && strings.ToUpper(norm[:8]) == `\\?\UNC\` {
			start = 8
		}
		index := strings.IndexByte(norm[min(start, len(norm)):], '\\')
		if index < 0 {
			return p, "", ""
		}
		index += start
		index2 := strings.IndexByte(norm[index+1:], '\\')
		if index2 < 0 {
			return p, "", ""
		}
		index2 += index + 1
		return p[:index2], p[index2 : index2+1], p[index2+1:]
	case strings.HasPrefix(norm, `\`):
		return "", p[:1], p[1:]
	case len(norm) >= 2 && norm[1] == ':':
		if len(norm) >= 3 && norm[2] == '\\' {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	}
	return "", "", p
}

// ntpath.splitdrive, the drive or UNC share and the remaining path
func NtSplitdrive(p string) (string, string) {
	drive, root, rest := NtSplitRoot(p)
	return drive, root + rest
}

// ntpath.normpath, slashes turned to backslashes and parent links collapsed below the drive and root
func NtNormpath(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	drive, root, rest := NtSplitRoot(p)
	comps := strings.Split(rest, `\`)
	for i := 0; i < len(comps); {
		switch {
		case comps[i] == "" || comps[i] == ".":
			comps = append(comps[:i], comps[i+1:]...)
		case comps[i] == ".." && i > 0 && comps[i-1] != "..":
			comps = append(comps[:i-1], comps[i+1:]...)
			i--
		case comps[i] == ".." && i == 0 && root != "":
			comps = comps[1:]
		default:
			i++
		}
	}
	if drive+root == "" && len(comps) == 0 {
		comps = []string{"."}
	}
	return drive + root + strings.Join(comps, `\`)
}

// ntpath.join with drive relative and drive changing components handled like CPython
func NtJoin(path string, paths ...string) string {
	drive, root, rest := NtSplitRoot(path)
	for _, p := range paths {
		pDrive, pRoot, pRest := NtSplitRoot(p)
		if pRoot != "" {
			if pDrive != "" || drive == "" {
				drive = pDrive
			}
			root, rest = pRoot, pRest
			continue
		}
		if pDrive != "" && pDrive != drive {
			if strings.ToLower(pDrive) != strings.ToLower(drive) {
				drive, root, rest = pDrive, pRoot, pRest
				continue
			}
			drive = pDrive
		}
		if rest != "" && !isNTSep(rest[len(rest)-1]) {
			rest += `\`
		}
		rest += pRest
	}
	if rest != "" && root == "" && drive != "" && !strings.ContainsRune(`:\/`, rune(drive[len(drive)-1])) {
		return drive + `\` + rest
	}
	return drive + root + rest
}

// ntpath.split, the head keeping drive and root and losing trailing separators
func NtSplit(p string) (string, string) {
	drive, root, rest := NtSplitRoot(p)
	i := len(rest)
	for i > 0 && !isNTSep(rest[i-1]) {
		i--
	}
	return drive + root + strings.TrimRight(rest[:i], `\/`), rest[i:]
}

// ntpath.basename, the tail of split
func NtBasename(p string) string {
	_, tail := NtSplit(p)
	return tail
}

// ntpath.dirname, the head of split
func NtDirname(p string) string {
	head, _ := NtSplit(p)
	return head
}

// ntpath.splitext with both slash kinds as separators
func NtSplitext(p string) (string, string) { return splitext(p, `\/`) }

// ntpath.isabs, a drive letter followed by a separator or a leading double separator
func NtIsAbs(p string) bool {
	s := strings.ReplaceAll(p[:min(3, len(p))], "/", `\`)
	return strings.HasPrefix(s, `\\`) || (len(s) == 3 && s[1:] == `:\`)
}

// ntpath.expanduser from USERPROFILE or HOMEDRIVE and HOMEPATH, a named user resolved beside the current home
func NtExpanduser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	i := 1
	for i < len(p) && !isNTSep(p[i]) {
		i++
	}
	var home string
	if profile, ok := os.LookupEnv("USERPROFILE"); ok {
		home = profile
	} else if homePath, ok := os.LookupEnv("HOMEPATH"); ok {
		home = NtJoin(os.Getenv("HOMEDRIVE"), homePath)
	} else {
		return p
	}
	if i != 1 {
		if target := p[1:i]; target != os.Getenv("USERNAME") {
			if os.Getenv("USERNAME") != NtBasename(home) {
				return p
			}
			home = NtJoin(NtDirname(home), target)
		}
	}
	return home + p[i:]
}
