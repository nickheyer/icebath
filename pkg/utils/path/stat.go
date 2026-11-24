package pathutil

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// Followed stat of p, a nil info and nil error when IsMissing reads its failure as a missing path or p holds a NUL byte
func StatIfExists(p string) (fs.FileInfo, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return nil, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		if IsMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	return fi, nil
}

// Whether err says a path is not there: not found, below a non-directory, behind a symlink loop or not a valid name
func IsMissing(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && missingErrno(errno)
}
