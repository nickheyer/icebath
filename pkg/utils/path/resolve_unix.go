//go:build !windows

package pathutil

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// One step of the walk: an element still to resolve, or once done is set a symlink whose target has been walked
type walkStep struct {
	name string
	done bool
}

// Elements of p pushed so that the first one is popped first, with how many were pushed
func pushElems(steps []walkStep, p string) ([]walkStep, int) {
	n := 0
	for {
		n++
		i := strings.LastIndexByte(p, '/')
		steps = append(steps, walkStep{name: p[i+1:]})
		if i < 0 {
			return steps, n
		}
		p = p[:i]
	}
}

// The target appended to dir uncleaned, so that each dot-dot in it applies to what the symlinks before it resolve to, as the kernel applies it
func resolveLink(dir, target string) (string, error) {
	if dir != "" && !strings.HasPrefix(target, "/") {
		target = dir + "/" + target
	}
	return resolve(target, false)
}

// Walk of p element by element from the root or the working directory, each symlink replaced by its target and dot-dot applied to what it resolved to
func resolve(p string, strict bool) (string, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: syscall.EINVAL}
	}
	if !strings.HasPrefix(p, "/") {
		// os.Getwd may answer through $PWD with symlinks in it, so the working directory is walked too
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = cwd + "/" + p
	}
	resolved := "/"
	steps, remaining := pushElems(nil, p)
	// Resolved targets of the symlinks met so far, empty while a target is still being walked
	seen := map[string]string{}
	// Trailing elements of resolved kept as written because they could not be looked at, below which nothing can be looked at either
	literal := 0
	for len(steps) > 0 {
		step := steps[len(steps)-1]
		steps = steps[:len(steps)-1]
		if step.done {
			seen[step.name] = resolved
			continue
		}
		remaining--
		switch step.name {
		case "", ".":
			continue
		case "..":
			resolved = resolved[:max(strings.LastIndexByte(resolved, '/'), 1)]
			literal = max(literal-1, 0)
			continue
		}
		next := resolved + "/" + step.name
		if resolved == "/" {
			next = "/" + step.name
		}
		if literal > 0 {
			resolved, literal = next, literal+1
			continue
		}
		fi, err := os.Lstat(next)
		if err != nil {
			if strict {
				return "", err
			}
			resolved, literal = next, 1
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if strict && remaining > 0 && !fi.IsDir() {
				return "", &fs.PathError{Op: "resolve", Path: next, Err: syscall.ENOTDIR}
			}
			resolved = next
			continue
		}
		if target, ok := seen[next]; ok {
			switch {
			case target != "":
				resolved = target
			case strict:
				return "", &fs.PathError{Op: "resolve", Path: next, Err: syscall.ELOOP}
			default:
				resolved, literal = next, 1
			}
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			if strict {
				return "", err
			}
			resolved = next
			continue
		}
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		seen[next] = ""
		var pushed int
		steps, pushed = pushElems(append(steps, walkStep{name: next, done: true}), target)
		remaining += pushed
	}
	return resolved, nil
}
