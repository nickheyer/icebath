//go:build !windows

package approval

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	pathSep  = "/"
	sepChars = "/"
)

func pyBasename(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

func splitDrive(p string) (string, string) { return "", p }

func splitPath(p string) (string, string) {
	i := strings.LastIndexByte(p, '/') + 1
	head, tail := p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

func pathName(token string) string {
	last := ""
	for _, part := range strings.Split(token, "/") {
		if part != "" && part != "." {
			last = part
		}
	}
	return last
}

func normcase(p string) string { return p }

func driveForm(p string) string { return p }

func expandVars(path string) string {
	if !strings.Contains(path, "$") {
		return path
	}
	i := 0
	for {
		k := strings.IndexByte(path[i:], '$')
		if k < 0 {
			return path
		}
		start := i + k
		j := start + 1
		var name string
		switch {
		case j < len(path) && path[j] == '{':
			e := strings.IndexByte(path[j:], '}')
			if e < 0 {
				i = start + 1
				continue
			}
			name = path[j+1 : j+e]
			j += e + 1
		default:
			for j < len(path) && asciiWord(path[j]) {
				j++
			}
			if j == start+1 {
				i = start + 1
				continue
			}
			name = path[start+1 : j]
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			i = j
			continue
		}
		tail := path[j:]
		path = path[:start] + value
		i = len(path)
		path += tail
	}
}

func realpath(p string) string {
	path, _ := joinRealpath("", p, map[string]*string{})
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, path)
		}
	}
	return filepath.Clean(path)
}

func joinRealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path != "" {
				var leaf string
				path, leaf = splitPath(path)
				if leaf == ".." {
					path = posixJoin(path, "..", "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := posixJoin(path, name)
		info, err := os.Lstat(newpath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if prior, ok := seen[newpath]; ok {
			if prior != nil {
				path = *prior
				continue
			}
			return posixJoin(newpath, rest), false
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		var ok bool
		path, ok = joinRealpath(path, target, seen)
		if !ok {
			return posixJoin(path, rest), false
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, true
}

func posixJoin(a string, parts ...string) string {
	path := a
	for _, b := range parts {
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
