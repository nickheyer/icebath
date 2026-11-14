//go:build !windows

package checkpoint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

func canonical(p string) (string, error) {
	p = home.ExpandUser(p)
	if !strings.HasPrefix(p, "/") {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + "/" + p
	}
	out, _, err := realpath("", p, map[string]*string{})
	if err != nil {
		return "", err
	}
	return filepath.Clean(out), nil
}

func realpath(path, rest string, seen map[string]*string) (string, bool, error) {
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
			if path == "" {
				path = ".."
				continue
			}
			dir, base := splitPosix(path)
			path = dir
			if base == ".." {
				path = joinPosix(joinPosix(path, ".."), "..")
			}
			continue
		}
		next := joinPosix(path, name)
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			path = next
			continue
		}
		if done, ok := seen[next]; ok {
			if done != nil {
				path = *done
				continue
			}
			return joinPosix(next, rest), false, nil
		}
		seen[next] = nil
		target, err := os.Readlink(next)
		if err != nil {
			return "", false, err
		}
		resolved, ok, err := realpath(path, target, seen)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return joinPosix(resolved, rest), false, nil
		}
		path = resolved
		seen[next] = &resolved
	}
	return path, true, nil
}

func splitPosix(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

func joinPosix(a, b string) string {
	switch {
	case strings.HasPrefix(b, "/"):
		return b
	case a == "" || strings.HasSuffix(a, "/"):
		return a + b
	default:
		return a + "/" + b
	}
}

func joinRelative(root, rel string) (string, bool) {
	return root + "/" + rel, true
}

func relativeTo(child, root string) (string, bool) {
	c, r := filepath.Clean(child), filepath.Clean(root)
	switch {
	case c == r:
		return ".", true
	case r == "/":
		return strings.TrimPrefix(c, "/"), strings.HasPrefix(c, "/")
	case strings.HasPrefix(c, r+"/"):
		return c[len(r)+1:], true
	}
	return "", false
}

func isSeparator(r rune) bool { return r == '/' }

func isMount(p string) bool {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	self, err := fileutil.LstatID(p)
	if err != nil {
		return false
	}
	parent, err := fileutil.LstatID(p + "/..")
	if err != nil {
		return false
	}
	return self.Device != parent.Device || self.Inode == parent.Inode
}

func makeRemovable(string) {}
