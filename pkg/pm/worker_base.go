package pm

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
)

// KindError is a failure carrying the Python exception class callers and the worker wire tell apart
type ClassError struct {
	Class   string
	Message string
}

func (e *ClassError) Error() string { return e.Message }

var classPrefix = regexp.MustCompile(`^(KeyError|TypeError|ValueError|AttributeError|RuntimeError|OSError): `)

// Python class name of a failure as type of exc name spells it
func errorClass(err error) string {
	var conflict *ResolutionConflict
	if errors.As(err, &conflict) {
		return "ResolutionConflict"
	}
	var install *InstallError
	if errors.As(err, &install) {
		return "InstallError"
	}
	var download *DownloadError
	if errors.As(err, &download) {
		switch download.Kind {
		case DownloadWasPaused:
			return "DownloadPaused"
		case DownloadHashFailed:
			return "HashError"
		case DownloadRangeFailed:
			return "_RangeError"
		case DownloadTransportFailed:
			return "DownloadTransportError"
		}
		return "DownloadError"
	}
	var py *ClassError
	if errors.As(err, &py) {
		return py.Class
	}
	var tar *TarReadError
	if errors.As(err, &tar) {
		return tar.Class
	}
	var unknown *UnknownPackageError
	if errors.As(err, &unknown) {
		return "KeyError"
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrExist):
		return "FileExistsError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) {
		return "OSError"
	}
	if m := classPrefix.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return "RuntimeError"
}
