//go:build windows

package approval

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	pathSep  = `\`
	sepChars = `\/`
)

func isSep(c byte) bool { return c == '/' || c == '\\' }

func splitRoot(p string) (string, string, string) {
	norm := strings.ReplaceAll(p, "/", `\`)
	switch {
	case strings.HasPrefix(norm, `\`):
		if strings.HasPrefix(norm, `\\`) {
			start := 2
			if len(norm) >= 8 && strings.EqualFold(norm[:8], `\\?\UNC\`) {
				start = 8
			}
			index := strings.IndexByte(norm[start:], '\\')
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
		}
		return "", p[:1], p[1:]
	case len(norm) >= 2 && norm[1] == ':':
		if len(norm) >= 3 && norm[2] == '\\' {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	}
	return "", "", p
}

func splitDrive(p string) (string, string) {
	d, r, t := splitRoot(p)
	return d, r + t
}

func splitPath(p string) (string, string) {
	d, r, rest := splitRoot(p)
	i := len(rest)
	for i > 0 && !isSep(rest[i-1]) {
		i--
	}
	head, tail := rest[:i], rest[i:]
	return d + r + strings.TrimRight(head, `\/`), tail
}

func pyBasename(p string) string {
	_, tail := splitPath(p)
	return tail
}

func pathName(token string) string {
	_, _, rest := splitRoot(token)
	last := ""
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part != "." {
			last = part
		}
	}
	return last
}

func normcase(p string) string { return pyLower(strings.ReplaceAll(p, "/", `\`)) }

func driveForm(p string) string {
	rest := ""
	switch {
	case strings.HasPrefix(p, "/mnt/") && len(p) >= 7 && isASCIILetter(p[5]) && p[6] == '/':
		rest = p[5:]
	case len(p) >= 3 && p[0] == '/' && isASCIILetter(p[1]) && p[2] == '/':
		rest = p[1:]
	default:
		return strings.ReplaceAll(p, "/", `\`)
	}
	tail := strings.TrimSuffix(rest[2:], "\n")
	if tail == "" || strings.Contains(tail, "\n") {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return strings.ReplaceAll(rest[:1]+`:\`+tail, "/", `\`)
}

func expandVars(path string) string {
	if !strings.ContainsAny(path, "$%") {
		return path
	}
	varchar := func(c byte) bool { return asciiWord(c) || c == '-' }
	var res strings.Builder
	index := 0
	for index < len(path) {
		c := path[index]
		switch c {
		case '\'':
			path = path[index+1:]
			if k := strings.IndexByte(path, '\''); k >= 0 {
				res.WriteByte('\'')
				res.WriteString(path[:k+1])
				index = k
			} else {
				res.WriteByte('\'')
				res.WriteString(path)
				index = len(path) - 1
			}
		case '%':
			if index+1 < len(path) && path[index+1] == '%' {
				res.WriteByte('%')
				index++
				break
			}
			path = path[index+1:]
			k := strings.IndexByte(path, '%')
			if k < 0 {
				res.WriteByte('%')
				res.WriteString(path)
				index = len(path) - 1
				break
			}
			name := path[:k]
			if v, ok := os.LookupEnv(name); ok {
				res.WriteString(v)
			} else {
				res.WriteString("%" + name + "%")
			}
			index = k
		case '$':
			switch {
			case index+1 < len(path) && path[index+1] == '$':
				res.WriteByte('$')
				index++
			case index+1 < len(path) && path[index+1] == '{':
				path = path[index+2:]
				k := strings.IndexByte(path, '}')
				if k < 0 {
					res.WriteString("${" + path)
					index = len(path) - 1
					break
				}
				name := path[:k]
				if v, ok := os.LookupEnv(name); ok {
					res.WriteString(v)
				} else {
					res.WriteString("${" + name + "}")
				}
				index = k
			default:
				index++
				start := index
				for index < len(path) && varchar(path[index]) {
					index++
				}
				name := path[start:index]
				if v, ok := os.LookupEnv(name); ok {
					res.WriteString(v)
				} else {
					res.WriteString("$" + name)
				}
				if index < len(path) {
					index--
				}
			}
		default:
			res.WriteByte(c)
		}
		index++
	}
	return res.String()
}

func realpath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	prefix, tail := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return abs
		}
		leaf := filepath.Base(prefix)
		if tail == "" {
			tail = leaf
		} else {
			tail = leaf + `\` + tail
		}
		prefix = parent
	}
}
