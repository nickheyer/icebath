package pyzip

import (
	"os"
	"strings"
	"syscall"
)

// st_mode Python synthesizes from file attributes, execute bits for program extensions
func statMode(st os.FileInfo) uint32 {
	var attrs uint32
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		attrs = sys.FileAttributes
	}
	var m uint32
	if attrs&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		m |= syscall.S_IFDIR | 0o111
	} else {
		m |= syscall.S_IFREG
	}
	if attrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
		m |= 0o444
	} else {
		m |= 0o666
	}
	if m&syscall.S_IFDIR == 0 {
		name := strings.ToLower(st.Name())
		for _, ext := range []string{".exe", ".bat", ".cmd", ".com"} {
			if strings.HasSuffix(name, ext) {
				m |= 0o111
			}
		}
	}
	return m
}
