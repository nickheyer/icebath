package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Solaris and illumos enumerate descriptors through the procfs path links
const OpenFilesAvailable = true

// Process error psutil wrap_exceptions raises for a procfs failure of pid
func procfsError(err error) error {
	switch {
	case errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH):
		return ErrNoSuchProcess
	case errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES):
		return ErrAccessDenied
	}
	return err
}

// Regular files pid holds open, read from the /proc/pid/path link of every descriptor
func OpenFiles(pid int) ([]OpenFile, error) {
	base := "/proc/" + strconv.Itoa(pid)
	dir, err := os.Open(base + "/fd")
	if err != nil {
		return nil, procfsError(err)
	}
	fds, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nil, procfsError(err)
	}
	out := []OpenFile{}
	hitENOENT := false
	for _, fd := range fds {
		link := filepath.Join(base, "path", fd)
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		file, err := os.Readlink(link)
		if err != nil {
			if errors.Is(err, syscall.ENOENT) {
				hitENOENT = true
				continue
			}
			return nil, procfsError(err)
		}
		regular, err := isFileStrict(file)
		if err != nil {
			return nil, ErrAccessDenied
		}
		if regular {
			n, err := strconv.Atoi(fd)
			if err != nil {
				return nil, err
			}
			out = append(out, OpenFile{Path: file, FD: n})
		}
	}
	if hitENOENT {
		if _, err := os.Stat(base); err != nil {
			return nil, procfsError(err)
		}
	}
	return out, nil
}
