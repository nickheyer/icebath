//go:build !windows

package proc

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// One regular file a process holds open
type OpenFile struct {
	Path string
	// Descriptor number
	FD int
}

// Whether path is a regular file, a permission failure reported and any other stat failure reading as false
func regularFile(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return false, err
		}
		return false, nil
	}
	return fi.Mode().IsRegular(), nil
}

// Kernel short name widened to the argv zero basename it truncates, the name process lists show
func widenName(pid int, short string) (string, error) {
	if len(short) < 15 {
		return short, nil
	}
	argv, err := Cmdline(pid)
	if err != nil {
		if errors.Is(err, ErrNotFound) && !Exists(pid) {
			return "", err
		}
		return short, nil
	}
	if len(argv) > 0 {
		if base := argv[0][strings.LastIndexByte(argv[0], '/')+1:]; strings.HasPrefix(base, short) {
			return base, nil
		}
	}
	return short, nil
}

// Account owning pid through its real uid, the decimal uid when the account database has no entry
func Owner(pid int) (string, error) {
	uid, err := RealUID(pid)
	if err != nil {
		return "", err
	}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username, nil
	}
	return strconv.Itoa(uid), nil
}
