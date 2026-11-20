package pypath

import (
	"errors"
	"fmt"
	"strings"
)

// posixpath.abspath, a relative path joined to the working directory of this process
func PosixAbspath(p string) (string, error) {
	if !PosixIsAbs(p) {
		cwd, err := Getcwd()
		if err != nil {
			return "", err
		}
		p = PosixJoin(cwd, p)
	}
	return PosixNormpath(p), nil
}

// posixpath.relpath, the relative path from start to p
func PosixRelpath(p, start string) (string, error) {
	if p == "" {
		return "", errors.New("no path specified")
	}
	if start == "" {
		start = "."
	}
	startAbs, err := PosixAbspath(start)
	if err != nil {
		return "", err
	}
	pathAbs, err := PosixAbspath(p)
	if err != nil {
		return "", err
	}
	return joinRelative(splitNonEmpty(strings.TrimLeft(startAbs, "/"), "/"), splitNonEmpty(strings.TrimLeft(pathAbs, "/"), "/"), "/", func(a, b string) bool { return a == b }), nil
}

// ntpath.relpath, the relative path from start to p, failing across drives
func NtRelpath(p, start string) (string, error) {
	if p == "" {
		return "", errors.New("no path specified")
	}
	if start == "" {
		start = "."
	}
	startAbs, err := ntAbspath(start)
	if err != nil {
		return "", err
	}
	pathAbs, err := ntAbspath(p)
	if err != nil {
		return "", err
	}
	startDrive, _, startRest := NtSplitRoot(startAbs)
	pathDrive, _, pathRest := NtSplitRoot(pathAbs)
	if NtNormcase(startDrive) != NtNormcase(pathDrive) {
		return "", fmt.Errorf("path is on mount '%s', start on mount '%s'", pathDrive, startDrive)
	}
	return joinRelative(splitNonEmpty(startRest, `\`), splitNonEmpty(pathRest, `\`), `\`, func(a, b string) bool { return NtNormcase(a) == NtNormcase(b) }), nil
}

// os.path.relpath of the host
func Relpath(p, start string) (string, error) {
	if windows {
		return NtRelpath(p, start)
	}
	return PosixRelpath(p, start)
}

// Components of a path tail, none for an empty tail
func splitNonEmpty(tail, sep string) []string {
	if tail == "" {
		return nil
	}
	return strings.Split(tail, sep)
}

// Parent steps out of the unshared start components followed by the unshared path components
func joinRelative(startList, pathList []string, sep string, same func(a, b string) bool) string {
	i := 0
	for i < len(startList) && i < len(pathList) && same(startList[i], pathList[i]) {
		i++
	}
	rel := make([]string, 0, len(startList)-i+len(pathList)-i)
	for range len(startList) - i {
		rel = append(rel, "..")
	}
	rel = append(rel, pathList[i:]...)
	if len(rel) == 0 {
		return "."
	}
	return strings.Join(rel, sep)
}
