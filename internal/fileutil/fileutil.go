// Crash-safe file replacement, cross-process file locks, tree operations and archive handling for every icebath package
package fileutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Distinct symlinks one path resolution follows before it fails with ELOOP
const maxLinkHops = 255

// One pending component of a lax resolution, or the marker that closes a symlink being resolved
type resolveStep struct {
	name     string
	doneLink string
}

// Absolute path with every existing symlink followed before any dot-dot after it, a missing remainder kept as written
func resolveLax(path string) (string, error) {
	abs, err := absUncleaned(path)
	if err != nil {
		return "", err
	}
	vol := filepath.VolumeName(abs)
	resolved := vol + string(filepath.Separator)
	var stack []resolveStep
	pushComponents := func(p string) {
		parts := strings.Split(p, string(filepath.Separator))
		for i := len(parts) - 1; i >= 0; i-- {
			stack = append(stack, resolveStep{name: parts[i]})
		}
	}
	pushComponents(abs[len(vol):])
	seen := map[string]string{}
	hops := 0
	for len(stack) > 0 {
		step := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if step.doneLink != "" {
			seen[step.doneLink] = resolved
			continue
		}
		switch step.name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, step.name)
		target, isLink := linkTarget(next)
		if !isLink {
			resolved = next
			continue
		}
		if done, ok := seen[next]; ok {
			if done != "" {
				resolved = done
			} else {
				resolved = next
			}
			continue
		}
		if hops++; hops > maxLinkHops {
			return "", &fs.PathError{Op: "resolve", Path: path, Err: syscall.ELOOP}
		}
		seen[next] = ""
		if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
			tvol := filepath.VolumeName(target)
			if tvol == "" {
				tvol = vol
			}
			resolved = tvol + string(filepath.Separator)
			target = target[len(filepath.VolumeName(target)):]
		}
		stack = append(stack, resolveStep{doneLink: next})
		pushComponents(filepath.FromSlash(target))
	}
	return resolved, nil
}

// Path made absolute against the working directory without collapsing any dot-dot lexically
func absUncleaned(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if vol := filepath.VolumeName(path); vol != "" {
		return filepath.Abs(path)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if path != "" && os.IsPathSeparator(path[0]) {
		return filepath.VolumeName(wd) + path, nil
	}
	return wd + string(filepath.Separator) + path, nil
}

// Path a replace writes through, the real file behind a symlink and the target itself otherwise
func publishPath(target string) (string, error) {
	fi, err := os.Lstat(target)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return target, nil
	}
	return resolveLax(target)
}

// Reports whether path equals root or lies beneath it
func within(path, root string) bool {
	if samePathText(path, root) {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return len(path) > len(prefix) && samePathText(path[:len(prefix)], prefix)
}

// Last element of a file name without its final extension, a leading dot not counting as one
func fileStem(path string) string {
	name := filepath.Base(path)
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}
