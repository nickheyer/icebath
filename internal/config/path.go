package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Kind of thing a setting's text names on this computer
type PathKind int

const (
	// A file
	PathFile PathKind = iota + 1
	// A folder
	PathFolder
	// A program: a bare name is looked up on PATH, anything else is the file of the program
	PathProgram
)

// What a setting's text names on this computer, and whether it must be there when the value is saved
type Path struct {
	Kind PathKind
	// The file, folder or program must be there when the value is saved, and be of its kind
	Exists bool
	// The text names several, separated as PATH separates its folders
	Several bool
}

// Words of the kind of a path in a sentence
func (k PathKind) words() string {
	switch k {
	case PathFolder:
		return "folder"
	case PathProgram:
		return "program"
	}
	return "file"
}

// The value a person saved with the path it names written out, or why it names none. One rule for every reader of a path: a leading ~ is
// the home folder, and a path must start with it or with /, so no reader resolves a relative one in its own way; a program may be the bare
// name of one on PATH, which is looked up. The path is kept whole, ~ written out and a program found kept by its full path, so every
// reader opens what was checked. Empty text is the default and a setting that names no path keeps its value; a list names one per item,
// and text that names several separates them as PATH does
func (s Setting) Located(value any) (any, error) {
	if s.Path == nil || value == nil {
		return value, nil
	}
	switch held := value.(type) {
	case string:
		if !s.Path.Several {
			return s.locate(held)
		}
		var out []string
		for _, item := range filepath.SplitList(held) {
			found, err := s.locate(item)
			if err != nil {
				return nil, err
			}
			if found != "" {
				out = append(out, found)
			}
		}
		return strings.Join(out, string(os.PathListSeparator)), nil
	case []string:
		out := make([]string, len(held))
		for i, item := range held {
			found, err := s.locate(item)
			if err != nil {
				return nil, err
			}
			out[i] = found
		}
		return out, nil
	}
	return value, nil
}

// One path written out, or why it can't be kept
func (s Setting) locate(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	what := s.Path.Kind.words()
	if s.Path.Kind == PathProgram && !strings.ContainsAny(text, `/\~`) {
		found, err := exec.LookPath(text)
		if err != nil {
			return "", fmt.Errorf("There is no program named %s on PATH. Write the whole path of the program, such as /usr/local/bin/%s.", text, text)
		}
		text = found
	}
	path := fsx.ExpandHome(text)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("Write the whole path of the %s, starting with / or ~, such as ~/%s.", what, filepath.Base(text))
	}
	path = filepath.Clean(path)
	if !s.Path.Exists {
		return path, nil
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("There is no %s at %s.", what, path)
	case err != nil:
		return "", fmt.Errorf("The %s at %s can't be read. Check that you may open it.", what, path)
	case s.Path.Kind == PathFolder && !info.IsDir():
		return "", fmt.Errorf("%s is a file, and %s takes a folder.", path, s.Title)
	case s.Path.Kind != PathFolder && info.IsDir():
		return "", fmt.Errorf("%s is a folder, and %s takes a %s.", path, s.Title, what)
	case s.Path.Kind == PathProgram && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0:
		return "", fmt.Errorf("%s is not a program you may run.", path)
	}
	return path, nil
}
