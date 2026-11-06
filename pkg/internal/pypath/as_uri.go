package pypath

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Path.as_uri of an absolute path, file URI with percent quoting and the drive kept on Windows
func AsURI(p string) (string, error) {
	if runtime.GOOS == "windows" {
		return ntAsURI(p)
	}
	return posixAsURI(p)
}

// POSIX form, file:///x with a double slash root kept as file://x
func posixAsURI(p string) (string, error) {
	if !PosixIsAbs(p) {
		return "", fmt.Errorf("relative paths can't be expressed as file URIs")
	}
	p = PathStr(p)
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "file:" + pyurl.Quote(p, "/"), nil
	}
	return "file://" + pyurl.Quote(p, "/"), nil
}

// Windows form, file:///C:/x for drive paths and file://server/share for UNC paths
func ntAsURI(p string) (string, error) {
	drive, root, _ := NtSplitRoot(p)
	if drive == "" || root == "" {
		return "", fmt.Errorf("relative paths can't be expressed as file URIs")
	}
	posix := strings.ReplaceAll(NtNormpath(p), `\`, "/")
	if len(drive) == 2 && drive[1] == ':' {
		return "file:///" + drive + pyurl.Quote(posix[2:], "/"), nil
	}
	return "file:" + pyurl.Quote(posix, "/"), nil
}
