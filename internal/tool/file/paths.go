package file

import (
	"strings"
)

type pathStyle struct {
	windows bool
}

func (p pathStyle) isSep(c byte) bool {
	return c == '/' || (p.windows && c == '\\')
}

func (p pathStyle) lastSep(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if p.isSep(s[i]) {
			return i
		}
	}
	return -1
}

func (p pathStyle) trimRightSeps(s string) string {
	for len(s) > 0 && p.isSep(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func splitWindowsRoot(s string) (drive, root, rest string) {
	norm := strings.ReplaceAll(s, "/", `\`)
	switch {
	case strings.HasPrefix(norm, `\\`):
		start := 2
		if len(norm) >= 8 && strings.EqualFold(norm[:8], `\\?\UNC\`) {
			start = 8
		}
		index := strings.Index(norm[start:], `\`)
		if index == -1 {
			return s, "", ""
		}
		index += start
		index2 := strings.Index(norm[index+1:], `\`)
		if index2 == -1 {
			return s, "", ""
		}
		index2 += index + 1
		return s[:index2], s[index2 : index2+1], s[index2+1:]
	case strings.HasPrefix(norm, `\`):
		return "", s[:1], s[1:]
	case len(norm) >= 2 && norm[1] == ':':
		if len(norm) >= 3 && norm[2] == '\\' {
			return s[:2], s[2:3], s[3:]
		}
		return s[:2], "", s[2:]
	}
	return "", "", s
}

func (p pathStyle) split(s string) (string, string) {
	if !p.windows {
		i := strings.LastIndex(s, "/") + 1
		head, tail := s[:i], s[i:]
		if head != "" && strings.Trim(head, "/") != "" {
			head = strings.TrimRight(head, "/")
		}
		return head, tail
	}
	drive, root, rest := splitWindowsRoot(s)
	i := p.lastSep(rest) + 1
	return drive + root + p.trimRightSeps(rest[:i]), rest[i:]
}

func (p pathStyle) dir(s string) string {
	head, _ := p.split(s)
	return head
}

func (p pathStyle) base(s string) string {
	_, tail := p.split(s)
	return tail
}

func (p pathStyle) isAbs(s string) bool {
	if !p.windows {
		return strings.HasPrefix(s, "/")
	}
	head := s
	if len(head) > 3 {
		head = head[:3]
	}
	head = strings.ReplaceAll(head, "/", `\`)
	return (len(head) >= 3 && head[1:3] == `:\`) || strings.HasPrefix(head, `\\`)
}

func (p pathStyle) join(a, b string) string {
	if !p.windows {
		switch {
		case strings.HasPrefix(b, "/"):
			return b
		case a == "" || strings.HasSuffix(a, "/"):
			return a + b
		}
		return a + "/" + b
	}
	drive, root, rest := splitWindowsRoot(a)
	bDrive, bRoot, bRest := splitWindowsRoot(b)
	switch {
	case bRoot != "":
		if bDrive != "" || drive == "" {
			drive = bDrive
		}
		root, rest = bRoot, bRest
	case bDrive != "" && bDrive != drive && !strings.EqualFold(bDrive, drive):
		drive, root, rest = bDrive, bRoot, bRest
	default:
		if bDrive != "" && bDrive != drive {
			drive = bDrive
		}
		if rest != "" && !p.isSep(rest[len(rest)-1]) {
			rest += `\`
		}
		rest += bRest
	}
	if rest != "" && root == "" && drive != "" && !strings.ContainsRune(`:\/`, rune(drive[len(drive)-1])) {
		return drive + `\` + rest
	}
	return drive + root + rest
}

func (p pathStyle) ext(s string) string {
	sep := p.lastSep(s)
	dot := strings.LastIndex(s, ".")
	if dot > sep {
		for i := sep + 1; i < dot; i++ {
			if s[i] != '.' {
				return s[dot:]
			}
		}
	}
	return ""
}

func (p pathStyle) stem(s string) string {
	return s[:len(s)-len(p.ext(s))]
}

func hostPathKey(path string) string {
	text := strings.ReplaceAll(path, `\`, "/")
	if len(text) >= 2 && text[1] == ':' {
		text = strings.ToLower(text[:1]) + text[1:]
	}
	if len(text) > 3 {
		text = strings.TrimRight(text, "/")
	}
	return text
}

func translateMountedHostPath(path, hostRoot, containerRoot string) (string, bool) {
	if path == "" || hostRoot == "" || containerRoot == "" {
		return "", false
	}
	key := hostPathKey(path)
	root := hostPathKey(hostRoot)
	if key == "" || root == "" {
		return "", false
	}
	mount := strings.TrimRight(containerRoot, "/")
	if mount == "" {
		mount = "/"
	}
	if key == root {
		return mount, true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return mount + "/" + key[len(prefix):], true
}
